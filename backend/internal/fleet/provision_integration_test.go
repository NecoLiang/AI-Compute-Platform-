package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"tokenfactory/internal/compute"
	tfcrypto "tokenfactory/pkg/crypto"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// T-084 开通流水线集成测试: 真实 MySQL + fakeNode(真实 SSH 握手/公钥认证) + 网关自检桩。

const provPortStart = 47200

type provEnv struct {
	db    *sqlx.DB
	s     *Service
	comp  *compute.Service
	fake  *fakeNode
	gw    *stubGateway
	node  int64
	order *compute.Order
	lease *Lease
}

// fakeContainerHostKey 容器 sshd 的 ed25519 主机公钥行与指纹(测试复用一把随机公钥)。
func fakeContainerHostKey(t *testing.T) (string, string) {
	t.Helper()
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(testUpstreamPub))
	require.NoError(t, err)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " root@lease", ssh.FingerprintSHA256(k)
}

func gpuListL(uuids ...string) string {
	var b strings.Builder
	for i, u := range uuids {
		b.WriteString("GPU " + strconv.Itoa(i) + ": NVIDIA H100 80GB HBM3 (UUID: " + u + ")\n")
	}
	return b.String()
}

// newProvEnv 1 台 2 卡节点(接到 fakeNode), 买家下 2 卡单并支付 → 一个待执行的 provision 任务。
func newProvEnv(t *testing.T, withGateway bool) *provEnv {
	t.Helper()
	return newProvEnvKind(t, "container", withGateway)
}

// newProvEnvKind 同 newProvEnv, 商品交付方式可选(container / bare_metal)。
func newProvEnvKind(t *testing.T, kind string, withGateway bool) *provEnv {
	t.Helper()
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	nodes := seedLeaseMarket(t, db, s, kind, 1, 2, 1)
	e := &provEnv{db: db, s: s, node: nodes[0], fake: newFakeNode(t, "")}

	pub, enc, err := generateNodeKey(e.node, s.credKey)
	require.NoError(t, err)
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	require.NoError(t, err)
	e.fake.Authorize(k)
	db.MustExec(`UPDATE supplier_nodes SET mgmt_pubkey=?, mgmt_privkey_encrypted=?, ssh_host='127.0.0.1', ssh_port=?,
		host_key_fingerprint=?, host_key_confirmed_at=NOW(), disk_per_card_gb=500, lease_port_start=?, lease_port_end=?
		WHERE id=?`, pub, enc, e.fake.Port(), e.fake.Fingerprint(), provPortStart, provPortStart+9, e.node)
	db.MustExec(`UPDATE gpu_slots SET gpu_uuid=? WHERE node_id=? AND gpu_index=0`, fakeUUID0, e.node)
	db.MustExec(`UPDATE gpu_slots SET gpu_uuid=? WHERE node_id=? AND gpu_index=1`, fakeUUID1, e.node)

	line, _ := fakeContainerHostKey(t)
	e.fake.Override("docker run --rm --network host --cap-drop ALL", fakeReply{Stdout: "@@applied iptables-nft\n"})
	e.fake.Override("if docker image inspect --format '{{.Id}}' "+testProbeImage, fakeReply{Stdout: "@@have\n"})
	e.fake.Override("if docker image inspect --format '{{.Id}}' "+testBaseImage, fakeReply{Stdout: "@@have\n"})
	e.fake.Override("set -e; (docker network inspect omnis-l-", fakeReply{Stdout: "@@created\n"})
	e.fake.Override("for i in $(seq 1 30); do if", fakeReply{Stdout: line + "\n"})

	if withGateway {
		e.gw = &stubGateway{leaseOut: gpuListL(fakeUUID0, fakeUUID1)}
		s.SetGateway(e.gw)
	}
	e.comp = wireLeases(t, s)
	e.order, err = placeManaged(e.comp, 21, 2)
	require.NoError(t, err)
	payOrder(t, db, e.comp, e.order.OrderNo)
	e.lease, err = s.repo.getLeaseByOrder(db, e.order.ID)
	require.NoError(t, err)
	require.NotNil(t, e.lease)
	require.Equal(t, e.node, e.lease.NodeID)
	return e
}

