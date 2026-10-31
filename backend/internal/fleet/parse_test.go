package fleet

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 样本均按 nvidia-smi / docker / journalctl 的真实输出格式整理(8×H100 SXM、4×RTX 4090、2×A40)。

const h100GPUList = `0, GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11, NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:18:00.0, 535.104.05
1, GPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d, NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:2A:00.0, 535.104.05
2, GPU-1d4f2c3b-5e6a-7b8c-9d0e-1f2a3b4c5d6e, NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:3A:00.0, 535.104.05
3, GPU-2e5a3d4c-6f7b-8c9d-0e1f-2a3b4c5d6e7f, NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:5D:00.0, 535.104.05
4, GPU-3f6b4e5d-7a8c-9d0e-1f2a-3b4c5d6e7f80, NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:9A:00.0, 535.104.05
5, GPU-4a7c5f6e-8b9d-0e1f-2a3b-4c5d6e7f8091, NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:AB:00.0, 535.104.05
6, GPU-5b8d6a7f-9c0e-1f2a-3b4c-5d6e7f8091a2, NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:BA:00.0, 535.104.05
7, GPU-6c9e7b8a-0d1f-2a3b-4c5d-6e7f8091a2b3, NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:DB:00.0, 535.104.05
`

const h100Topo = "\t\x1b[4mGPU0\tGPU1\tGPU2\tGPU3\tGPU4\tGPU5\tGPU6\tGPU7\tNIC0\tNIC1\tCPU Affinity\tNUMA Affinity\tGPU NUMA ID\x1b[0m\n" +
	"GPU0\t X \tNV18\tNV18\tNV18\tNV18\tNV18\tNV18\tNV18\tPXB\tSYS\t0-55,112-167\t0\t\tN/A\n" +
	"GPU1\tNV18\t X \tNV18\tNV18\tNV18\tNV18\tNV18\tNV18\tSYS\tSYS\t0-55,112-167\t0\t\tN/A\n" +
	"GPU2\tNV18\tNV18\t X \tNV18\tNV18\tNV18\tNV18\tNV18\tSYS\tSYS\t0-55,112-167\t0\t\tN/A\n" +
	"GPU3\tNV18\tNV18\tNV18\t X \tNV18\tNV18\tNV18\tNV18\tSYS\tSYS\t0-55,112-167\t0\t\tN/A\n" +
	"GPU4\tNV18\tNV18\tNV18\tNV18\t X \tNV18\tNV18\tNV18\tSYS\tPXB\t56-111,168-223\t1\t\tN/A\n" +
	"GPU5\tNV18\tNV18\tNV18\tNV18\tNV18\t X \tNV18\tNV18\tSYS\tSYS\t56-111,168-223\t1\t\tN/A\n" +
	"GPU6\tNV18\tNV18\tNV18\tNV18\tNV18\tNV18\t X \tNV18\tSYS\tSYS\t56-111,168-223\t1\t\tN/A\n" +
	"GPU7\tNV18\tNV18\tNV18\tNV18\tNV18\tNV18\tNV18\t X \tSYS\tSYS\t56-111,168-223\t1\t\tN/A\n" +
	"NIC0\tPXB\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\t X \tSYS\n" +
	"NIC1\tSYS\tSYS\tSYS\tSYS\tPXB\tSYS\tSYS\tSYS\tSYS\t X \n\n" +
	"Legend:\n\n  X    = Self\n  SYS  = Connection traversing PCIe as well as the SMP interconnect between NUMA nodes (e.g., QPI/UPI)\n" +
	"  NODE = Connection traversing PCIe as well as the interconnect between PCIe Host Bridges within a NUMA node\n" +
	"  PHB  = Connection traversing PCIe as well as a PCIe Host Bridge (typically the CPU)\n" +
	"  PXB  = Connection traversing multiple PCIe bridges (without traversing the PCIe Host Bridge)\n" +
	"  PIX  = Connection traversing at most a single PCIe bridge\n" +
	"  NV#  = Connection traversing a bonded set of # NVLinks\n"

