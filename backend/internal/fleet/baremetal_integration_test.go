package fleet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tokenfactory/pkg/accessgw"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-089 整机托管集成测试: 真实 MySQL + fakeNode(真实 SSH) + 有状态的宿主机账户模拟(fakeHost)。
// fakeHost 只在测试中按平台下发脚本的语义维护账户状态, 脚本本身的 shell 语义另由 ubuntu 容器手工验证过。

const hostRootPrefix = "docker run --rm --privileged --pid host --network none --label omnis.probe=1 -v /:/host --entrypoint chroot "

type fakeAcct struct {
	authorized string
	sudo       bool
	sessions   int // sshd 会话进程
	procs      int // 其他进程
}

type fakeHost struct {
	mu      sync.Mutex
	accts   map[string]*fakeAcct
	scripts []string
	fail    *fakeReply // 非空时下一条脚本返回它
}

func installFakeHost(t *testing.T, f *fakeNode) *fakeHost {
	t.Helper()
	h := &fakeHost{accts: map[string]*fakeAcct{}}
	f.Handle(hostRootPrefix, func(cmd string) fakeReply {
		m := b64Re.FindStringSubmatch(cmd)
		if m == nil {
			return fakeReply{Stderr: "no script", Code: 2}
		}
		raw, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			return fakeReply{Stderr: err.Error(), Code: 2}
		}
		return h.run(string(raw))
	})
	return h
}

func (h *fakeHost) run(script string) fakeReply {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scripts = append(h.scripts, script)
	if h.fail != nil {
		r := *h.fail
		h.fail = nil
		return r
	}
	var u string
	for _, l := range strings.Split(script, "\n") {
		if v, ok := strings.CutPrefix(l, "U="); ok {
			u = v
			break
		}
	}
	a := h.accts[u]
	switch {
	case strings.Contains(script, "useradd -m"):
		const marker = `printf '%s\n' '`
		i := strings.Index(script, marker)
		j := strings.Index(script[i+len(marker):], "'")
		if a == nil {
			a = &fakeAcct{}
			h.accts[u] = a
		}
		a.authorized, a.sudo = script[i+len(marker):i+len(marker)+j], true
		return fakeReply{Stdout: "@@account " + u + "\n"}
	case a == nil:
		return fakeReply{Stdout: "@@absent\n"}
	case strings.Contains(script, "-x 'sshd|sshd-session'"):
		n := a.sessions
		a.sessions = 0
		return fakeReply{Stdout: "@@killed " + strconv.Itoa(n) + "\n"}
	case strings.Contains(script, "userdel -r"):
		n := a.sessions + a.procs
		delete(h.accts, u)
		return fakeReply{Stdout: "@@removed " + strconv.Itoa(n) + "\n"}
	case strings.Contains(script, "@@locked"):
		n := a.sessions + a.procs
		a.authorized, a.sudo, a.sessions, a.procs = "", false, 0, 0
		return fakeReply{Stdout: "@@locked " + strconv.Itoa(n) + "\n"}
	}
	return fakeReply{Stderr: "unknown script", Code: 2}
}

func (h *fakeHost) acct(u string) *fakeAcct {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a := h.accts[u]; a != nil {
		c := *a
		return &c
	}
	return nil
}

func (h *fakeHost) login(u string, sessions, procs int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.accts[u].sessions += sessions
	h.accts[u].procs += procs
}

type bmEnv struct {
	*provEnv
	host *fakeHost
	user string
}

// newBMEnv 1 台 2 卡整机节点, 买家下整机单并支付; active=true 时跑完开通。
func newBMEnv(t *testing.T, active bool) *bmEnv {
	t.Helper()
	e := &bmEnv{provEnv: newProvEnvKind(t, "bare_metal", true)}
	e.s.cfg.Grace.Short, e.s.cfg.Grace.Long, e.s.cfg.Grace.Nightly = 24*time.Hour, 72*time.Hour, 24*time.Hour
	e.host = installFakeHost(t, e.fake)
	e.user = "omnis-" + e.lease.LeaseNo
	require.Equal(t, "bare_metal", e.lease.DeliveryKind)
	if active {
		e.outcome(t, runOne(t, e.s))
		e.lease = e.leaseNow(t)
		require.Equal(t, LeaseActive, e.lease.State)
	}
	return e
}

