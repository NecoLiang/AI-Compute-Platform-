package fleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"tokenfactory/internal/scheduler"
	"tokenfactory/pkg/errcode"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// CR-S1 Minor: 首次接触时一次性密码在主机指纹确认前就发出。供给方可在建节点或填写连接信息时提交
// expected_host_fingerprint: 预先钉死, 引导/探活在握手阶段按它校验, 不一致即中止且不发送任何凭证。

func otherFingerprint(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return ssh.FingerprintSHA256(s.PublicKey())
}

func registerWithFP(t *testing.T, s *Service, name string, portStart int, fp string) (*Node, error) {
	t.Helper()
	v, err := s.RegisterManagedNode(10, scheduler.ManagedNodeReq{ProductID: 1, NodeName: name, TotalCards: 2,
		LeasePortStart: portStart, LeasePortEnd: portStart + 9, ExpectedHostFingerprint: fp})
	if err != nil {
		return nil, err
	}
	return v.(*Node), nil
}

// 预期指纹与实际不一致: 握手阶段中止, 密码一次都没有发出; 节点转 key_mismatch 并记录实际观测指纹。
// 预期指纹可以在建节点时提交, 也可以在填写连接信息时提交。
func TestExpectedFingerprintMismatchNeverSendsPassword(t *testing.T) {
	for _, at := range []string{"create", "ssh_update"} {
		t.Run(at, func(t *testing.T) {
			db := setupFleetDB(t)
			seedManaged(t, db)
			s := newTestFleet(t, db)
			fake := newFakeNode(t, genPW)
			wrong := otherFingerprint(t)

			in := SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root", BootstrapPassword: genPW}
			createFP := ""
			if at == "create" {
				createFP = wrong
			} else {
				in.ExpectedHostFingerprint = wrong
			}
			n, err := registerWithFP(t, s, "gpu-efp-1", 47720, createFP)
			require.NoError(t, err)

			_, err = s.UpdateSSH(context.Background(), 10, n.ID, in, "")
			require.Error(t, err)
			assert.Equal(t, errcode.Conflict, codeOf(err))
			assert.Contains(t, err.Error(), "一次性密码未发送")
			assert.Equal(t, 0, fake.PasswordTries(), "指纹不一致时不得发送密码")
			assert.Equal(t, 0, fake.AuthorizedCount())

			got, err := s.repo.GetNode(n.ID)
			require.NoError(t, err)
			assert.Equal(t, VerifyKeyMismatch, got.VerifyStatus)
			require.NotNil(t, got.HostKeyFingerprint)
			assert.Equal(t, fake.Fingerprint(), *got.HostKeyFingerprint, "记录实际观测指纹, 待供给方核对确认")
			assert.False(t, got.hostKeyConfirmed())
			assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='node.host_key_preconfirmed' AND target_id=?`, n.ID))
			assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='node.key_mismatch' AND target_id=?`, n.ID))
			assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='node.bootstrap_password_used' AND target_id=?`, n.ID))
		})
	}
}

// 预期指纹一致: 一次性密码引导正常安装公钥, 指纹已确认, 首次完整探活直接通过(无需 TOFU 确认这一步)。
// 预置指纹在首次填写连接信息时保留; 之后改 host/port 视为换机器, 照常清除。
func TestExpectedFingerprintMatchSkipsTOFU(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, genPW)

	n, err := registerWithFP(t, s, "gpu-efp-2", 47740, "  "+fake.Fingerprint()+" ")
	require.NoError(t, err)
	assert.True(t, n.hostKeyConfirmed(), "建节点时提交的预期指纹直接钉死")
	assert.Equal(t, VerifyUnverified, n.VerifyStatus)

	res, err := s.UpdateSSH(context.Background(), 10, n.ID, SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root",
		BootstrapPassword: genPW}, "")
	require.NoError(t, err)
	assert.Equal(t, "installed", res.Bootstrap)
	assert.True(t, res.HostKey.Confirmed, "首次填写连接信息保留预置指纹")
	assert.Equal(t, 1, fake.PasswordTries())

	taskID, err := s.Verify(10, n.ID)
	require.NoError(t, err)
	done := runTasks(t, s)
	require.Len(t, done, 1)
	assert.Equal(t, taskID, done[0].ID)
	o := outcomeOf(t, done[0])
	assert.Empty(t, o.AwaitingHostKey)
	require.True(t, o.Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, n.ID))

	// 改端口 = 换机器: 预置指纹清除, 回到 TOFU。
	res, err = s.UpdateSSH(context.Background(), 10, n.ID, SSHInput{Host: "127.0.0.1", Port: fake.Port() + 1, User: "root"}, "")
	require.NoError(t, err)
	assert.False(t, res.HostKey.Confirmed)
	assert.Nil(t, res.HostKey.Fingerprint)
}

// 推荐路径: 只手动写入公钥行(不提供密码) + 预期指纹, 首次探活直接通过; 平台从不接触密码。
func TestPublicKeyOnlyWithExpectedFingerprint(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, "")

	n, err := registerWithFP(t, s, "gpu-efp-3", 47760, "")
	require.NoError(t, err)
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(*n.MgmtPubkey))
	require.NoError(t, err)
	fake.Authorize(k) // 供给方按上架说明手动加入 authorized_keys

	res, err := s.UpdateSSH(context.Background(), 10, n.ID, SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root",
		ExpectedHostFingerprint: fake.Fingerprint()}, "")
	require.NoError(t, err)
	assert.Equal(t, "skipped", res.Bootstrap)
	assert.True(t, res.HostKey.Confirmed)

	_, err = s.Verify(10, n.ID)
	require.NoError(t, err)
	done := runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, n.ID))
	assert.Equal(t, 0, fake.PasswordTries())
}

// 格式非法 → 40001, 不建节点 / 不改连接信息; 未提交该字段时原 TOFU 流程不变。
func TestExpectedFingerprintValidationAndDefaultFlow(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	fake := newFakeNode(t, genPW)

	for _, bad := range []string{"abc", "MD5:aa:bb", "SHA256:short", "256 " + fake.Fingerprint() + " root@h (ED25519)",
		fake.Fingerprint() + "="} {
		_, err := registerWithFP(t, s, "gpu-efp-bad", 47780, bad)
		assert.Equal(t, errcode.ParamInvalid, codeOf(err), bad)
	}
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM supplier_nodes WHERE node_name='gpu-efp-bad'`))

	n, err := registerWithFP(t, s, "gpu-efp-4", 47800, "")
	require.NoError(t, err)
	assert.Nil(t, n.HostKeyFingerprint)
	_, err = s.UpdateSSH(context.Background(), 10, n.ID, SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root",
		BootstrapPassword: genPW, ExpectedHostFingerprint: "SHA256:nope"}, "")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	assert.Equal(t, 0, fake.PasswordTries())
	got, err := s.repo.GetNode(n.ID)
	require.NoError(t, err)
	assert.Nil(t, got.SSHHost, "校验失败不保存连接信息")

	// 不带该字段: 原流程(引导后记录观测指纹, 待供给方确认)。
	res, err := s.UpdateSSH(context.Background(), 10, n.ID, SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root",
		BootstrapPassword: genPW}, "")
	require.NoError(t, err)
	assert.Equal(t, "installed", res.Bootstrap)
	assert.False(t, res.HostKey.Confirmed)
	require.NotNil(t, res.HostKey.Fingerprint)
	assert.Equal(t, fake.Fingerprint(), *res.HostKey.Fingerprint)
}