func (e *provEnv) outcome(t *testing.T, task *Task) ProvisionOutcome {
	t.Helper()
	require.NotNil(t, task)
	require.Equal(t, TaskSucceeded, task.State, "last_error=%v", task.LastError)
	var o ProvisionOutcome
	require.NoError(t, json.Unmarshal(task.Result, &o))
	return o
}

func (e *provEnv) cmdsWith(prefix string) []string {
	var out []string
	for _, c := range e.fake.Commands() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestProvisionHappyPath(t *testing.T) {
	e := newProvEnv(t, true)
	_, fp := fakeContainerHostKey(t)

	o := e.outcome(t, runOne(t, e.s))
	assert.Equal(t, e.lease.LeaseNo, o.LeaseNo)
	assert.Equal(t, provPortStart, o.UpstreamPort)
	assert.Equal(t, fp, o.UpstreamHostKeyFP)
	assert.Equal(t, []string{"applied iptables-nft"}, o.Firewall)
	assert.False(t, o.ImagePulled)
	assert.False(t, o.Abandoned)
	assert.Equal(t, []string{e.lease.LeaseNo}, e.gw.leases)

	l, err := e.s.repo.getLease(e.db, e.lease.ID)
	require.NoError(t, err)
	assert.Equal(t, LeaseActive, l.State)
	require.NotNil(t, l.UpstreamPort)
	assert.Equal(t, provPortStart, *l.UpstreamPort)
	require.NotNil(t, l.UpstreamHostKeyFP)
	assert.Equal(t, fp, *l.UpstreamHostKeyFP)
	require.NotNil(t, l.ContainerName)
	assert.Equal(t, containerNameOf(l.LeaseNo), *l.ContainerName)

	var ord struct {
		Status string `db:"status"`
		Match  int    `db:"m"`
	}
	require.NoError(t, e.db.Get(&ord, `SELECT o.status, (l.access_until = o.lease_end_at) m FROM orders o
		JOIN leases l ON l.order_id=o.id WHERE o.id=?`, e.order.ID))
	assert.Equal(t, "active", ord.Status)
	assert.Equal(t, 1, ord.Match, "租约 access_until 必须等于订单 lease_end_at")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM order_deliveries WHERE order_id=? AND access_status='delivered'
		AND selftest_passed_at IS NOT NULL`, e.order.ID))
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title='算力已开通'
		AND link=?`, "/console/buyer/orders/"+e.order.OrderNo))

	// 交付记录里给买家的是网关入口与登录名, 不含节点地址。
	d, err := e.comp.GetDelivery(e.order.ID)
	require.NoError(t, err)
	require.NotNil(t, d)
	info, err := tfcrypto.Decrypt(d.CredentialEncrypted, bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	assert.Contains(t, info, `"gw.test"`)
	assert.Contains(t, info, e.lease.LeaseNo)
	assert.NotContains(t, info, "127.0.0.1")
	assert.NotContains(t, info, strconv.Itoa(provPortStart))

	created := e.cmdsWith("set -e; (docker network inspect omnis-l-")
	require.Len(t, created, 1)
	for _, frag := range []string{"--cpus 126 --memory 1016g --memory-swap 1016g", "--shm-size 64g", "--storage-opt size=1000G",
		"-p " + strconv.Itoa(provPortStart) + ":22", `--gpus '"device=` + fakeUUID0 + "," + fakeUUID1 + `"'`,
		"--name " + containerNameOf(l.LeaseNo) + " "} {
		assert.Contains(t, created[0], frag)
	}
	fw := e.cmdsWith("docker run --rm --network host --cap-drop ALL")
	require.Len(t, fw, 1)
	script := decodeB64(t, fw[0])
	assert.Contains(t, script, "--ctorigdstport 47200:47209")
	assert.Contains(t, script, "IPS='10.0.0.2'")
}

