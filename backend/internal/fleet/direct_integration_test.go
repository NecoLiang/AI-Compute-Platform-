package fleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tokenfactory/pkg/config"
	tfcrypto "tokenfactory/pkg/crypto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// 直连映射访问模式(fleet.access_mode=direct)集成测试: 真实 MySQL + 两台 fakeNode——
// e.fake 是宿主机(管理 sshd), ctr 充当租约容器 sshd(节点地址:租约端口)。宿主机收到的 sync_access 脚本由 ctrSim
// 按真实语义作用到 ctr: 写公钥 → 重写 ctr 的授权列表; cut → ctr 拒绝新连接; connect → 恢复。

// ctrSim 租约容器的模拟状态。
type ctrSim struct {
	t   *testing.T
	ctr *fakeNode

	mu        sync.Mutex
	connected bool
	modes     []string
	platform  []string // 最近一次写入的 platform_keys 行
}

var keysTarRe = regexp.MustCompile(`echo ([A-Za-z0-9+/=]+) \| base64 -d \| docker cp -a - "\$C":/etc/`)

// syncPrefix accessSyncCommand 的命令前缀: 脚本以 "C=omnis-l" 开头(9 字节, base64 前 12 个字符稳定)。
var syncPrefix = "echo " + base64.StdEncoding.EncodeToString([]byte("C=omnis-l"))

func (c *ctrSim) handle(cmd string) fakeReply {
	script := decodeB64(c.t, cmd)
	c.mu.Lock()
	defer c.mu.Unlock()
	var out strings.Builder
	switch {
	case strings.Contains(script, "docker network disconnect -f"):
		c.modes = append(c.modes, syncCut)
		c.connected = false
		c.ctr.SetDown(true)
		out.WriteString("@@disconnected\n@@done\n")
		return fakeReply{Stdout: out.String()}
	case strings.Contains(script, `docker network connect "$NW" "$C"`):
		c.modes = append(c.modes, syncConnect)
	default:
		c.modes = append(c.modes, syncKeys)
	}
	m := keysTarRe.FindStringSubmatch(script)
	if m == nil {
		return fakeReply{Stderr: "no keys tar", Code: 6}
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return fakeReply{Stderr: err.Error(), Code: 6}
	}
	files := readKeysTar(c.t, raw)
	var keys []ssh.PublicKey
	c.platform = nil
	for _, f := range []string{"omnis/platform_keys", "omnis/buyer_keys"} {
		for _, line := range nonEmptyLines(files[f][1]) {
			k, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				return fakeReply{Stderr: err.Error(), Code: 6}
			}
			if f == "omnis/platform_keys" {
				c.platform = append(c.platform, line)
				if len(opts) == 0 || opts[0] != `from="10.0.0.1"` {
					return fakeReply{Stderr: "平台公钥行缺少 from=", Code: 6}
				}
			}
			keys = append(keys, k)
		}
	}
	c.ctr.SetAuthorized(keys)
	out.WriteString("@@keys\n")
	if c.modes[len(c.modes)-1] == syncConnect {
		if !c.connected {
			out.WriteString("@@connected\n")
		}
		c.connected = true
		c.ctr.SetDown(false)
		out.WriteString("@@port " + strconv.Itoa(c.ctr.Port()) + "\n")
	}
	out.WriteString("@@done\n")
	return fakeReply{Stdout: out.String()}
}

func (c *ctrSim) lastMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.modes) == 0 {
		return ""
	}
	return c.modes[len(c.modes)-1]
}

// newDirectProvEnv 同 newProvEnv, 访问模式为 direct: 租约端口段只有 ctr 监听的那一个端口。
func newDirectProvEnv(t *testing.T) (*provEnv, *ctrSim) {
	t.Helper()
	e := newProvNode(t, "container", false)
	e.s.cfg.AccessMode = config.AccessModeDirect
	require.True(t, e.s.directMode())

	sim := &ctrSim{t: t, ctr: newFakeNode(t, "")}
	sim.ctr.Override("nvidia-smi -L", fakeReply{Stdout: gpuListL(fakeUUID0, fakeUUID1)})
	p := sim.ctr.Port()
	e.db.MustExec(`UPDATE supplier_nodes SET lease_port_start=?, lease_port_end=? WHERE id=?`, p, p, e.node)
	e.fake.Override("for i in $(seq 1 30); do if", fakeReply{Stdout: sim.ctr.HostKeyLine() + " root@lease\n"})
	e.fake.Handle(syncPrefix, sim.handle)
	e.fake.Override(`if [ "$(docker inspect --type container -f '{{.State.Running}}' omnis-l-`, fakeReply{Stdout: "@@killed 1\n"})

	var err error
	e.order, err = placeManaged(e.comp, 21, 2)
	require.NoError(t, err)
	e.payAndLoad(t)
	return e, sim
}