// 4×RTX 4090: 0/1 同一 PCIe 交换, 2/3 同 NUMA 不同 Host Bridge(NODE), 两组之间 SYS。
const rtx4090Topo = "\tGPU0\tGPU1\tGPU2\tGPU3\tCPU Affinity\tNUMA Affinity\tGPU NUMA ID\n" +
	"GPU0\t X \tPIX\tSYS\tSYS\t0-31\t0\t\tN/A\n" +
	"GPU1\tPIX\t X \tSYS\tSYS\t0-31\t0\t\tN/A\n" +
	"GPU2\tSYS\tSYS\t X \tNODE\t32-63\t1\t\tN/A\n" +
	"GPU3\tSYS\tSYS\tNODE\t X \t32-63\t1\t\tN/A\n\nLegend:\n\n  X    = Self\n"

func TestParseGPUList(t *testing.T) {
	gpus, err := ParseGPUList(h100GPUList)
	require.NoError(t, err)
	require.Len(t, gpus, 8)
	assert.Equal(t, "GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11", gpus[0].UUID)
	assert.Equal(t, 81559, gpus[0].MemoryMiB)
	assert.Equal(t, "NVIDIA H100 80GB HBM3", gpus[7].Name)
	assert.Equal(t, "535.104.05", gpus[3].Driver)

	// nounits 格式
	g, err := ParseGPUList("0, GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA GeForce RTX 4090, 24564, 00000000:01:00.0, 550.54.15\n")
	require.NoError(t, err)
	assert.Equal(t, 24564, g[0].MemoryMiB)
}

func TestParseGPUListRejectsAbnormalOutput(t *testing.T) {
	cases := map[string]string{
		"驱动未加载":     "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver. Make sure that the latest NVIDIA driver is installed and running.\n",
		"空输出":       "",
		"UUID 被篡改":  "0, GPU-$(reboot), NVIDIA H100 80GB HBM3, 81559 MiB, 00000000:18:00.0, 535.104.05\n",
		"index 不连续": "0, GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA H100, 81559 MiB, 00000000:18:00.0, 535\n2, GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeef, NVIDIA H100, 81559 MiB, 00000000:2A:00.0, 535\n",
		"UUID 重复":   "0, GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA H100, 81559 MiB, 00000000:18:00.0, 535\n1, GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA H100, 81559 MiB, 00000000:2A:00.0, 535\n",
		"显存异常":      "0, GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA H100, [N/A], 00000000:18:00.0, 535\n",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseGPUList(out)
			assert.Error(t, err)
		})
	}
}

func TestParseTopo(t *testing.T) {
	g, err := ParseTopo(h100Topo, 8)
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		assert.Equal(t, "g0-nvlink", g[i])
	}

	g, err = ParseTopo(rtx4090Topo, 4)
	require.NoError(t, err)
	assert.Equal(t, map[int]string{0: "g0-pcie", 1: "g0-pcie", 2: "g1-numa", 3: "g1-numa"}, g)

	single := "\tGPU0\tCPU Affinity\tNUMA Affinity\tGPU NUMA ID\nGPU0\t X \t0-15\t0\t\tN/A\n"
	g, err = ParseTopo(single, 1)
	require.NoError(t, err)
	assert.Equal(t, "g0-solo", g[0])

	_, err = ParseTopo(h100Topo, 4)
	assert.Error(t, err, "矩阵卡数与清单不符必须报错")
	_, err = ParseTopo("command not found: nvidia-smi", 1)
	assert.Error(t, err)
}

func TestParseECCAndRemap(t *testing.T) {
	out := "0, 0, No\n1, 2, No\n2, [N/A], [N/A]\n3, 0, Yes\n"
	h, err := ParseECC(out)
	require.NoError(t, err)
	assert.Equal(t, 0, h[0].UncorrectedECC)
	assert.Equal(t, 2, h[1].UncorrectedECC)
	assert.Equal(t, -1, h[2].UncorrectedECC)
	assert.True(t, h[3].RetiredPending)

	r, err := ParseRemappedRows("GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee, 0, 0\nGPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeef, 0, 1\n")
	require.NoError(t, err)
	assert.False(t, r["GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"].Failure)
	assert.True(t, r["GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeef"].Failure)

	_, err = ParseECC("Field \"retired_pages.pending\" is not a valid field to query.\n")
	assert.Error(t, err)
}