func (e *bmEnv) node(t *testing.T) *Node {
	t.Helper()
	n, err := e.s.repo.GetNode(e.provEnv.node)
	require.NoError(t, err)
	return n
}

func (e *bmEnv) hostRootCmds() []string { return e.cmdsWith(hostRootPrefix) }

func TestBareMetalProvisionAndGatewayRoute(t *testing.T) {
	e := newBMEnv(t, false)
	pinned := e.fake.Fingerprint()

	o := e.outcome(t, runOne(t, e.s))
	assert.Equal(t, e.fake.Port(), o.UpstreamPort, "上游 = 宿主机管理 sshd")
	assert.Equal(t, pinned, o.UpstreamHostKeyFP, "上游指纹 = 节点钉死指纹")
	assert.Empty(t, o.Firewall)

	l := e.leaseNow(t)
	assert.Equal(t, LeaseActive, l.State)
	require.NotNil(t, l.UpstreamPort)
	assert.Equal(t, e.fake.Port(), *l.UpstreamPort)
	assert.Nil(t, l.PortClaim, "整机不占租约端口段")
	assert.Nil(t, l.ContainerName)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, e.lease.ID))
	assert.Equal(t, "active", orderStatus(t, e.provEnv))

	a := e.host.acct(e.user)
	require.NotNil(t, a, "宿主机已建租约账户")
	assert.True(t, a.sudo)
	assert.True(t, strings.HasPrefix(a.authorized, `from="10.0.0.2" ssh-ed25519 `), a.authorized)
	// 不做防火墙/容器/基础镜像步骤; 宿主机 root 操作只经探针镜像特权容器。
	assert.Empty(t, e.cmdsWith("docker run --rm --network host --cap-drop ALL"))
	assert.Empty(t, e.cmdsWith("set -e; (docker network inspect omnis-l-"))
	require.Len(t, e.hostRootCmds(), 1)
	assert.True(t, strings.HasPrefix(e.hostRootCmds()[0], hostRootPrefix+testProbeImage+" /host /bin/sh -c '"))

	// 网关: 买家登录整机租约, 上游账户为 omnis-<lease_no>。
	rdb := testRedis(t)
	sys := newSigner(t)
	api, err := NewGatewayAPI(e.s, rdb, gwTestSecret, fpOf(sys))
	require.NoError(t, err)
	buyer := newSigner(t)
	e.db.MustExec(`INSERT INTO buyer_ssh_keys (user_id, name, public_key, fingerprint) VALUES (21,'k','x',?)`, fpOf(buyer))
	r, err := api.Authenticate(context.Background(), pkReq(e.lease.LeaseNo, "198.51.100.1", buyer))
	require.NoError(t, err)
	require.True(t, r.Allow)
	assert.Equal(t, e.user, r.UpstreamUser)
	assert.Equal(t, e.fake.Port(), r.UpstreamPort)
	assert.Equal(t, pinned, r.UpstreamHostKeyFP)
	assert.True(t, accessgw.ValidUpstreamUser(r.UpstreamUser))

	// 开通期间系统自检身份同样路由到租约账户。
	e.db.MustExec(`UPDATE leases SET state='provisioning' WHERE id=?`, e.lease.ID)
	r, err = api.Authenticate(context.Background(), pkReq(e.lease.LeaseNo, "10.0.0.1", sys))
	require.NoError(t, err)
	require.True(t, r.Allow)
	assert.True(t, r.System)
	assert.Equal(t, e.user, r.UpstreamUser)
}

func TestBareMetalProvisionHostFailureRetries(t *testing.T) {
	e := newBMEnv(t, false)
	e.host.fail = &fakeReply{Stdout: "@@missing visudo\n", Code: 3}
	done := runOne(t, e.s)
	require.NotNil(t, done)
	assert.NotEqual(t, TaskSucceeded, done.State)
	require.NotNil(t, done.LastError)
	assert.Contains(t, *done.LastError, "宿主机缺少 visudo")
	assert.Equal(t, LeaseProvisioning, e.leaseNow(t).State)
	assert.Nil(t, e.host.acct(e.user))
}