func activeDirectEnv(t *testing.T) (*provEnv, *ctrSim) {
	t.Helper()
	e, sim := newDirectProvEnv(t)
	e.outcome(t, runOne(t, e.s))
	e.lease = e.leaseNow(t)
	require.Equal(t, LeaseActive, e.lease.State)
	return e, sim
}

// loginAs 以 signer 登录 ctr(钉死主机公钥), 返回错误即登录不成功。
func loginAs(sim *ctrSim, user string, signer ssh.Signer) error {
	c, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(sim.ctr.Port())), &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(sim.ctr.HostKey()), Timeout: 2 * time.Second})
	if err == nil {
		_ = c.Close()
	}
	return err
}

// runKind 执行下一条到期任务并断言其类型与结果。
func runKind(t *testing.T, e *provEnv, kind string) *Task {
	t.Helper()
	task := runOne(t, e.s)
	require.NotNil(t, task)
	require.Equal(t, kind, task.Kind)
	require.Equal(t, TaskSucceeded, task.State, "last_error=%v", task.LastError)
	return task
}

func syncTaskCount(t *testing.T, e *provEnv, state string) int {
	t.Helper()
	return countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind=? AND state=?`, e.lease.ID, KindSyncAccess, state)
}

func TestDirectProvisionHappyPath(t *testing.T) {
	e, sim := newDirectProvEnv(t)
	port := sim.ctr.Port()

	o := e.outcome(t, runOne(t, e.s))
	assert.Equal(t, port, o.UpstreamPort)
	assert.Equal(t, sim.ctr.Fingerprint(), o.UpstreamHostKeyFP)
	assert.Empty(t, o.Firewall, "直连模式不下发来源限制规则")
	assert.Empty(t, e.cmdsWith("docker run --rm --network host --cap-drop ALL"))
	assert.Equal(t, syncConnect, sim.lastMode(), "开通时写公钥并接入租约网络")
	require.Len(t, sim.platform, 1)
	assert.Contains(t, sim.platform[0], "no-port-forwarding")
	assert.Contains(t, sim.ctr.Commands(), "nvidia-smi -L", "自检走买家同一入口: 节点地址:租约端口")

	l := e.leaseNow(t)
	assert.Equal(t, LeaseActive, l.State)
	require.NotNil(t, l.UpstreamPort)
	assert.Equal(t, port, *l.UpstreamPort)

	created := e.cmdsWith("set -e; (docker network inspect omnis-l-")
	require.Len(t, created, 1)
	assert.Contains(t, created[0], "-p "+strconv.Itoa(port)+":22")

	// 交付记录与连接信息: 节点地址 + 映射端口 + root + 主机指纹, 仅公钥。
	d, err := e.comp.GetDelivery(e.order.ID)
	require.NoError(t, err)
	require.NotNil(t, d)
	info, err := tfcrypto.Decrypt(d.CredentialEncrypted, bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	assert.Contains(t, info, `"127.0.0.1"`)
	assert.Contains(t, info, strconv.Itoa(port))
	assert.Contains(t, info, `"root"`)
	assert.NotContains(t, info, "gw.test")

	acc, err := e.s.OrderAccess(21, e.order.OrderNo)
	require.NoError(t, err)
	assert.Equal(t, config.AccessModeDirect, acc.AccessMode)
	assert.Equal(t, "127.0.0.1", acc.Host)
	assert.Equal(t, port, acc.Port)
	assert.Equal(t, sim.ctr.Fingerprint(), acc.HostKeyFingerprint)
	assert.Equal(t, "root", acc.Login)
	assert.Equal(t, []string{authMethodPubkey}, acc.AuthMethods)
	assert.Empty(t, acc.GatewayHost)
	raw, err := json.Marshal(acc)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"access_mode":"direct"`)

	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title='算力已开通'
		AND content LIKE ?`, "%ssh -p "+strconv.Itoa(port)+" root@127.0.0.1%"))
}

func TestDirectProvisionSelftestRejectsWrongSSHD(t *testing.T) {
	e, _ := newDirectProvEnv(t)
	// 端口上的 sshd 主机公钥与开通时从容器内读到的不一致(端口被别的进程占用): 自检失败, 不交付。
	line, _ := fakeContainerHostKey(t)
	e.fake.Override("for i in $(seq 1 30); do if", fakeReply{Stdout: line + "\n"})
	got := runOne(t, e.s)
	require.NotNil(t, got)
	assert.Equal(t, TaskPending, got.State)
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, "直连自检失败")
	assert.Contains(t, *got.LastError, "主机指纹")
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM order_deliveries WHERE order_id=?`, e.order.ID))
}