func TestProvisionRetryIsIdempotent(t *testing.T) {
	e := newProvEnv(t, true)
	// 首次: 基础镜像不在本地 → 拉取; 建容器失败 → 可重试。
	e.fake.Override("if docker image inspect --format '{{.Id}}' "+testBaseImage, fakeReply{Stdout: "@@missing\n"})
	e.fake.Override("set -e; (docker network inspect omnis-l-", fakeReply{Stderr: "docker: Error response from daemon: no space left", Code: 125})
	got := runOne(t, e.s)
	require.NotNil(t, got)
	assert.Equal(t, TaskPending, got.State)
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, "创建租约容器失败")
	assert.Len(t, e.cmdsWith("docker pull -q "+testBaseImage), 1)
	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeaseProvisioning, l.State)
	require.NotNil(t, l.PortClaim)
	assert.Equal(t, provPortStart, *l.PortClaim)
	var st string
	require.NoError(t, e.db.Get(&st, `SELECT status FROM orders WHERE id=?`, e.order.ID))
	assert.Equal(t, "provisioning", st)

	// 重试: 47200 此时已被上次的半成品容器监听, 必须沿用已占用端口而不是重新选。
	e.fake.Override("if docker image inspect --format '{{.Id}}' "+testBaseImage, fakeReply{Stdout: "@@have\n"})
	e.fake.Override("set -e; (docker network inspect omnis-l-", fakeReply{Stdout: "@@created\n"})
	e.fake.Override(cmdPorts, fakeReply{Stdout: "LISTEN 0 4096 0.0.0.0:47200 0.0.0.0:*\n"})
	e.db.MustExec(`UPDATE node_tasks SET run_after=NOW() WHERE id=?`, got.ID)
	o := e.outcome(t, runOne(t, e.s))
	assert.Equal(t, provPortStart, o.UpstreamPort)
	assert.False(t, o.ImagePulled)
	assert.Len(t, e.cmdsWith("set -e; (docker network inspect omnis-l-"), 2)
	assert.Len(t, e.cmdsWith(cmdPorts), 1, "已占用端口后不再重新选端口")
	l, _ = e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeaseActive, l.State)
}

func TestProvisionWithoutGatewayNeverDelivers(t *testing.T) {
	e := newProvEnv(t, false)
	got := runOne(t, e.s)
	require.NotNil(t, got)
	assert.Equal(t, TaskPending, got.State, "网关未接入属可重试错误, 最终走换节点/退款")
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, "访问网关(cmd/accessgw)未接入")
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM order_deliveries WHERE order_id=?`, e.order.ID))
	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeaseProvisioning, l.State)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='reserved'`, e.lease.ID))
}

