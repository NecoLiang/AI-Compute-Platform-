package fleet

import (
	"context"
	"strings"
	"testing"

	"tokenfactory/pkg/config"

	"github.com/stretchr/testify/assert"
)

// T-088 挖矿特征扫描集成测试: 真实 MySQL + 假 SSH 节点 + compute 冻结路径。

// miningOut 构造节点返回的扫描输出(docker top 与 /proc/net/tcp 真实格式)。
func miningOut(procs []string, tcp []string) string {
	var b strings.Builder
	b.WriteString("@@top\nPID                 COMMAND             COMMAND\n")
	for _, p := range procs {
		b.WriteString(p + "\n")
	}
	b.WriteString("@@tcp\n  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	b.WriteString("   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1\n")
	for i, c := range tcp {
		b.WriteString("   " + string(rune('1'+i)) + ": " + c + " 00000000:00000000 00:00000000 00000000     0        0 2 1\n")
	}
	b.WriteString("@@end\n")
	return b.String()
}

func TestMiningScanAlertThenAutoFreeze(t *testing.T) {
	e := activeProvEnv(t)
	e.comp.SetLeaseHooks(e.s.LeaseHooks())
	e.s.cfg.Mining = config.FleetMining{ProcessNames: config.DefaultMinerProcessNames(), PoolPorts: config.DefaultMiningPoolPorts()}
	scan := cmdOK(miningScanCommand(testProbeImage, e.lease.LeaseNo))
	alerts := func(typ string) int {
		return countRows(t, e.db, `SELECT COUNT(*) FROM risk_alerts WHERE target_type='order' AND target_id=? AND alert_type=?`,
			e.order.ID, typ)
	}
	ctx := context.Background()

	// 正常负载: 无命中。
	e.fake.Override(scan, fakeReply{Stdout: miningOut([]string{"4210 python3 python3 train.py --epochs 3"}, nil)})
	e.s.MiningScanRound(ctx)
	assert.Contains(t, e.fake.Commands(), scan, "经管理通道对在租容器执行扫描")
	assert.Equal(t, 0, alerts(miningAlertType))
	assert.Equal(t, LeaseActive, e.leaseNow(t).State)

	// 输出不完整 / 容器未运行: 本轮不判定。
	e.fake.Override(scan, fakeReply{Stdout: "@@top\nPID COMMAND COMMAND\n1 xmrig ./xmrig -o stratum+tcp://p:3333\n"})
	e.s.MiningScanRound(ctx)
	e.fake.Override(scan, fakeReply{Stdout: "@@notrunning\n"})
	e.s.MiningScanRound(ctx)
	assert.Equal(t, LeaseActive, e.leaseNow(t).State)
	assert.Equal(t, 0, alerts(miningAlertType)+alerts(miningFreezeType))

	// 矿工进程名 + 矿池常用端口外连: 只告警, 不冻结; 重复扫描不重复告警。
	suspect := miningOut([]string{"4302 xmrig ./xmrig --config c.json"},
		[]string{"0200000A:A2C0 0408A8C0:0D05 01"}) // 10.0.0.2 → 192.168.8.4:3333
	e.fake.Override(scan, fakeReply{Stdout: suspect})
	e.s.MiningScanRound(ctx)
	e.s.MiningScanRound(ctx)
	assert.Equal(t, 1, alerts(miningAlertType))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM risk_alerts WHERE target_id=? AND alert_type=? AND status='pending'
		AND level='medium' AND rule_detail LIKE '%pool_port%' AND rule_detail LIKE '%"process"%'`, e.order.ID, miningAlertType))
	assert.Equal(t, LeaseActive, e.leaseNow(t).State)
	assert.Equal(t, "active", orderStatus(t, e))

	// stratum 参数: 自动冻结(订单 frozen + 租约 suspended + 断会话 + 吊销凭证) + 告警 + 审计 + 通知。
	e.fake.Override(scan, fakeReply{Stdout: miningOut([]string{"4303 kworker /tmp/.k -o stratum+ssl://1.2.3.4:443 -u w"}, nil)})
	e.s.MiningScanRound(ctx)
	assert.Equal(t, "frozen", orderStatus(t, e))
	assert.Equal(t, "revoked", deliveryStatus(t, e))
	l := e.leaseNow(t)
	assert.Equal(t, LeaseSuspended, l.State)
	assert.NotNil(t, l.SuspendedAt)
	leaseTaskOf(t, e, KindKillSessions)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM risk_alerts WHERE target_id=? AND alert_type=? AND status='processing'
		AND resolution=? AND level='high' AND rule_detail LIKE '%stratum+ssl://%'`, e.order.ID, miningFreezeType, miningResAutoFrozen))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM audit_logs WHERE action=? AND target_type='order' AND target_id=?
		AND before_value='active' AND operator_id IS NULL`, miningAuditAction, e.order.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=? AND title='订单已冻结'`, e.order.BuyerID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=(SELECT supplier_id FROM supplier_nodes WHERE id=?)
		AND title='在租环境已冻结'`, e.lease.NodeID))

	// 冻结后不再扫描该租约(只扫 active), 不重复冻结/告警。
	n := len(e.fake.Commands())
	e.s.MiningScanRound(ctx)
	assert.Equal(t, n, len(e.fake.Commands()))
	assert.Equal(t, 1, alerts(miningFreezeType))
}

func TestMiningScanPoolAddrFreezesAndNodeDownSkips(t *testing.T) {
	e := activeProvEnv(t)
	e.comp.SetLeaseHooks(e.s.LeaseHooks())
	e.s.cfg.Mining.PoolAddrs = []string{"8.8.8.0/24"}
	scan := cmdOK(miningScanCommand(testProbeImage, e.lease.LeaseNo))

	// 扫描命令失败: 不判定。
	e.fake.Override(scan, fakeReply{Stderr: "Error: No such container", Code: 1})
	e.s.MiningScanRound(context.Background())
	assert.Equal(t, LeaseActive, e.leaseNow(t).State)

	// 进程改名但外连矿池地址清单: 冻结。
	e.fake.Override(scan, fakeReply{Stdout: miningOut([]string{"77 bash bash"}, []string{"0200000A:A2C2 08080808:01BB 01"})})
	e.s.MiningScanRound(context.Background())
	assert.Equal(t, LeaseSuspended, e.leaseNow(t).State)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM risk_alerts WHERE target_id=? AND alert_type=? AND rule_detail LIKE '%pool_addr%'`,
		e.order.ID, miningFreezeType))
}