func TestDirectBuyerKeysSyncToLease(t *testing.T) {
	e, sim := activeDirectEnv(t)
	buyer := newSigner(t)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(buyer.PublicKey())))
	require.Error(t, loginAs(sim, "root", buyer), "未登记公钥前不能登录")

	k, err := e.s.AddBuyerKey(21, "laptop", line, "203.0.113.9")
	require.NoError(t, err)
	assert.Equal(t, 1, syncTaskCount(t, e, TaskPending))
	// 未开始的同步任务合并: 再加一把不重复入队。
	other := newSigner(t)
	_, err = e.s.AddBuyerKey(21, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(other.PublicKey()))), "203.0.113.9")
	require.NoError(t, err)
	assert.Equal(t, 1, syncTaskCount(t, e, TaskPending))

	task := runKind(t, e, KindSyncAccess)
	var out SyncAccessOutcome
	require.NoError(t, json.Unmarshal(task.Result, &out))
	assert.Equal(t, syncConnect, out.Mode)
	assert.Equal(t, sim.ctr.Port(), out.Result.Port)
	require.NoError(t, loginAs(sim, "root", buyer))
	require.NoError(t, loginAs(sim, "root", other))

	require.NoError(t, e.s.DeleteBuyerKey(21, k.ID, "203.0.113.9"))
	runKind(t, e, KindSyncAccess)
	assert.Error(t, loginAs(sim, "root", buyer), "删除的公钥不能再登录")
	assert.NoError(t, loginAs(sim, "root", other))

}

func TestDirectFreezeCutsAndUnfreezeRestores(t *testing.T) {
	e, sim := activeDirectEnv(t)
	buyer := newSigner(t)
	_, err := e.s.AddBuyerKey(21, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(buyer.PublicKey()))), "203.0.113.9")
	require.NoError(t, err)
	runKind(t, e, KindSyncAccess)
	require.NoError(t, loginAs(sim, "root", buyer))

	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	require.Equal(t, LeaseSuspended, e.leaseNow(t).State)
	task := runKind(t, e, KindKillSessions)
	assert.Equal(t, syncCut, sim.lastMode(), "冻结先断开容器网络")
	var ko KillSessionsOutcome
	require.NoError(t, json.Unmarshal(task.Result, &ko))
	assert.Equal(t, 1, ko.Killed, "再结束已有会话")
	assert.Error(t, loginAs(sim, "root", buyer), "冻结后买家连不上")

	// 冻结中买家改公钥: sync_access 按执行时的租约状态(suspended)保持断开。
	_, err = e.s.AddBuyerKey(21, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))), "203.0.113.9")
	require.NoError(t, err)
	runKind(t, e, KindSyncAccess)
	assert.Equal(t, syncCut, sim.lastMode(), "冻结期间的同步保持断开, 不会被公钥变化重新接入")
	assert.Error(t, loginAs(sim, "root", buyer))

	_, err = e.comp.AdminUnfreezeOrder(10, e.order.OrderNo, false, "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, LeaseActive, e.leaseNow(t).State)
	assert.Equal(t, 1, syncTaskCount(t, e, TaskPending), "解冻入队重新接入")
	runKind(t, e, KindSyncAccess)
	assert.Equal(t, syncConnect, sim.lastMode())
	assert.NoError(t, loginAs(sim, "root", buyer), "解冻后恢复连接")
}

