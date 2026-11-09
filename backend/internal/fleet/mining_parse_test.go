package fleet

import (
	"testing"

	"tokenfactory/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 真实格式样例: docker top -eo pid,comm,args 与 /proc/net/tcp{,6}。
const miningSample = `@@top
PID                 COMMAND             COMMAND
4101                sshd                sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups
4210                python3             python3 train.py --epochs 3
4302                xmrig               ./xmrig -o stratum+tcp://pool.example.org:3333 -u wallet
4303                kworker             /tmp/.k -o STRATUM2+SSL://1.2.3.4:443
@@tcp
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0200000A:0016 0300000A:B1E4 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0200000A:A2C0 0408A8C0:0D05 01 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 20 4 30 10 -1
   3: 0200000A:A2C2 08080808:0D05 02 00000000:00000000 00:00000000 00000000     0        0 4 1 0000000000000000 20 4 30 10 -1
   4: 0100007F:A2C4 0100007F:0D05 01 00000000:00000000 00:00000000 00000000     0        0 5 1 0000000000000000 20 4 30 10 -1
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0000000000000000FFFF00000200000A:A2C6 0000000000000000FFFF00000909090A:115C 01 00000000:00000000 00:00000000 00000000     0        0 6 1 0000000000000000 20 4 30 10 -1
@@end
`

func TestParseMiningScan(t *testing.T) {
	sc, err := ParseMiningScan(miningSample)
	require.NoError(t, err)
	assert.True(t, sc.Running)
	require.Len(t, sc.Procs, 4)
	assert.Equal(t, "xmrig", sc.Procs[2].Comm)
	assert.Contains(t, sc.Procs[2].Args, "stratum+tcp://")

	// 入站(本地端口 22 在监听)与环回被剔除; IPv4 小端解析; tcp6 的 v4-mapped 取 v4。
	var remotes []string
	for _, c := range sc.Conns {
		remotes = append(remotes, c.Remote.String())
	}
	assert.Equal(t, []string{"192.168.8.4:3333", "8.8.8.8:3333", "10.9.9.9:4444"}, remotes)

	sc, err = ParseMiningScan("@@notrunning\n")
	require.NoError(t, err)
	assert.False(t, sc.Running)

	_, err = ParseMiningScan("@@top\nPID COMMAND COMMAND\n1 sh sh\n@@tcp\n")
	assert.Error(t, err, "缺 @@end 视为不完整, 不判定")
}

func TestParseProcNetAddrIPv6(t *testing.T) {
	// 2001:db8::1 按 32 位字小端: 2001:0db8 → B80D0120, 0, 0, 00000001 → 01000000
	ap, err := parseProcNetAddr("B80D0120000000000000000001000000:01BB")
	require.NoError(t, err)
	assert.Equal(t, "[2001:db8::1]:443", ap.String())
	_, err = parseProcNetAddr("zz:01")
	assert.Error(t, err)
}

func TestMiningAnalyze(t *testing.T) {
	r, err := newMiningRules(config.FleetMining{ProcessNames: config.DefaultMinerProcessNames(),
		PoolPorts: config.DefaultMiningPoolPorts(), PoolAddrs: []string{"8.8.8.0/24"}})
	require.NoError(t, err)
	sc, err := ParseMiningScan(miningSample)
	require.NoError(t, err)
	hits := r.Analyze(sc)
	kinds := map[string]int{}
	for _, h := range hits {
		kinds[h.Kind]++
		assert.Equal(t, h.Kind == MiningHitStratum || h.Kind == MiningHitPoolAddr, h.Freeze, h.Kind)
	}
	assert.Equal(t, map[string]int{MiningHitStratum: 2, MiningHitPoolAddr: 1, MiningHitProcess: 1, MiningHitPoolPort: 2}, kinds)
	assert.True(t, hits[0].Freeze, "冻结类在前")

	// 只有进程名/常用端口: 只告警。
	clean := MiningScan{Running: true, Procs: []MiningProc{{PID: 1, Comm: "T-Rex", Args: "t-rex --algo kawpow"}}}
	hits = r.Analyze(clean)
	require.Len(t, hits, 1)
	assert.False(t, hits[0].Freeze)

	_, err = newMiningRules(config.FleetMining{PoolAddrs: []string{"pool.example.org"}})
	assert.Error(t, err, "不支持域名")
}
