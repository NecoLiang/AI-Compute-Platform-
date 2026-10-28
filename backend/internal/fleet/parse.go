package fleet

// 探活命令输出解析。输入全部来自供给方宿主机(不可信): 解析失败一律返回错误或"未知",
// 绝不猜测补值; 上层把解析错误记为该检查项不通过。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GPU nvidia-smi --query-gpu 的一行。
type GPU struct {
	Index     int    `json:"index"`
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	MemoryMiB int    `json:"memory_mib"`
	PCIBusID  string `json:"pci_bus_id"`
	Driver    string `json:"driver_version"`
}

var uuidRe = regexp.MustCompile(`^GPU-[0-9a-fA-F-]{32,40}$`)

// splitCSV nvidia-smi 的 CSV 不带引号, 字段内不含逗号(name 不含逗号)。
func splitCSV(line string) []string {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func nonEmptyLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// parseMiB 接受 "81559" 或 "81559 MiB"(csv 不带 nounits 时)。
func parseMiB(s string) (int, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "MiB"))
	return strconv.Atoi(s)
}

// ParseGPUList 解析 P4: nvidia-smi --query-gpu=index,uuid,name,memory.total,pci.bus_id,driver_version
// --format=csv,noheader[,nounits]。index 必须连续唯一, uuid 必须形如 GPU-xxxxxxxx-…。
func ParseGPUList(out string) ([]GPU, error) {
	var gpus []GPU
	seenIdx, seenUUID := map[int]bool{}, map[string]bool{}
	for _, line := range nonEmptyLines(out) {
		f := splitCSV(line)
		if len(f) != 6 {
			return nil, fmt.Errorf("nvidia-smi 输出格式异常(期望 6 列): %q", truncate(line, 120))
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("GPU index 非数字: %q", f[0])
		}
		if !uuidRe.MatchString(f[1]) {
			return nil, fmt.Errorf("GPU UUID 格式异常: %q", truncate(f[1], 60))
		}
		mem, err := parseMiB(f[3])
		if err != nil || mem <= 0 {
			return nil, fmt.Errorf("显存容量无法解析: %q", f[3])
		}
		if seenIdx[idx] || seenUUID[f[1]] {
			return nil, fmt.Errorf("GPU index/UUID 重复: %d %s", idx, f[1])
		}
		seenIdx[idx], seenUUID[f[1]] = true, true
		gpus = append(gpus, GPU{Index: idx, UUID: f[1], Name: f[2], MemoryMiB: mem, PCIBusID: f[4], Driver: f[5]})
	}
	if len(gpus) == 0 {
		return nil, fmt.Errorf("未发现 GPU(nvidia-smi 无输出)")
	}
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].Index < gpus[j].Index })
	for i, g := range gpus {
		if g.Index != i {
			return nil, fmt.Errorf("GPU index 不连续: 缺少 %d", i)
		}
	}
	return gpus, nil
}