func TestGatewayModeNeverEnqueuesSyncAccess(t *testing.T) {
	e := activeProvEnv(t)
	require.False(t, e.s.directMode())
	k, err := e.s.AddBuyerKey(21, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))), "203.0.113.9")
	require.NoError(t, err)
	require.NoError(t, e.s.DeleteBuyerKey(21, k.ID, "203.0.113.9"))
	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	_, err = e.comp.AdminUnfreezeOrder(10, e.order.OrderNo, false, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE kind=?`, KindSyncAccess))
}

func TestDirectOnboarding(t *testing.T) {
	e, sim := newDirectProvEnv(t)
	ob, err := e.s.Onboarding(20, e.node)
	require.NoError(t, err)
	assert.Equal(t, config.AccessModeDirect, ob.AccessMode)
	assert.Empty(t, ob.FirewallScripts, "直连模式不需要来源限制脚本")
	assert.Empty(t, ob.GatewayEgressIPs)
	assert.True(t, strings.HasPrefix(ob.AuthorizedKeysLine, `from="10.0.0.1",`))
	port := strconv.Itoa(sim.ctr.Port())
	assert.Contains(t, strings.Join(ob.Requirements, "\n"), port+"-"+port+" 对公网(买家)开放入方向")
	raw, err := json.Marshal(ob)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"firewall_scripts":{}`)
	assert.Contains(t, string(raw), `"gateway_egress_ips":[]`)
}

func TestDirectReconcileRestartRechecksViaPublicEntry(t *testing.T) {
	e, sim := activeDirectEnv(t)
	rec := &recordedObs{}
	e.s.SetOutageRecorder(rec)
	no := e.lease.LeaseNo
	start := cmdOK(startContainerCommand(no))
	stop := cmdOK(stopContainerCommand(no))
	e.fake.Override(start, fakeReply{Stdout: "@@started\n"})
	e.fake.Override(stop, fakeReply{Stdout: "@@stopped\n"})
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: containerNameOf(no) + " exited\n"})
	logins := func() int {
		n := 0
		for _, c := range sim.ctr.Commands() {
			if c == "nvidia-smi -L" {
				n++
			}
		}
		return n
	}
	before := logins()
	sim.ctr.SetDown(true) // 容器停止期间入口不可用

	// 节点重启后容器 exited: 不下发防火墙 → start → 主机密钥 → 写公钥/接入网络 → 直连复检。
	e.s.ReconcileRound(context.Background())
	assert.Empty(t, e.cmdsWith("docker run --rm --network host --cap-drop ALL"))
	assert.Contains(t, e.fake.Commands(), start)
	assert.NotContains(t, e.fake.Commands(), stop)
	assert.Equal(t, syncConnect, sim.lastMode())
	assert.Equal(t, before+1, logins())
	assert.True(t, rec.last().Up)

	// 复检登录不上(入口被占用/端口未放行): 停回去, 不对外。
	sim.ctr.RotateHostKey()
	e.s.ReconcileRound(context.Background())
	assert.Contains(t, e.fake.Commands(), stop)
	assert.False(t, rec.last().Up)
	require.NotNil(t, e.leaseNow(t).ResourceAlert)
}