// xidHead 真实节点上 xidCommand 输出的头部(@@now 对应 2026-10-28T10:00:00+08:00)。
const xidHead = "@@id uid=0 groups=root\n@@now 1793152800\n"

func TestParseXid(t *testing.T) {
	out := xidHead + "@@src journal\n@@first 2026-10-27T08:00:01+0800\n" +
		"2026-10-28T09:12:01+0800 gpu-node-01 kernel: NVRM: Xid (PCI:0000:9a:00): 79, pid=0, name=, GPU has fallen off the bus.\n" +
		"2026-10-28T09:20:44+0800 gpu-node-01 kernel: NVRM: Xid (PCI:0000:18:00): 13, pid=48211, name=python3, Graphics SM Warp Exception on (GPC 1, TPC 0, SM 0)\n"
	x := ParseXid(out, time.Hour)
	require.True(t, x.Known)
	assert.Equal(t, "journal", x.Source)
	assert.Equal(t, 2, x.Events)
	assert.Equal(t, []int{79}, x.FatalXidsFor("00000000:9A:00.0"))
	assert.Empty(t, x.FatalXidsFor("00000000:18:00.0"), "Xid 13 是应用错误, 不判卡故障")
	now := time.Unix(1793152800, 0)
	assert.True(t, x.NodeNow.Equal(now))
	assert.True(t, x.CoverFrom.Equal(now.Add(-time.Hour)), "日志从昨天开始, 时间窗完整覆盖")
	require.Len(t, x.List, 2)
	assert.True(t, x.List[0].At.Equal(time.Date(2026, 10, 28, 1, 12, 1, 0, time.UTC)))
	assert.Equal(t, "0000:9a:00", x.List[0].Bus)

	clean := ParseXid("@@id uid=1001 groups=omnis docker adm\n@@now 1793152800\n@@src journal\n@@first 2026-10-27T08:00:01+08:00\n", time.Hour)
	assert.True(t, clean.Known)
	assert.Zero(t, clean.Events)

	// journalctl 对非特权用户把提示写到 stderr, 经 @@err 前缀送回。
	noPerm := ParseXid("@@id uid=1001 groups=omnis docker adm\n@@now 1793152800\n@@src journal\n@@first \n"+
		"@@err Hint: You are currently not seeing messages from other users and the system.\n"+
		"@@err       Users in groups 'adm', 'systemd-journal' can see all messages.\n", time.Hour)
	assert.False(t, noPerm.Known)
	assert.Contains(t, noPerm.Note, "无权限")

	unprivileged := ParseXid("@@id uid=1001 groups=omnis docker\n@@now 1793152800\n@@src journal\n", time.Hour)
	assert.False(t, unprivileged.Known)

	// 旧格式(无 @@src)视为输出不完整。
	assert.False(t, ParseXid("@@id uid=0 groups=root\n", time.Hour).Known)
}

func TestParseXidDmesgFallback(t *testing.T) {
	// dmesg --time-format iso 不能按时间过滤: 窗口外的旧事件由 ParseXid 丢弃; 环形缓冲最早消息晚于窗口起点时覆盖起点后移。
	out := xidHead + "@@src dmesg\n@@first 2026-10-28T09:30:00,000000+08:00\n" +
		"2026-10-28T08:10:00,111111+08:00 NVRM: Xid (PCI:0000:18:00): 48, pid=0, name=, An uncorrectable double bit error (DBE) has been detected on GPU in the framebuffer at partition 6, subpartition 0.\n" +
		"2026-10-28T09:45:12,654321+08:00 NVRM: Xid (PCI:0000:18:00): 94, pid=4122, name=python3, Contained: SM (0x1). RST: No, D-RST: No\n"
	x := ParseXid(out, time.Hour)
	require.True(t, x.Known)
	assert.Equal(t, "dmesg", x.Source)
	assert.Equal(t, 1, x.Events, "08:10 的事件在 1 小时窗口外")
	assert.Empty(t, x.FatalXidsFor("0000:18:00"), "Xid 94 为 RESTART_APP, 不判卡故障")
	assert.True(t, x.CoverFrom.Equal(time.Date(2026, 10, 28, 1, 30, 0, 0, time.UTC)))
	assert.True(t, x.List[0].At.Equal(time.Date(2026, 10, 28, 1, 45, 12, 654321000, time.UTC)))

	restricted := ParseXid(xidHead+"@@src dmesg\n@@first \n@@err dmesg: read kernel buffer failed: Operation not permitted\n", time.Hour)
	assert.False(t, restricted.Known)
	assert.Contains(t, restricted.Note, "无权限")

	// 首条消息时间读不到: 不能确认过去的日志完整, 覆盖起点 = 扫描时刻。
	noFirst := ParseXid(xidHead+"@@src dmesg\n@@first \n", time.Hour)
	assert.True(t, noFirst.Known)
	assert.True(t, noFirst.CoverFrom.Equal(noFirst.NodeNow))
}