func TestProvisionSelftestFailures(t *testing.T) {
	t.Run("GPU 集合不符", func(t *testing.T) {
		e := newProvEnv(t, true)
		e.gw.leaseOut = gpuListL(fakeUUID0)
		got := runOne(t, e.s)
		require.NotNil(t, got)
		assert.Equal(t, TaskPending, got.State)
		require.NotNil(t, got.LastError)
		assert.Contains(t, *got.LastError, "与分配的卡")
		assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM order_deliveries WHERE order_id=?`, e.order.ID))
	})
	t.Run("网关登录失败", func(t *testing.T) {
		e := newProvEnv(t, true)
		e.gw.leaseErr = errors.New("ssh: handshake failed: host key mismatch")
		got := runOne(t, e.s)
		require.NotNil(t, got)
		assert.Equal(t, TaskPending, got.State)
		assert.Contains(t, *got.LastError, "经网关自检登录失败")
	})
	t.Run("主服务可直连租约端口", func(t *testing.T) {
		e := newProvEnv(t, true)
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(provPortStart))
		if err != nil {
			t.Skipf("端口 %d 被本机占用: %v", provPortStart, err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		got := runOne(t, e.s)
		require.NotNil(t, got)
		assert.Equal(t, TaskPending, got.State)
		assert.Contains(t, *got.LastError, "租约端口未限制来源")
		var st string
		require.NoError(t, e.db.Get(&st, `SELECT status FROM orders WHERE id=?`, e.order.ID))
		assert.Equal(t, "provisioning", st)
	})
	t.Run("节点没有 DOCKER-USER 链", func(t *testing.T) {
		e := newProvEnv(t, true)
		e.fake.Override("docker run --rm --network host --cap-drop ALL", fakeReply{Stdout: "@@nochain\n", Code: 3})
		got := runOne(t, e.s)
		require.NotNil(t, got)
		assert.Equal(t, TaskPending, got.State)
		assert.Contains(t, *got.LastError, "DOCKER-USER")
		assert.Empty(t, e.cmdsWith("set -e; (docker network inspect omnis-l-"), "规则未就位不得建容器")
	})
}

func TestProvisionCancelledBeforeRun(t *testing.T) {
	e := newProvEnv(t, true)
	require.NoError(t, e.comp.CancelOrder(e.order.OrderNo))
	assert.Nil(t, runOne(t, e.s), "取消时待执行的开通任务已被置为 failed")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='provision' AND state='failed'`, e.lease.ID))
	assert.Empty(t, e.fake.Commands())
}

func TestProvisionAbandonedWhenFrozenDuringSelftest(t *testing.T) {
	e := newProvEnv(t, true)
	e.gw.onLease = func() { e.db.MustExec(`UPDATE orders SET status='frozen' WHERE id=?`, e.order.ID) }
	o := e.outcome(t, runOne(t, e.s))
	assert.True(t, o.Abandoned)
	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeaseProvisioning, l.State, "未激活, 容器残留由后续 purge/解冻重开处理")
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM order_deliveries WHERE order_id=?`, e.order.ID))
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='reserved'`, e.lease.ID))
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title='算力已开通'`))

	// 冻结后再执行: 订单不在 paid/provisioning → 放弃(failed), 不再动节点。
	_, err := e.s.repo.insertLeaseTask(e.db, e.node, e.lease.ID, KindProvision, prioProvision, LeaseProvisionPayload{LeaseID: e.lease.ID})
	require.NoError(t, err)
	n := len(e.fake.Commands())
	got := runOne(t, e.s)
	require.NotNil(t, got)
	assert.Equal(t, TaskFailed, got.State)
	assert.Contains(t, *got.LastError, "frozen")
	assert.Len(t, e.fake.Commands(), n)
}

// 在租卡轻探活异常 → leases.resource_alert(T-105 停机认定的输入); 恢复后清空。
func TestLightProbeSetsLeaseResourceAlert(t *testing.T) {
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s))
	n, err := e.s.repo.GetNode(e.node)
	require.NoError(t, err)

	e.fake.Override(cmdTelemetry, fakeReply{Stdout: fakeTelLine(fakeUUID0, 0) + fakeTelLine(fakeUUID1, 2)})
	e.s.lightProbe(context.Background(), n)
	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	require.NotNil(t, l.ResourceAlert)
	assert.Contains(t, *l.ResourceAlert, "GPU1 不可纠正 ECC 错误 2 次")
	assert.Equal(t, LeaseActive, l.State, "停机认定属 T-105, 这里只记录")
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, e.lease.ID))

	e.fake.Override(cmdTelemetry, fakeReply{Stdout: fakeTelLine(fakeUUID0, 0) + fakeTelLine(fakeUUID1, 0)})
	e.s.lightProbe(context.Background(), n)
	l, _ = e.s.repo.getLease(e.db, e.lease.ID)
	assert.True(t, l.ResourceAlert == nil || *l.ResourceAlert == "")
}