// 到期: 无宽限, 直接 purging + 节点 needs_reimage(闸门 + 旧指纹快照); purge 删账户、写回执、释放卡、提醒供给方。
func TestBareMetalExpiryPurgesAndRequiresReimage(t *testing.T) {
	e := newBMEnv(t, true)
	oldFP := e.fake.Fingerprint()
	e.host.login(e.user, 1, 2)
	genBefore := e.node(t).Generation

	expireLease(t, e.provEnv)
	e.s.ExpiryRound(context.Background())
	assert.Equal(t, "completed", orderStatus(t, e.provEnv))
	l := e.leaseNow(t)
	require.Equal(t, LeasePurging, l.State, "整机无宽限")
	assert.Nil(t, l.GraceUntil)
	n := e.node(t)
	assert.Equal(t, VerifyNeedsReimage, n.VerifyStatus)
	require.NotNil(t, n.ReimageRequiredAt)
	require.NotNil(t, n.ReimagePrevFP)
	assert.Equal(t, oldFP, *n.ReimagePrevFP)
	assert.Greater(t, n.Generation, genBefore)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop'`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM audit_logs WHERE action='node.needs_reimage' AND target_id=?`, n.ID))

	done := runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, TaskSucceeded, done.State, "last_error=%v", done.LastError)
	assert.Equal(t, LeasePurged, e.leaseNow(t).State)
	assert.Nil(t, e.host.acct(e.user), "账户与家目录已删除")
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=? AND state='free'`, n.ID))
	stock, _ := leaseStock(t, e.db)
	assert.Equal(t, 0, stock, "待重装节点不计库存")

	var raw string
	require.NoError(t, e.db.Get(&raw, `SELECT after_value FROM audit_logs WHERE action='lease.purged' AND target_id=?`, e.lease.ID))
	var rc PurgeReceipt
	require.NoError(t, json.Unmarshal([]byte(raw), &rc))
	assert.Equal(t, e.user, rc.Account)
	assert.Empty(t, rc.Container)
	assert.Equal(t, []PurgeStep{{Step: "account", Target: e.user, Outcome: "removed"}}, rc.Steps)
	assert.Contains(t, rc.Erasure, "needs_reimage")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=20 AND title='整机节点待重装'`))

	// 闸门: 任何绕过重装确认回到可售的路径都不通。
	assert.Equal(t, VerifyNeedsReimage, e.node(t).VerifyStatus, "删账户后仍待重装")
	_, err := e.s.Verify(20, n.ID)
	assert.Equal(t, errcode.Conflict, codeOf(err))
	_, err = e.s.ConfirmHostKey(20, n.ID, oldFP, "")
	assert.Equal(t, errcode.Conflict, codeOf(err))
	_, err = e.s.UpdateSSH(context.Background(), 20, n.ID, SSHInput{Host: "127.0.0.1", Port: e.fake.Port(), User: "root", ExpectedHostFingerprint: oldFP}, "")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err), "旧指纹不能当新系统指纹")
	// 库层兜底: 即便状态被(异常路径)改成 verifying, 闸门在时 markVerified 也不写回。
	e.db.MustExec(`UPDATE supplier_nodes SET verify_status='verifying' WHERE id=?`, n.ID)
	cur := e.node(t)
	require.NoError(t, e.s.inTx(func(tx *sqlx.Tx) error {
		ok, err := e.s.repo.markVerified(tx, n.ID, guardOf(cur), cur.Generation, oldFP, &Inventory{})
		assert.False(t, ok, "markVerified 库层兜底")
		return err
	}))
	e.db.MustExec(`UPDATE supplier_nodes SET verify_status='needs_reimage' WHERE id=?`, n.ID)

	// 重装确认: 旧指纹 40001; 新指纹 → verifying, 闸门清除, 新指纹钉死。
	_, err = e.s.ReimageConfirm(20, n.ID, oldFP, "")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	newFP := e.fake.RotateHostKey()
	taskID, err := e.s.ReimageConfirm(20, n.ID, newFP, "")
	require.NoError(t, err)
	assert.NotZero(t, taskID)
	n2 := e.node(t)
	assert.Equal(t, VerifyVerifying, n2.VerifyStatus)
	assert.Nil(t, n2.ReimageRequiredAt)
	require.NotNil(t, n2.HostKeyFingerprint)
	assert.Equal(t, newFP, *n2.HostKeyFingerprint)
	assert.NotNil(t, n2.HostKeyConfirmedAt)
	assert.Greater(t, n2.Generation, n.Generation)
}