func TestParseXidTruncated(t *testing.T) {
	var b strings.Builder
	b.WriteString(xidHead + "@@src journal\n@@first 2026-10-27T08:00:01+0800\n")
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))
	for i := 0; i <= xidLineCap; i++ {
		ts := start.Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05-0700")
		fmt.Fprintf(&b, "%s gpu-node-01 kernel: NVRM: Xid (PCI:0000:18:00): 31, pid=1, name=x, MMU Fault\n", ts)
	}
	x := ParseXid(b.String(), time.Hour)
	require.True(t, x.Known)
	assert.Equal(t, xidLineCap, x.Events)
	assert.True(t, x.CoverFrom.Equal(start.Add(time.Second)), "截断后只能确认最早保留事件之后")
}

func TestParseXidMissingTools(t *testing.T) {
	x := ParseXid(xidHead+"@@src dmesg\n@@first \n@@err sh: 1: dmesg: not found\n", time.Hour)
	assert.False(t, x.Known)
	assert.Contains(t, x.Note, "dmesg: not found")
}

func TestFatalXidsCatalog(t *testing.T) {
	for _, n := range []int{46, 48, 62, 64, 74, 79, 95, 109, 119, 120, 136, 140, 143, 155, 156, 158} {
		assert.True(t, FatalXids[n], "Xid %d", n)
	}
	for _, n := range []int{13, 31, 43, 61, 63, 92, 94} {
		assert.False(t, FatalXids[n], "Xid %d", n)
	}
}

func TestXidSince(t *testing.T) {
	s, w := xidSince(30 * time.Second)
	assert.Equal(t, "-2min", s)
	assert.Equal(t, 2*time.Minute, w)
	s, w = xidSince(61*time.Minute + time.Second)
	assert.Equal(t, "-62min", s)
	assert.Equal(t, 62*time.Minute, w)
	s, w = xidSince(20 * time.Hour)
	assert.Equal(t, "-20h", s)
	assert.Equal(t, 20*time.Hour, w)
	s, _ = xidSince(72 * time.Hour)
	assert.Equal(t, "-24h", s)
	for _, d := range []time.Duration{0, time.Minute, 999 * time.Minute, 1000 * time.Minute, 48 * time.Hour} {
		s, _ := xidSince(d)
		_, err := xidCommand(s)
		assert.NoError(t, err, s)
	}
}

