package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// P4–P8: 只读检查, 不触碰在租资源。
func (p *probeRun) inventory(ctx context.Context) {
	out, err := p.run(ctx, cmdGPUList)
	if err != nil {
		p.set("P4", false, "nvidia-smi 执行失败: "+err.Error())
		return
	}
	gpus, err := ParseGPUList(out)
	if err != nil {
		p.set("P4", false, err.Error())
		return
	}
	uuids := make([]string, 0, len(gpus))
	for _, g := range gpus {
		uuids = append(uuids, g.UUID)
	}
	owners, err := p.s.repo.uuidOwners(p.s.repo.db, p.n.ID, uuids)
	if err != nil {
		p.set("P4", false, "查询卡 UUID 占用失败: "+err.Error())
		return
	}
	if len(owners) > 0 {
		var d []string
		for u, id := range owners {
			d = append(d, fmt.Sprintf("%s 已被节点 #%d 登记", u, id))
		}
		p.set("P4", false, "同一张卡不能重复上架(如为本人机器请删除旧节点; 若被他人抢先登记请提交工单由运营仲裁): "+strings.Join(d, "; "))
		return
	}
	// 宿主机上的 GPU 计算进程: 查不清就不能确认哪些卡可做侵入项(P9 挂卡、P13 压测), 整次探活不通过。
	out, err = p.run(ctx, cmdGPUApps)
	if err != nil {
		p.set("P4", false, "查询 GPU 计算进程失败(无法确认空闲卡未被占用): "+err.Error())
		return
	}
	apps, err := ParseComputeApps(out)
	if err != nil {
		p.set("P4", false, err.Error())
		return
	}
	p.gpus, p.inv.GPUs, p.inv.DriverVersion = gpus, gpus, gpus[0].Driver
	p.set("P4", true, fmt.Sprintf("%d 张 %s, 驱动 %s", len(gpus), gpus[0].Name, gpus[0].Driver))

	// 在租/预留/排空的卡只做只读检查。
	slots, err := p.s.repo.listSlots(p.s.repo.db, p.n.ID)
	if err != nil {
		p.set("P5", false, "读取卡槽失败: "+err.Error())
		return
	}
	busy := map[string]bool{}
	for _, sl := range slots {
		switch sl.State {
		case SlotReserved, SlotLeased, SlotDraining:
			busy[sl.GPUUUID] = true
		}
	}
	// 非平台在租的卡上已有宿主机计算进程(供给方自用或残留): 不做侵入项、不计入可售, 进程退出后下次完整探活自动恢复。
	hostBusy := map[int]string{}
	for _, g := range gpus {
		if busy[g.UUID] {
			continue
		}
		if pids := apps[strings.ToUpper(g.UUID)]; len(pids) > 0 {
			hostBusy[g.Index] = fmt.Sprintf("宿主机计算进程占用(pid %s), 不计入可售, 进程退出后下次完整探活自动恢复", strings.Join(pids, ","))
			continue
		}
		p.freeIdx[g.Index] = true
	}

	if d := CheckConsistency(p.product.GpuModel, p.cat, p.n.TotalCards, gpus); len(d) > 0 {
		p.set("P5", false, strings.Join(d, "; "))
	} else {
		p.set("P5", true, fmt.Sprintf("型号 %s、卡数 %d、显存一致", p.product.GpuModel, len(gpus)))
	}

	if out, err := p.run(ctx, cmdTopo); err != nil {
		p.set("P6", false, "nvidia-smi topo 执行失败: "+err.Error())
	} else if topo, err := ParseTopo(out, len(gpus)); err != nil {
		p.set("P6", false, err.Error())
	} else {
		p.inv.TopoGroups = topo
		p.topoLinks, _ = ParseTopoMatrix(out, len(gpus)) // 与 ParseTopo 同一表头解析, 此处不会失败
		groups := map[string]bool{}
		for _, g := range topo {
			groups[g] = true
		}
		p.set("P6", true, fmt.Sprintf("%d 个拓扑分组", len(groups)))
	}

	// P7 只反映硬件健康; 占用卡在其后并入 Faulty(硬件原因优先)。
	p.health(ctx, 24*time.Hour, "P7")
	if p.inv.Faulty == nil {
		p.inv.Faulty = map[int]string{}
	}
	for idx, why := range hostBusy {
		if p.inv.Faulty[idx] == "" {
			p.inv.Faulty[idx] = why
		}
	}
	p.host(ctx)
	p.modelCache(ctx)
}

