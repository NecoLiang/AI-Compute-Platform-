package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"tokenfactory/internal/scheduler"
)

// 真机只读联调(docs/25 §25.13 的接入/探活/健康段): 接入 → TOFU → 完整探活 → 轻探活, 打印每项结论。
// 只在设置 LIVE_NODE_HOST 时运行; 需 TEST_MYSQL_DSN(本地测试库, 每次新建)。
//
// 用于"机上已有生产服务在跑"的节点: 运行前确认每张卡都有宿主机计算进程(全部视为占用, 不挂卡、不压测),
// 有任一空闲卡即中止; 前后比对计算进程与 LIVE_ENGINE_HEALTH(如 http://127.0.0.1:8000/health), 不一致即失败。
// 管理公钥行经 LIVE_SSH_ALIAS(运维现有登录)写入, 结束时按公钥体删除且保留其余行。
//
//	LIVE_NODE_HOST/LIVE_NODE_PORT/LIVE_NODE_USER  节点 SSH(平台视角)
//	LIVE_SSH_ALIAS       运维本机可直接 ssh 的别名, 仅用于写入/删除管理公钥行与前后比对
//	LIVE_EGRESS_IP       本机访问节点的出口 IP(写入公钥行 from=)
//	LIVE_PROBE_IMAGE     探针镜像 ref@sha256(节点能拉取)
//	LIVE_TOTAL_CARDS     声明卡数(默认 8)
//	LIVE_ENGINE_HEALTH   可选, 节点本机上的服务健康地址
func TestLiveNodeReadOnly(t *testing.T) {
	host := os.Getenv("LIVE_NODE_HOST")
	if host == "" {
		t.Skip("LIVE_NODE_HOST 未设置, 跳过真机联调")
	}
	port, err := strconv.Atoi(os.Getenv("LIVE_NODE_PORT"))
	require.NoError(t, err, "LIVE_NODE_PORT")
	user, alias := os.Getenv("LIVE_NODE_USER"), os.Getenv("LIVE_SSH_ALIAS")
	egress, image := os.Getenv("LIVE_EGRESS_IP"), os.Getenv("LIVE_PROBE_IMAGE")
	require.NotEmpty(t, user, "LIVE_NODE_USER")
	require.NotEmpty(t, alias, "LIVE_SSH_ALIAS")
	require.NotEmpty(t, egress, "LIVE_EGRESS_IP")
	require.NoError(t, checkImage(image), "LIVE_PROBE_IMAGE")
	cards := 8
	if v := os.Getenv("LIVE_TOTAL_CARDS"); v != "" {
		cards, err = strconv.Atoi(v)
		require.NoError(t, err)
	}
	health := os.Getenv("LIVE_ENGINE_HEALTH")

	remote := func(cmd string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// 只取 stdout: 远端 bash 的 setlocale 告警走 stderr, 合并输出时会与无换行的输出拼在同一行。
		var stderr strings.Builder
		c := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=20", alias, cmd)
		c.Stderr = &stderr
		out, err := c.Output()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stripLocaleWarn(stderr.String())))
		}
		return string(out), err
	}
	snapshot := func() (map[string][]string, string) {
		out, err := remote(cmdGPUApps)
		require.NoError(t, err, out)
		apps, err := ParseComputeApps(stripLocaleWarn(out))
		require.NoError(t, err)
		code := ""
		// 节点 sshd 偶发拒绝新连接(MaxStartups), 健康读数失败重试, 仍失败如实报错而不是记空值。
		for i := 0; health != "" && i < 3; i++ {
			out, err := remote("curl -s -o /dev/null -w '%{http_code}' -m 10 " + health)
			if code = strings.TrimSpace(stripLocaleWarn(out)); err == nil && code != "" {
				break
			}
			t.Logf("读取服务健康第 %d 次失败: %v %q", i+1, err, out)
			time.Sleep(3 * time.Second)
		}
		return apps, code
	}

	// 前置: 每张卡都被宿主机进程占用(平台不会对其做任何侵入项), 否则中止。
	out, err := remote(cmdGPUList)
	require.NoError(t, err, out)
	gpus, err := ParseGPUList(stripLocaleWarn(out))
	require.NoError(t, err)
	appsBefore, healthBefore := snapshot()
	for _, g := range gpus {
		require.NotEmpty(t, appsBefore[strings.ToUpper(g.UUID)], "GPU%d 无宿主机进程: 本联调只用于全部卡在用的节点, 中止以免压测", g.Index)
	}
	t.Logf("前置: %d 张卡全部有宿主机计算进程; 服务健康=%q", len(gpus), healthBefore)

	db := setupFleetDB(t)
	seedManaged(t, db)
	db.MustExec(`UPDATE products SET card_count=? WHERE id=1`, cards)
	s := newTestFleet(t, db)
	s.cfg.ProbeImage, s.cfg.ControllerEgressIPs = image, []string{egress}
	s.cfg.ModelCacheDir = "/tmp/omnis-live-model-cache-absent"
	s.dialer.allowLoopback, s.dialer.DialTimeout, s.dialer.Attempts = false, 20*time.Second, 2

	v, err := s.RegisterManagedNode(10, scheduler.ManagedNodeReq{ProductID: 1, NodeName: "live-" + host, TotalCards: cards,
		LeasePortStart: 47000, LeasePortEnd: 47009})
	require.NoError(t, err)
	id := v.(*Node).ID
	ob, err := s.Onboarding(10, id)
	require.NoError(t, err)
	body := pubKeyBody(*v.(*Node).MgmtPubkey)
	cmd, err := installKeyCommand(ob.AuthorizedKeysLine, body)
	require.NoError(t, err)
	out, err = remote(cmd)
	require.NoError(t, err, out)
	require.Contains(t, out, "@@installed")
	t.Cleanup(func() {
		// 只删本次公钥行: 先备份, 行数恰好少 1 才覆盖(保留 inode 与权限), 否则保留备份并报错。
		rm := `f=~/.ssh/authorized_keys; cp -p "$f" "$f.omnis-live-bak" || exit 1; grep -vF '` + body + `' "$f" > "$f.omnis-tmp"; ` +
			`if [ "$(wc -l < "$f.omnis-tmp")" -eq "$(( $(wc -l < "$f") - 1 ))" ]; then cat "$f.omnis-tmp" > "$f" && rm -f "$f.omnis-tmp" "$f.omnis-live-bak" && echo @@removed; ` +
			`else rm -f "$f.omnis-tmp"; echo @@kept-backup; fi`
		out, err := remote(rm)
		if err != nil || !strings.Contains(out, "@@removed") {
			t.Errorf("管理公钥行未能删除, 请人工处理(备份 ~/.ssh/authorized_keys.omnis-live-bak): %v %s", err, out)
		}
	})

	ctx := context.Background()
	_, err = s.UpdateSSH(ctx, 10, id, SSHInput{Host: host, Port: port, User: user}, "")
	require.NoError(t, err)
	_, err = s.Verify(10, id)
	require.NoError(t, err)
	done := runTasks(t, s)
	require.Len(t, done, 1)
	o := outcomeOf(t, done[0])
	require.NotEmpty(t, o.AwaitingHostKey, "首次连接应停在 TOFU 等确认, failed=%v", o.Failed)
	scan, err := exec.Command("sh", "-c", fmt.Sprintf("ssh-keyscan -p %d %s 2>/dev/null | ssh-keygen -lf - | awk '{print $2}'", port, host)).Output()
	require.NoError(t, err)
	require.Contains(t, strings.Fields(string(scan)), o.AwaitingHostKey, "平台观测指纹应是 ssh-keyscan 结果之一")
	t.Logf("TOFU: 观测指纹 %s 与 ssh-keyscan 一致", o.AwaitingHostKey)

	_, err = s.ConfirmHostKey(10, id, o.AwaitingHostKey, "")
	require.NoError(t, err)
	started := time.Now()
	done = runTasks(t, s)
	require.Len(t, done, 1)
	o = outcomeOf(t, done[0])
	result, checks := latestChecks(t, db, id)
	t.Logf("完整探活 %s, 结论 %s, 未通过 %v", time.Since(started).Round(time.Second), result, o.Failed)
	ids := make([]string, 0, len(checks))
	for k := range checks {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool { return checkNum(ids[i]) < checkNum(ids[j]) })
	for _, k := range ids {
		c := checks[k]
		mark := "✔"
		switch {
		case c.Skipped:
			mark = "–"
		case !c.OK:
			mark = "✘"
		}
		t.Logf("  %s %-3s %s", mark, k, c.Detail)
	}
	var inv string
	require.NoError(t, db.Get(&inv, `SELECT inventory FROM node_probe_reports WHERE node_id=? ORDER BY id DESC LIMIT 1`, id))
	var parsed Inventory
	require.NoError(t, json.Unmarshal([]byte(inv), &parsed))
	for _, g := range gpus {
		require.Contains(t, parsed.Faulty[g.Index], "宿主机计算进程占用", "GPU%d 必须按占用排除", g.Index)
	}

	// 轻探活(60s 周期那一路): 遥测 + docker + Xid + 缓存目录 + 宿主机进程。
	n, err := s.repo.GetNode(id)
	require.NoError(t, err)
	s.lightProbe(ctx, n)
	var hb struct {
		Fail   int     `db:"probe_fail_count"`
		Status string  `db:"status"`
		Note   *string `db:"cache_note"`
	}
	require.NoError(t, db.Get(&hb, `SELECT probe_fail_count, status, cache_note FROM supplier_nodes WHERE id=?`, id))
	note := "<nil>"
	if hb.Note != nil {
		note = *hb.Note
	}
	t.Logf("轻探活: 失败计数 %d, 节点状态 %s, 缓存目录 %s", hb.Fail, hb.Status, note)
	nt := nodeTelemetry(t, db, id)
	t.Logf("轻探活 Xid 覆盖: from=%v", nt.XidFrom)

	appsAfter, healthAfter := snapshot()
	require.Equal(t, appsBefore, appsAfter, "宿主机计算进程前后必须一致")
	require.Equal(t, healthBefore, healthAfter, "服务健康前后必须一致")
	t.Logf("后置: 计算进程未变化, 服务健康=%q", healthAfter)
}

func checkNum(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "P"))
	return n
}

// stripLocaleWarn 去掉远端 shell 的 setlocale 告警行(不属于命令输出)。
func stripLocaleWarn(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.Contains(l, "setlocale") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}