func TestParseTelemetry(t *testing.T) {
	// H100 SXM 实际格式(nounits); 第二张卡在跑负载, 第三张为不支持 ECC/功耗的消费卡读数。
	out := "GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11, 1, 81559, 31, 69.37, 0x0000000000000001, 0, 0, 0, 0\n" +
		"GPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d, 74210, 81559, 62, 512.80, 0x0000000000000004, 0, 12, 97, 61\n" +
		"GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee, 412, 24564, 38, [N/A], [Not Supported], [N/A], [N/A], 3, [N/A]\n"
	g, err := ParseTelemetry(out)
	require.NoError(t, err)
	require.Len(t, g, 3)
	assert.Equal(t, int64(1), *g[0].VRAMUsedMiB)
	assert.Equal(t, int64(81559), *g[0].VRAMTotalMiB)
	assert.Equal(t, 31, *g[0].TempC)
	assert.InDelta(t, 69.37, *g[0].PowerW, 1e-9)
	assert.Equal(t, uint64(1), *g[0].ThrottleReasons)
	assert.Equal(t, int64(12), *g[1].ECCCorrected)
	assert.Nil(t, g[2].PowerW)
	assert.Nil(t, g[2].ThrottleReasons)
	assert.Nil(t, g[2].ECCUncorrected)
	assert.Nil(t, g[2].ECCCorrected)
	assert.Equal(t, 97, *g[1].UtilGPU)
	assert.Equal(t, 61, *g[1].UtilMem)
	assert.Nil(t, g[2].UtilMem)
	assert.Equal(t, []int{33, 30}, []int{avgUtil(g, false), avgUtil(g, true)}, "N/A 不计入平均")

	_, err = ParseTelemetry("Field \"clocks_throttle_reasons.active\" is not a valid field to query.\n")
	assert.Error(t, err)
	assert.True(t, isInvalidFieldErr("Field \"clocks_throttle_reasons.active\" is not a valid field to query."))
	_, err = ParseTelemetry("GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11, 90000, 81559, 31, 69.37, 0x0, 0, 0, 0, 0\n")
	assert.Error(t, err, "已用大于总量")
	_, err = ParseTelemetry("GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11, 1, 81559, 31, 69.37, 0x0, 0, 0\n")
	assert.Error(t, err, "列数不足")
	_, err = ParseTelemetry("No devices were found\n")
	assert.Error(t, err)
	_, err = ParseTelemetry("")
	assert.Error(t, err)
}

func TestParseTopoMatrix(t *testing.T) {
	m, err := ParseTopoMatrix(h100Topo, 8)
	require.NoError(t, err)
	require.Len(t, m, 8)
	assert.Equal(t, "X", m[0][0])
	assert.Equal(t, "NV18", m[0][7])
	m, err = ParseTopoMatrix(rtx4090Topo, 4)
	require.NoError(t, err)
	for i := range m {
		for j := range m[i] {
			if i != j {
				assert.False(t, strings.HasPrefix(m[i][j], "NV"))
			}
		}
	}
	_, err = ParseTopoMatrix(h100Topo, 4)
	assert.Error(t, err)
}

func TestParseCacheDF(t *testing.T) {
	// 真实 GNU coreutils 输出(df -B1 --output=avail,size)。
	d, err := ParseCacheDF("      Avail   1B-blocks\n39125975040 62671097856\n")
	require.NoError(t, err)
	assert.Equal(t, int64(39125975040), d.AvailBytes)
	assert.Equal(t, int64(62671097856), d.TotalBytes)
	d, err = ParseCacheDF("@@missing\n")
	require.NoError(t, err)
	assert.True(t, d.Missing)
	_, err = ParseCacheDF("df: unrecognized option '--output=avail,size'\n")
	assert.Error(t, err)

	c, err := cacheDFCommand("/data/omnis-models")
	require.NoError(t, err)
	assert.Equal(t, "if [ -d '/data/omnis-models' ]; then df -B1 --output=avail,size '/data/omnis-models'; else echo @@missing; fi", c)
	for _, bad := range []string{"data", "/data/../etc", "/data/x'; rm -rf /", "/data/$(id)"} {
		_, err := cacheDFCommand(bad)
		assert.Error(t, err, bad)
	}
}

func TestParseHostInfo(t *testing.T) {
	out := "@@nproc\n224\n@@mem\nMemTotal:       2113401588 kB\n@@arch\nx86_64\n@@kernel\n5.15.0-113-generic\n" +
		"@@os\nPRETTY_NAME=\"Ubuntu 22.04.4 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"22.04\"\nID=ubuntu\nID_LIKE=debian\n" +
		"@@df\nFilesystem     1073741824-blocks  Used Available Capacity Mounted on\n/dev/nvme0n1p1             13968G  1022G    12946G       8% /data\n"
	h, err := ParseHostInfo(out)
	require.NoError(t, err)
	assert.Equal(t, 224, h.CPUCores)
	assert.Equal(t, 2015, h.MemGB)
	assert.Equal(t, "ubuntu", h.OSID)
	assert.Equal(t, "22.04", h.OSVersion)
	assert.Equal(t, 12946, h.DiskFreeGB)
	assert.Equal(t, "/data", h.DiskPath)
	assert.Equal(t, "x86_64", h.Arch)

	_, err = ParseHostInfo("@@nproc\nsh: nproc: not found\n")
	assert.Error(t, err)
}