// modelCache 模型缓存目录所在文件系统容量(一键部署 F4 输入, 不是检查项, 不影响探活结论)。
func (p *probeRun) modelCache(ctx context.Context) {
	df, note := p.readCacheDF(ctx, 30*time.Second)
	if note != "" {
		slog.Warn("fleet 完整探活读取模型缓存目录容量失败", "node_id", p.n.ID, "detail", note)
	}
	if err := p.s.recordCacheDF(p.n.ID, p.gen, cacheSourceFull, df, note, time.Now()); err != nil {
		slog.Error("fleet 记录模型缓存目录容量失败", "node_id", p.n.ID, "error", err)
	}
}

// health P7: ECC 不可纠正错误、退役页/行重映射、Xid; 有问题的卡记入 inv.Faulty(不计入可售)。
// window 为 Xid 扫描时间窗, 结论写入 checkID(P13 压测后复用为 "-10min" 复扫时不写检查项)。
func (p *probeRun) health(ctx context.Context, window time.Duration, checkID string) {
	if p.inv.Faulty == nil {
		p.inv.Faulty = map[int]string{}
	}
	out, err := p.run(ctx, cmdECC)
	if err != nil {
		p.set(checkID, false, "ECC 查询失败: "+err.Error())
		return
	}
	ecc, err := ParseECC(out)
	if err != nil {
		p.set(checkID, false, err.Error())
		return
	}
	var notes []string
	remap := map[string]RemapRow{}
	if out, err := p.run(ctx, cmdRemap); err != nil {
		notes = append(notes, "行重映射查询不支持(Ampere 之前的卡)")
	} else if remap, err = ParseRemappedRows(out); err != nil {
		notes = append(notes, "行重映射输出无法解析")
		remap = map[string]RemapRow{}
	}
	since, window := xidSince(window)
	cmd, _ := xidCommand(since)
	xout, _ := p.run(ctx, cmd) // 以解析结果为准(错误输出带 @@err 前缀)
	xidAt := time.Now()
	xid := ParseXid(xout, window)
	if err := p.s.recordXidScan(p.n.ID, p.gen, xid, xidAt); err != nil {
		slog.Error("fleet 记录 Xid 扫描失败", "node_id", p.n.ID, "error", err)
	}
	if !xid.Known {
		notes = append(notes, "Xid 未知: "+xid.Note)
	}
	for _, g := range p.gpus {
		var why []string
		if h := ecc[g.Index]; h != nil {
			if h.UncorrectedECC > 0 {
				why = append(why, fmt.Sprintf("不可纠正 ECC 错误 %d 次", h.UncorrectedECC))
			}
			if h.RetiredPending {
				why = append(why, "退役页待生效(需重置 GPU)")
			}
		}
		if r, ok := remap[g.UUID]; ok {
			if r.Failure {
				why = append(why, "行重映射失败")
			} else if r.Pending {
				why = append(why, "行重映射待生效(需重置 GPU)")
			}
		}
		if x := xid.FatalXidsFor(g.PCIBusID); len(x) > 0 {
			why = append(why, fmt.Sprintf("近期致命 Xid %v", x))
		}
		if len(why) > 0 {
			p.inv.Faulty[g.Index] = strings.Join(why, "、")
			delete(p.freeIdx, g.Index)
		}
	}
	if checkID == "" {
		return
	}
	detail := fmt.Sprintf("%d 张卡健康", len(p.gpus)-len(p.inv.Faulty))
	for _, i := range sortedKeys(p.inv.Faulty) {
		detail += fmt.Sprintf("; GPU%d 标记故障: %s", i, p.inv.Faulty[i])
	}
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	p.set(checkID, len(p.inv.Faulty) < len(p.gpus), detail)
}

var dockerVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*`)

// host P8 主机资源与驱动版本; 顺带读取 docker info(P9 复用), 以 Docker 数据目录所在盘作为数据盘。
func (p *probeRun) host(ctx context.Context) {
	dockerRoot := "/var/lib/docker"
	out, derr := p.run(ctx, cmdDocker)
	if derr == nil {
		p.docker, derr = ParseDockerInfo(out)
	} else {
		_, derr = ParseDockerInfo(out + "\n" + derr.Error())
	}
	if derr == nil && p.docker.DockerRootDir != "" {
		dockerRoot = p.docker.DockerRootDir
	}
	if derr != nil {
		p.set("P9", false, derr.Error())
	}
	if out, err := p.run(ctx, cmdSMIHeader); err == nil {
		p.inv.CUDAVersion = ParseCUDAVersion(out)
	}
	cmd, err := hostInfoCommand(dockerRoot)
	if err != nil {
		p.set("P8", false, err.Error())
		return
	}
	out, err = p.run(ctx, cmd)
	if err != nil {
		p.set("P8", false, "读取主机资源失败: "+err.Error())
		return
	}
	h, err := ParseHostInfo(out)
	if err != nil {
		p.set("P8", false, err.Error())
		return
	}
	p.inv.CPUCores, p.inv.MemGB, p.inv.DiskFreeGB, p.inv.DiskPath = h.CPUCores, h.MemGB, h.DiskFreeGB, h.DiskPath
	p.inv.OS, p.inv.Kernel = strings.TrimSpace(h.OSPretty), h.Kernel
	req := ResourceReq{Cards: p.n.TotalCards, ReserveCPU: p.s.cfg.HostReserveCPU, ReserveMemGB: p.s.cfg.HostReserveMemGB}
	if p.n.DiskPerCardGB != nil {
		req.DiskPerCardGB = *p.n.DiskPerCardGB
	}
	diffs := CheckResources(h, req)
	if c, ok := CompareVersion(p.inv.DriverVersion, p.s.cfg.MinDriverVersion); !ok {
		diffs = append(diffs, fmt.Sprintf("驱动版本 %q 无法识别", p.inv.DriverVersion))
	} else if c < 0 {
		diffs = append(diffs, fmt.Sprintf("驱动版本 %s 低于平台要求 %s", p.inv.DriverVersion, p.s.cfg.MinDriverVersion))
	}
	if len(diffs) > 0 {
		p.set("P8", false, strings.Join(diffs, "; "))
		return
	}
	p.set("P8", true, fmt.Sprintf("%s / %s, %d 核, %dGB 内存, %s 可用 %dGB", h.OSPretty, h.Kernel, h.CPUCores, h.MemGB, h.DiskPath, h.DiskFreeGB))
}

// runtime P9 容器运行时 + P10 磁盘配额能力。探针容器只挂空闲卡。
func (p *probeRun) runtime(ctx context.Context) {
	if c := p.checks["P9"]; !c.Skipped {
		return // docker info 已失败
	}
	if p.docker.ServerVersion == "" {
		p.set("P9", false, "未能读取 docker info")
		return
	}
	v := dockerVersionRe.FindString(p.docker.ServerVersion)
	if c, ok := CompareVersion(v, "24"); !ok || c < 0 {
		p.set("P9", false, fmt.Sprintf("Docker 版本 %s 低于要求 24", p.docker.ServerVersion))
		return
	}
	image := p.s.cfg.ProbeImage
	cmd, err := pullCommand(image)
	if err != nil {
		p.set("P9", false, "平台探针镜像配置非法: "+err.Error())
		return
	}
	if _, err := p.runFor(ctx, cmd, pullTimeout); err != nil {
		p.set("P9", false, "拉取平台探针镜像失败: "+err.Error())
		return
	}
	free := p.freeUUIDs()
	cmd, err = runtimeCheckCommand(image, free)
	if err != nil {
		p.set("P9", false, err.Error())
		return
	}
	out, err := p.run(ctx, cmd)
	if err != nil {
		p.set("P9", false, "docker run --gpus 失败(检查 nvidia-container-toolkit): "+err.Error())
		return
	}
	if len(free) > 0 {
		seen := ParseGPUListL(out)
		if len(seen) != len(free) {
			p.set("P9", false, fmt.Sprintf("探针容器内可见 %d 张卡, 期望 %d 张", len(seen), len(free)))
			return
		}
	}
	p.set("P9", true, fmt.Sprintf("Docker %s, 存储驱动 %s(%s), 探针容器可见 %d 张空闲卡",
		p.docker.ServerVersion, p.docker.Driver, p.docker.BackingFS(), len(free)))

	cmd, _ = quotaCheckCommand(image)
	out, err = p.run(ctx, cmd)
	drv := fmt.Sprintf("存储驱动 %s, 底层 %s; 需 overlay2 + xfs pquota", p.docker.Driver, p.docker.BackingFS())
	switch {
	case err != nil:
		p.set("P10", false, fmt.Sprintf("--storage-opt size= 不可用(%s), 该节点只能整机出租: %v", drv, err))
	case strings.Contains(out, "@@enforced"):
		p.inv.QuotaOK = true
		p.set("P10", true, "支持按容器限制磁盘(64M 配额内写 128M 被拒)")
	case strings.Contains(out, "@@not-enforced"):
		p.set("P10", false, fmt.Sprintf("--storage-opt size= 被接受但未生效(64M 配额内写入 128M 成功; %s), 该节点只能整机出租", drv))
	default:
		p.set("P10", false, fmt.Sprintf("磁盘配额自检未得出结论(探针容器内写入失败或输出异常: %q), 该节点只能整机出租", truncate(strings.TrimSpace(out), 160)))
	}
}

// freeUUIDs 可做侵入项的空闲健康卡 UUID(按 index 升序)。
func (p *probeRun) freeUUIDs() []string {
	var out []string
	for _, g := range p.gpus {
		if p.freeIdx[g.Index] {
			out = append(out, g.UUID)
		}
	}
	return out
}