// ParseCUDAVersion 从 nvidia-smi 默认输出表头取 "CUDA Version: 12.2"。
func ParseCUDAVersion(out string) string {
	m := regexp.MustCompile(`CUDA Version:\s*([0-9]+\.[0-9]+)`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// CompareVersion 比较点分数字版本("535.104.05" vs "535.104"), 缺省段按 0。
// 任一段非数字返回 ok=false。
func CompareVersion(a, b string) (cmp int, ok bool) {
	pa, pb := strings.Split(strings.TrimSpace(a), "."), strings.Split(strings.TrimSpace(b), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		var err error
		if i < len(pa) {
			if x, err = strconv.Atoi(pa[i]); err != nil {
				return 0, false
			}
		}
		if i < len(pb) {
			if y, err = strconv.Atoi(pb[i]); err != nil {
				return 0, false
			}
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// ParseTopo 解析 P6: nvidia-smi topo -m 矩阵, 输出 index→topo_group。
//
// 分组规则(确定性): 先按 NV#(NVLink/NVSwitch)、PIX/PXB(同 PCIe 交换)连边求连通分量;
// 仍是孤立的卡再按 PHB/NODE(同 CPU/同 NUMA)连边。标签形如 g0-nvlink / g1-pcie / g2-numa / g3-solo,
// 编号按组内最小 index 升序, 长度 ≤ 16(gpu_slots.topo_group VARCHAR(16))。
func ParseTopo(out string, gpuCount int) (map[int]string, error) {
	lines := strings.Split(ansiRe.ReplaceAllString(out, ""), "\n")
	header := -1
	var cols []string
	for i, l := range lines {
		// 表头行以空白开头、第一个字段是 GPU0; 数据行以 "GPU0\t X" 顶格开头。
		f := strings.Fields(l)
		if len(f) >= 1 && f[0] == "GPU0" && (l[0] == '\t' || l[0] == ' ') {
			header, cols = i, f
			break
		}
	}
	if header < 0 {
		return nil, fmt.Errorf("topo 输出缺少表头")
	}
	ngpu := 0
	for _, c := range cols {
		if strings.HasPrefix(c, "GPU") {
			if _, err := strconv.Atoi(c[3:]); err == nil {
				ngpu++
				continue
			}
		}
		break
	}
	if ngpu != gpuCount {
		return nil, fmt.Errorf("topo 矩阵 GPU 数 %d 与清单 %d 不一致", ngpu, gpuCount)
	}
	link := make([][]string, ngpu)
	for r := 0; r < ngpu; r++ {
		row := -1
		for i := header + 1; i < len(lines); i++ {
			f := strings.Fields(lines[i])
			if len(f) > 0 && f[0] == fmt.Sprintf("GPU%d", r) {
				row = i
				break
			}
		}
		if row < 0 {
			return nil, fmt.Errorf("topo 缺少 GPU%d 行", r)
		}
		f := strings.Fields(lines[row])
		if len(f) < ngpu+1 {
			return nil, fmt.Errorf("topo GPU%d 行列数不足", r)
		}
		link[r] = f[1 : ngpu+1]
	}
	parent := make([]int, ngpu)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}
	kind := make([]string, ngpu)
	for i := 0; i < ngpu; i++ {
		for j := 0; j < ngpu; j++ {
			if i == j {
				continue
			}
			v := link[i][j]
			switch {
			case strings.HasPrefix(v, "NV"):
				union(i, j)
				kind[i] = "nvlink"
			case v == "PIX" || v == "PXB":
				union(i, j)
				if kind[i] == "" {
					kind[i] = "pcie"
				}
			}
		}
	}
	for i := 0; i < ngpu; i++ {
		if kind[i] != "" {
			continue
		}
		for j := 0; j < ngpu; j++ {
			if i != j && kind[j] == "" && (link[i][j] == "PHB" || link[i][j] == "NODE") {
				union(i, j)
				kind[i] = "numa"
			}
		}
	}
	// 组的类型取组内最强连接。
	rank := map[string]int{"nvlink": 3, "pcie": 2, "numa": 1, "": 0}
	groupKind, groupNo := map[int]string{}, map[int]int{}
	for i := 0; i < ngpu; i++ {
		r := find(i)
		if rank[kind[i]] > rank[groupKind[r]] {
			groupKind[r] = kind[i]
		}
	}
	res, next := map[int]string{}, 0
	for i := 0; i < ngpu; i++ {
		r := find(i)
		if _, ok := groupNo[r]; !ok {
			groupNo[r] = next
			next++
		}
		k := groupKind[r]
		if k == "" {
			k = "solo"
		}
		res[i] = fmt.Sprintf("g%d-%s", groupNo[r], k)
	}
	return res, nil
}

// truncate 按字符截断(不切断多字节字符)。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ===== P7 健康 =====

// GPUHealth 单卡健康读数。
type GPUHealth struct {
	Index          int    `json:"index"`
	UncorrectedECC int    `json:"uncorrected_ecc"` // -1 = 不支持/未知
	RetiredPending bool   `json:"retired_pages_pending"`
	RemapFailure   bool   `json:"remapped_rows_failure"`
	RemapPending   bool   `json:"remapped_rows_pending"`
	Note           string `json:"note,omitempty"`
}

func naValue(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.HasPrefix(s, "[") || strings.EqualFold(s, "N/A")
}

// yesValue 兼容 "Yes"/"No" 与 "1"/"0"。
func yesValue(s string) bool {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "yes") {
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}

// ParseECC 解析 nvidia-smi --query-gpu=index,ecc.errors.uncorrected.volatile.total,retired_pages.pending
// --format=csv,noheader,nounits。不支持 ECC 的卡(消费卡)记 -1, 不判故障。
func ParseECC(out string) (map[int]*GPUHealth, error) {
	res := map[int]*GPUHealth{}
	for _, line := range nonEmptyLines(out) {
		f := splitCSV(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("ECC 输出格式异常: %q", truncate(line, 120))
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("ECC 输出 index 非数字: %q", f[0])
		}
		h := &GPUHealth{Index: idx, UncorrectedECC: -1}
		if !naValue(f[1]) {
			n, err := strconv.Atoi(f[1])
			if err != nil {
				return nil, fmt.Errorf("ECC 计数无法解析: %q", f[1])
			}
			h.UncorrectedECC = n
		}
		h.RetiredPending = !naValue(f[2]) && yesValue(f[2])
		res[idx] = h
	}
	return res, nil
}

// RemapRow nvidia-smi --query-remapped-rows=gpu_uuid,remapped_rows.pending,remapped_rows.failure 的一行。
type RemapRow struct {
	Pending bool
	Failure bool
}

// ParseRemappedRows 按 uuid 返回; Ampere 之前的卡不支持该查询, 上层把命令失败记为"不支持"。
func ParseRemappedRows(out string) (map[string]RemapRow, error) {
	res := map[string]RemapRow{}
	for _, line := range nonEmptyLines(out) {
		f := splitCSV(line)
		if len(f) != 3 || !uuidRe.MatchString(f[0]) {
			return nil, fmt.Errorf("remapped-rows 输出格式异常: %q", truncate(line, 120))
		}
		res[f[0]] = RemapRow{Pending: yesValue(f[1]), Failure: yesValue(f[2])}
	}
	return res, nil
}

// FatalXids 视为卡级致命的 Xid(NVIDIA Xid 文档: DBE/掉卡/GSP/NVLink 等)。
var FatalXids = map[int]bool{48: true, 61: true, 62: true, 64: true, 74: true, 79: true, 95: true, 119: true, 120: true}

// XidScan Xid 扫描结果。Known=false 表示无权限读内核日志, 报告记"未知"(docs/25 P7)。
type XidScan struct {
	Known  bool             `json:"known"`
	Note   string           `json:"note,omitempty"`
	ByBus  map[string][]int `json:"by_bus,omitempty"` // 归一化 PCI 地址 → Xid 列表
	Events int              `json:"events"`
}

var xidRe = regexp.MustCompile(`NVRM: Xid \(PCI:([0-9a-fA-F]{4,8}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2})(?:\.[0-9a-fA-F])?\):\s*(\d+)`)

// NormalizeBusID 统一 "00000000:18:00.0" / "0000:18:00" 为 "0000:18:00"。
func NormalizeBusID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "."); i > 0 && i >= len(s)-2 {
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return s
	}
	dom := parts[0]
	if len(dom) > 4 {
		dom = dom[len(dom)-4:]
	}
	return dom + ":" + parts[1] + ":" + parts[2]
}

// ParseXid 解析 xidCommand 的输出: 首行 "@@id uid=<n> groups=<g …>", 之后是 journalctl 过滤结果。
// 非 root 且不在 adm/systemd-journal 组, 或 journalctl 提示看不到他人消息时, 结果为"未知"。
func ParseXid(out string) XidScan {
	scan := XidScan{ByBus: map[string][]int{}}
	lines := strings.Split(out, "\n")
	privileged := false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "@@id ") {
			for _, f := range strings.Fields(l[5:]) {
				if f == "uid=0" {
					privileged = true
				}
				g := strings.TrimPrefix(f, "groups=")
				if g == "adm" || g == "systemd-journal" || g == "root" {
					privileged = true
				}
			}
			continue
		}
		if m := xidRe.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			bus := NormalizeBusID(m[1])
			scan.ByBus[bus] = append(scan.ByBus[bus], n)
			scan.Events++
			continue
		}
		if strings.Contains(l, "not found") {
			return XidScan{Known: false, Note: "节点无 journalctl, Xid 记为未知"}
		}
		if strings.Contains(l, "not seeing messages") || strings.Contains(l, "No journal files") ||
			strings.Contains(l, "ermission denied") {
			return XidScan{Known: false, Note: "无权限读取内核日志(需 root 或 adm/systemd-journal 组), Xid 记为未知"}
		}
	}
	if !privileged {
		return XidScan{Known: false, Note: "管理账户非 root 且不在 adm/systemd-journal 组, 内核日志不可读, Xid 记为未知"}
	}
	scan.Known = true
	return scan
}