// HTTP 层: POST /supplier/nodes 与 PUT /supplier/nodes/:id/ssh 接受 expected_host_fingerprint。
func TestExpectedFingerprintHTTP(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	sched := scheduler.NewService(scheduler.NewRepository(db))
	sched.SetManagedNodes(s)
	fake := newFakeNode(t, genPW)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", int64(10)) })
	g := r.Group("/api/v1")
	scheduler.NewHandler(sched).RegisterSupplierRoutes(g)
	NewHandler(s).RegisterSupplierRoutes(g)
	call := func(method, path string, body any) (int, json.RawMessage) {
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		var env struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
		return env.Code, env.Data
	}

	code, data := call(http.MethodPost, "/api/v1/supplier/nodes", map[string]any{"product_id": 1, "node_name": "gpu-efp-http",
		"total_cards": 2, "mgmt_mode": "ssh", "lease_port_start": 47820, "lease_port_end": 47829,
		"expected_host_fingerprint": fake.Fingerprint()})
	require.Equal(t, 0, code, string(data))
	var created struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	require.NoError(t, json.Unmarshal(data, &created))
	got, err := s.repo.GetNode(created.Node.ID)
	require.NoError(t, err)
	assert.True(t, got.hostKeyConfirmed())

	code, _ = call(http.MethodPost, "/api/v1/supplier/nodes", map[string]any{"product_id": 1, "node_name": "gpu-efp-http2",
		"total_cards": 2, "mgmt_mode": "ssh", "lease_port_start": 47830, "lease_port_end": 47839,
		"expected_host_fingerprint": "bogus"})
	assert.Equal(t, errcode.ParamInvalid, code)

	code, _ = call(http.MethodPut, fmt.Sprintf("/api/v1/supplier/nodes/%d/ssh", created.Node.ID), map[string]any{
		"host": "127.0.0.1", "port": fake.Port(), "user": "root", "bootstrap_password": genPW,
		"expected_host_fingerprint": otherFingerprint(t)})
	assert.Equal(t, errcode.Conflict, code)
	assert.Equal(t, 0, fake.PasswordTries())
}