func TestParseDockerInfo(t *testing.T) {
	out := `{"ID":"7c1f","Containers":3,"Driver":"overlay2","DriverStatus":[["Backing Filesystem","xfs"],["Supports d_type","true"],["Using metacopy","false"],["Native Overlay Diff","true"],["userxattr","false"]],"ServerVersion":"24.0.7","DockerRootDir":"/data/docker","CgroupVersion":"2","Runtimes":{"io.containerd.runc.v2":{"path":"runc"},"nvidia":{"path":"nvidia-container-runtime"},"runc":{"path":"runc"}}}`
	d, err := ParseDockerInfo(out)
	require.NoError(t, err)
	assert.Equal(t, "24.0.7", d.ServerVersion)
	assert.Equal(t, "xfs", d.BackingFS())
	assert.True(t, d.HasNvidiaRuntime())
	assert.Equal(t, "/data/docker", d.DockerRootDir)

	_, err = ParseDockerInfo("permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get \"http://%2Fvar%2Frun%2Fdocker.sock/v1.24/info\": dial unix /var/run/docker.sock: connect: permission denied")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}

func TestParseVersionAndCUDA(t *testing.T) {
	c, ok := CompareVersion("535.104.05", "535.104")
	assert.True(t, ok)
	assert.Equal(t, 1, c)
	c, _ = CompareVersion("525.147.05", "535.104")
	assert.Equal(t, -1, c)
	c, _ = CompareVersion("535.104", "535.104.0")
	assert.Equal(t, 0, c)
	_, ok = CompareVersion("535.x", "535")
	assert.False(t, ok)

	hdr := "Tue Oct 28 10:00:00 2026\n+---------------------------------------------------------------------------------------+\n| NVIDIA-SMI 535.104.05             Driver Version: 535.104.05   CUDA Version: 12.2     |\n"
	assert.Equal(t, "12.2", ParseCUDAVersion(hdr))
	assert.Equal(t, "", ParseCUDAVersion("garbage"))
}

func TestParseContainerAndPorts(t *testing.T) {
	l := "GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11)\nGPU 1: NVIDIA H100 80GB HBM3 (UUID: GPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d)\n"
	assert.Equal(t, []string{"GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11", "GPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d"}, ParseGPUListL(l))

	ss := "LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*\nLISTEN 0      4096         0.0.0.0:40000      0.0.0.0:*\nLISTEN 0      4096            [::]:40001         [::]:*\n"
	p := ParseListeningPorts(ss)
	assert.True(t, p[22] && p[40000] && p[40001])
	assert.False(t, p[40002])
}

func TestParseBenchAndThrottle(t *testing.T) {
	out := "Unable to find image 'probe' locally\nsome torch warning\n" +
		`{"results":[{"index":0,"uuid":"GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11","ok":true,"tflops":612.4,"error":""},{"index":1,"uuid":"GPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d","ok":false,"tflops":0,"error":"CUDA error: an illegal memory access was encountered"}]}` + "\n"
	r, err := ParseBench(out)
	require.NoError(t, err)
	require.Len(t, r, 2)
	assert.InDelta(t, 612.4, r[0].TFLOPS, 0.01)
	assert.False(t, r[1].OK)

	_, err = ParseBench("docker: Error response from daemon: could not select device driver \"\" with capabilities: [[gpu]].\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not select device driver")

	th, err := ParseThrottle("@@t0\nGPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11, 0x0000000000000004\nGPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d, 0x0000000000000000\n" +
		"@@t1\nGPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11, 0x0000000000000004\nGPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d, 0x0000000000000040\n")
	require.NoError(t, err)
	assert.Zero(t, th["GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11"]&throttleFailMask, "SW Power Cap 是满载常态, 不判降频")
	assert.NotZero(t, th["GPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d"]&throttleFailMask)
	assert.True(t, strings.Contains(DescribeThrottle(0x40), "硬件温控"))
}