// FatalXidsFor 返回该卡在扫描窗口内出现的致命 Xid。
func (x XidScan) FatalXidsFor(pciBusID string) []int {
	var res []int
	for _, n := range x.ByBus[NormalizeBusID(pciBusID)] {
		if FatalXids[n] {
			res = append(res, n)
		}
	}
	return res
}

// ===== P8 主机资源 =====

// HostInfo P8 主机资源。
type HostInfo struct {
	Arch       string `json:"arch"`
	Kernel     string `json:"kernel"`
	OSID       string `json:"os_id"`
	OSVersion  string `json:"os_version"`
	OSPretty   string `json:"os_pretty"`
	CPUCores   int    `json:"cpu_cores"`
	MemGB      int    `json:"mem_gb"`
	DiskFreeGB int    `json:"disk_free_gb"`
	DiskPath   string `json:"disk_path"`
}

// sections 把 "@@name" 分隔的输出切成段。
func sections(out string) map[string]string {
	res, cur := map[string]string{}, ""
	var b strings.Builder
	flush := func() {
		if cur != "" {
			res[cur] = b.String()
		}
		b.Reset()
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "@@") {
			flush()
			cur = strings.Fields(l[2:] + " ")[0]
			continue
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	flush()
	return res
}

// ParseHostInfo 解析 hostInfoCommand 的输出(nproc / MemTotal / uname / os-release / df -P -B1G)。
func ParseHostInfo(out string) (HostInfo, error) {
	s := sections(out)
	var h HostInfo
	var err error
	if h.CPUCores, err = strconv.Atoi(strings.TrimSpace(s["nproc"])); err != nil || h.CPUCores <= 0 {
		return h, fmt.Errorf("CPU 核数无法解析")
	}
	mem := strings.Fields(s["mem"])
	if len(mem) < 2 || mem[0] != "MemTotal:" {
		return h, fmt.Errorf("内存容量无法解析")
	}
	kb, err := strconv.ParseInt(mem[1], 10, 64)
	if err != nil || kb <= 0 {
		return h, fmt.Errorf("内存容量无法解析")
	}
	h.MemGB = int(kb / 1024 / 1024)
	h.Arch = strings.TrimSpace(s["arch"])
	h.Kernel = strings.TrimSpace(s["kernel"])
	for _, l := range strings.Split(s["os"], "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			h.OSID = v
		case "VERSION_ID":
			h.OSVersion = v
		case "PRETTY_NAME":
			h.OSPretty = v
		}
	}
	df := nonEmptyLines(s["df"])
	if len(df) < 2 {
		return h, fmt.Errorf("数据盘容量无法解析(df 无输出)")
	}
	f := strings.Fields(df[len(df)-1])
	if len(f) < 6 {
		return h, fmt.Errorf("数据盘容量无法解析: %q", truncate(df[len(df)-1], 80))
	}
	avail := strings.TrimSuffix(f[3], "G")
	if h.DiskFreeGB, err = strconv.Atoi(avail); err != nil {
		return h, fmt.Errorf("数据盘可用容量无法解析: %q", f[3])
	}
	h.DiskPath = f[5]
	return h, nil
}

