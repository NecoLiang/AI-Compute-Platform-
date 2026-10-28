package fleet

import (
	"strings"
	"testing"

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

func TestParseXid(t *testing.T) {
	out := "@@id uid=0 groups=root\n" +
		"2026-10-28T09:12:01+0800 gpu-node-01 kernel: NVRM: Xid (PCI:0000:9a:00): 79, pid=0, name=, GPU has fallen off the bus.\n" +
		"2026-10-28T09:20:44+0800 gpu-node-01 kernel: NVRM: Xid (PCI:0000:18:00): 13, pid=48211, name=python3, Graphics SM Warp Exception on (GPC 1, TPC 0, SM 0)\n"
	x := ParseXid(out)
	require.True(t, x.Known)
	assert.Equal(t, 2, x.Events)
	assert.Equal(t, []int{79}, x.FatalXidsFor("00000000:9A:00.0"))
	assert.Empty(t, x.FatalXidsFor("00000000:18:00.0"), "Xid 13 是应用错误, 不判卡故障")

	clean := ParseXid("@@id uid=1001 groups=omnis docker adm\n")
	assert.True(t, clean.Known)
	assert.Zero(t, clean.Events)

	noPerm := ParseXid("@@id uid=1001 groups=omnis docker\nHint: You are currently not seeing messages from other users and the system.\n      Users in groups 'adm', 'systemd-journal' can see all messages.\n")
	assert.False(t, noPerm.Known)
	assert.Contains(t, noPerm.Note, "未知")

	unprivileged := ParseXid("@@id uid=1001 groups=omnis docker\n")
	assert.False(t, unprivileged.Known)
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

func TestParseXidMissingJournalctl(t *testing.T) {
	x := ParseXid("@@id uid=0 groups=root\nsh: 1: journalctl: not found\n")
	assert.False(t, x.Known)
	assert.Contains(t, x.Note, "journalctl")
}