// 开通途中(容器已建好、买家公钥与端口映射已就位, 自检未过尚未激活)订单被冻结: 直连模式没有网关拦截,
// 冻结必须入队 sync_access 并断开容器; 解冻后重新开通恢复连接。
func TestDirectFreezeDuringProvisioningCutsAccess(t *testing.T) {
	e, sim := newDirectProvEnv(t)
	e.lease = e.leaseNow(t)
	buyer := newSigner(t)
	_, err := e.s.AddBuyerKey(21, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(buyer.PublicKey()))), "203.0.113.9")
	require.NoError(t, err)
	runKind(t, e, KindSyncAccess)
	assert.Equal(t, syncKeys, sim.lastMode(), "订单仍在开通中: 只写公钥")

	// 自检失败(容器内看不到分配的卡): 开通任务退避重试, 此时容器已接入网络且买家公钥可登录。
	sim.ctr.Override("nvidia-smi -L", fakeReply{Stdout: gpuListL(fakeUUID0)})
	got := runOne(t, e.s)
	require.NotNil(t, got)
	require.Equal(t, KindProvision, got.Kind)
	require.Equal(t, TaskPending, got.State)
	require.Equal(t, LeaseProvisioning, e.leaseNow(t).State)
	require.NoError(t, loginAs(sim, "root", buyer), "前置: 激活前容器已可登录")

	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	require.Equal(t, LeaseProvisioning, e.leaseNow(t).State, "开通中冻结不迁移租约状态")
	require.Equal(t, 1, syncTaskCount(t, e, TaskPending), "开通中冻结入队 sync_access")
	runKind(t, e, KindSyncAccess)
	assert.Equal(t, syncCut, sim.lastMode(), "订单已不在开通中: 断开容器")
	assert.Error(t, loginAs(sim, "root", buyer), "冻结后买家连不上")

	// 冻结期间买家改公钥也不会重新接入。
	_, err = e.s.AddBuyerKey(21, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))), "203.0.113.9")
	require.NoError(t, err)
	runKind(t, e, KindSyncAccess)
	assert.Equal(t, syncCut, sim.lastMode())
	assert.Error(t, loginAs(sim, "root", buyer))
}

// 开通任务执行期间订单被冻结: 流水线走到激活才发现, 放弃激活的同时立即断开已接入的容器。
func TestDirectProvisionAbandonedCutsAccess(t *testing.T) {
	e, sim := newDirectProvEnv(t)
	e.lease = e.leaseNow(t)
	buyer := newSigner(t)
	_, err := e.s.AddBuyerKey(21, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(buyer.PublicKey()))), "203.0.113.9")
	require.NoError(t, err)
	runKind(t, e, KindSyncAccess)

	// 自检阶段(容器已接入)冻结订单: 用自检命令的回调模拟"开通任务执行中途"。
	var once sync.Once
	sim.ctr.ClearOverrides()
	sim.ctr.Handle("nvidia-smi -L", func(string) fakeReply {
		once.Do(func() { require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo)) })
		return fakeReply{Stdout: gpuListL(fakeUUID0, fakeUUID1)}
	})
	got := runOne(t, e.s)
	require.NotNil(t, got)
	require.Equal(t, KindProvision, got.Kind)
	require.Equal(t, TaskSucceeded, got.State, "last_error=%v", got.LastError)
	assert.True(t, e.outcome(t, got).Abandoned)
	assert.Equal(t, syncCut, sim.lastMode(), "放弃激活时立即断开")
	assert.Error(t, loginAs(sim, "root", buyer), "未激活的租约不能留着可登录的容器")
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM order_deliveries WHERE order_id=?`, e.order.ID))
}

// 在租订单被关闭/退款: purge 走普通车道, 之前必须有快车道 sync_access 先断开入口。
func TestDirectPurgeCutsAccessFirst(t *testing.T) {
	e, sim := activeDirectEnv(t)
	buyer := newSigner(t)
	_, err := e.s.AddBuyerKey(21, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(buyer.PublicKey()))), "203.0.113.9")
	require.NoError(t, err)
	runKind(t, e, KindSyncAccess)
	require.NoError(t, loginAs(sim, "root", buyer))

	require.NoError(t, e.comp.AdminUpdateOrderStatus(1, e.order.OrderNo, "cancelled", "127.0.0.1"))
	require.Equal(t, LeasePurging, e.leaseNow(t).State)
	require.Equal(t, 1, syncTaskCount(t, e, TaskPending), "进入清除时入队 sync_access")
	assert.True(t, isExpressKind(KindSyncAccess), "断开入口走快车道, 不排在同节点长任务之后")

	task := runOne(t, e.s)
	require.NotNil(t, task)
	require.Equal(t, KindSyncAccess, task.Kind, "断开先于 purge 执行")
	require.Equal(t, TaskSucceeded, task.State, "last_error=%v", task.LastError)
	assert.Equal(t, syncCut, sim.lastMode())
	assert.Error(t, loginAs(sim, "root", buyer), "订单关闭后买家立即连不上, 不必等 purge")
}