// 冻结: 只断 sshd 会话; 冻结中到期: 撤权限 + 结束进程, 账户保留作现场, 节点待重装; 有冻结租约时不能重装确认。
func TestBareMetalFreezeAndSuspendedExpiry(t *testing.T) {
	e := newBMEnv(t, true)
	e.host.login(e.user, 2, 3)
	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	require.Equal(t, LeaseSuspended, e.leaseNow(t).State)

	done := runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, KindKillSessions, done.Kind)
	require.Equal(t, TaskSucceeded, done.State, "last_error=%v", done.LastError)
	var ko KillSessionsOutcome
	require.NoError(t, json.Unmarshal(done.Result, &ko))
	assert.Equal(t, KillSessionsOutcome{LeaseNo: e.lease.LeaseNo, Killed: 2, Running: true}, ko)
	a := e.host.acct(e.user)
	require.NotNil(t, a)
	assert.Equal(t, 0, a.sessions)
	assert.Equal(t, 3, a.procs, "冻结不结束买家其他进程")
	assert.True(t, a.sudo)

	expireLease(t, e.provEnv)
	e.s.ExpiryRound(context.Background())
	assert.Equal(t, LeaseSuspended, e.leaseNow(t).State)
	assert.Equal(t, VerifyNeedsReimage, e.node(t).VerifyStatus)

	done = runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, KindStop, done.Kind)
	require.Equal(t, TaskSucceeded, done.State, "last_error=%v", done.LastError)
	var so StopOutcome
	require.NoError(t, json.Unmarshal(done.Result, &so))
	assert.Equal(t, "locked", so.Account)
	a = e.host.acct(e.user)
	require.NotNil(t, a, "账户与家目录保留")
	assert.False(t, a.sudo)
	assert.Empty(t, a.authorized)
	assert.Zero(t, a.procs)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=?`, e.lease.ID))

	newFP := e.fake.RotateHostKey()
	_, err := e.s.ReimageConfirm(20, e.provEnv.node, newFP, "")
	assert.Equal(t, errcode.Conflict, codeOf(err), "冻结核查中不能重装确认")
	assert.Equal(t, VerifyNeedsReimage, e.node(t).VerifyStatus)
}

// 供给方先重装、清除任务连到新系统(指纹不符): 节点保持 needs_reimage(不变成 key_mismatch);
// 重装确认时 purging 租约随重装结案, 待执行的 purge 作废。
func TestBareMetalReimageConfirmFinalizesPurging(t *testing.T) {
	e := newBMEnv(t, true)
	oldFP := e.fake.Fingerprint()
	// 在租期间退款(运营结案): 买家曾持有 root → 无论走哪条路结束都要求重装。
	require.NoError(t, e.comp.CompleteOrder(e.order.OrderNo))
	o, err := e.comp.GetOrder(e.order.OrderNo)
	require.NoError(t, err)
	tx := e.db.MustBegin()
	require.NoError(t, e.s.releaseLeaseTx(tx, o, "refunded"))
	require.NoError(t, tx.Commit())
	require.Equal(t, LeasePurging, e.leaseNow(t).State)
	require.Equal(t, VerifyNeedsReimage, e.node(t).VerifyStatus)

	newFP := e.fake.RotateHostKey()
	done := runOne(t, e.s)
	require.NotNil(t, done)
	assert.NotEqual(t, TaskSucceeded, done.State)
	n := e.node(t)
	assert.Equal(t, VerifyNeedsReimage, n.VerifyStatus, "闸门在时指纹变化不转 key_mismatch")
	require.NotNil(t, n.ReimagePrevFP)
	assert.Equal(t, oldFP, *n.ReimagePrevFP)

	taskID, err := e.s.ReimageConfirm(20, n.ID, newFP, "")
	require.NoError(t, err)
	assert.NotZero(t, taskID)
	assert.Equal(t, LeasePurged, e.leaseNow(t).State)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=?`, e.lease.ID))
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='purge' AND state='pending'`, e.lease.ID))
	var raw string
	require.NoError(t, e.db.Get(&raw, `SELECT after_value FROM audit_logs WHERE action='lease.purged' AND target_id=?`, e.lease.ID))
	var rc PurgeReceipt
	require.NoError(t, json.Unmarshal([]byte(raw), &rc))
	assert.Equal(t, "supplier_reimage", rc.Executor)
	assert.Equal(t, "reimaged", rc.Steps[0].Outcome)
}