// ===== P9 容器运行时 =====

// DockerInfo docker info --format '{{json .}}' 中用到的字段。
type DockerInfo struct {
	ServerVersion string                     `json:"ServerVersion"`
	DockerRootDir string                     `json:"DockerRootDir"`
	Driver        string                     `json:"Driver"`
	DriverStatus  [][]string                 `json:"DriverStatus"`
	Runtimes      map[string]json.RawMessage `json:"Runtimes"`
	CgroupVersion string                     `json:"CgroupVersion"`
}

// BackingFS overlay2 的底层文件系统(Backing Filesystem)。
func (d DockerInfo) BackingFS() string {
	for _, kv := range d.DriverStatus {
		if len(kv) == 2 && kv[0] == "Backing Filesystem" {
			return kv[1]
		}
	}
	return ""
}

// HasNvidiaRuntime nvidia-container-toolkit 注册的 runtime, 或 CDI 方式(--gpus 仍可用, 以 P9 实跑为准)。
func (d DockerInfo) HasNvidiaRuntime() bool {
	_, ok := d.Runtimes["nvidia"]
	return ok
}

// ParseDockerInfo 只接受 JSON 输出; 权限不足时 docker 输出错误文本, 原样报给供给方。
func ParseDockerInfo(out string) (DockerInfo, error) {
	var d DockerInfo
	out = strings.TrimSpace(out)
	if !strings.HasPrefix(out, "{") {
		return d, fmt.Errorf("docker info 失败: %s", truncate(out, 200))
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return d, fmt.Errorf("docker info 输出无法解析: %v", err)
	}
	if d.ServerVersion == "" {
		return d, fmt.Errorf("docker 守护进程不可用(无 ServerVersion), 请确认管理账户在 docker 组")
	}
	return d, nil
}

// ParseGPUListL 解析容器内 nvidia-smi -L, 返回 UUID 列表。
func ParseGPUListL(out string) []string {
	var res []string
	re := regexp.MustCompile(`^GPU \d+: .*\(UUID: (GPU-[0-9a-fA-F-]+)\)`)
	for _, l := range nonEmptyLines(out) {
		if m := re.FindStringSubmatch(l); m != nil {
			res = append(res, m[1])
		}
	}
	return res
}

// ===== P11/P12 端口 =====

// ParseListeningPorts 解析 ss -Htln 的本地监听端口。
func ParseListeningPorts(out string) map[int]bool {
	res := map[int]bool{}
	for _, l := range nonEmptyLines(out) {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		addr := f[3]
		i := strings.LastIndex(addr, ":")
		if i < 0 {
			continue
		}
		if p, err := strconv.Atoi(addr[i+1:]); err == nil {
			res[p] = true
		}
	}
	return res
}

// ===== P13 短压测 =====

// BenchResult 探针镜像内单卡矩阵乘结果。
type BenchResult struct {
	Index  int     `json:"index"`
	UUID   string  `json:"uuid"`
	OK     bool    `json:"ok"`
	TFLOPS float64 `json:"tflops"`
	Error  string  `json:"error"`
}

// ParseBench 取输出中最后一个 {"results":[…]} 行; 容器内其他日志忽略。
func ParseBench(out string) ([]BenchResult, error) {
	lines := nonEmptyLines(out)
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], `{"results"`) {
			continue
		}
		var r struct {
			Results []BenchResult `json:"results"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &r); err != nil {
			return nil, fmt.Errorf("压测结果无法解析: %v", err)
		}
		return r.Results, nil
	}
	return nil, fmt.Errorf("压测无结果输出: %s", truncate(strings.Join(lastN(lines, 3), " | "), 300))
}

func lastN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// 降频原因位(NVML clocksThrottleReasons)。
const (
	throttleSwPowerCap          = 0x4
	throttleHwSlowdown          = 0x8
	throttleSwThermal           = 0x20
	throttleHwThermal           = 0x40
	throttleHwPowerBrake        = 0x80
	throttleFailMask     uint64 = throttleHwSlowdown | throttleSwThermal | throttleHwThermal | throttleHwPowerBrake
)

// ParseThrottle 解析采样命令输出(多轮 "uuid, 0x…"), 返回每卡各轮按位或的结果。
// "[Not Supported]" 记 0。
func ParseThrottle(out string) (map[string]uint64, error) {
	res := map[string]uint64{}
	for _, l := range nonEmptyLines(out) {
		if strings.HasPrefix(l, "@@") {
			continue
		}
		f := splitCSV(l)
		if len(f) != 2 || !uuidRe.MatchString(f[0]) {
			return nil, fmt.Errorf("降频采样输出格式异常: %q", truncate(l, 120))
		}
		if naValue(f[1]) {
			res[f[0]] |= 0
			continue
		}
		v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(f[1]), "0x"), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("降频原因无法解析: %q", f[1])
		}
		res[f[0]] |= v
	}
	return res, nil
}

// DescribeThrottle 把失败位翻译成中文。
func DescribeThrottle(bits uint64) string {
	var s []string
	if bits&throttleHwSlowdown != 0 {
		s = append(s, "硬件降频(HW Slowdown)")
	}
	if bits&throttleSwThermal != 0 {
		s = append(s, "软件温控降频")
	}
	if bits&throttleHwThermal != 0 {
		s = append(s, "硬件温控降频")
	}
	if bits&throttleHwPowerBrake != 0 {
		s = append(s, "外部功率刹车降频(Power Brake)")
	}
	return strings.Join(s, "、")
}
