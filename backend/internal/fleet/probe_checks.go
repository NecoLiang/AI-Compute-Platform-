package fleet

import (
	"context"
	"fmt"
	"log/slog"
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
	// 宿主机上的 GPU 计算进程: 查不清就不能确认哪些卡空闲可售, 整次探活不通过。
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
	// 非平台在租的卡上已有宿主机计算进程(供给方自用或残留): 不计入可售, 进程退出后下次完整探活自动恢复。
	hostBusy := map[int]string{}
	for _, g := range gpus {
		if busy[g.UUID] {
			continue
		}
		if pids := apps[strings.ToUpper(g.UUID)]; len(pids) > 0 {
			hostBusy[g.Index] = fmt.Sprintf("宿主机计算进程占用(pid %s), 不计入可售, 进程退出后下次完整探活自动恢复", strings.Join(pids, ","))
		}
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
	p.health(ctx)
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

// health P7: ECC 不可纠正错误、退役页/行重映射、近 24h 的 Xid; 有问题的卡记入 inv.Faulty(不计入可售)。
func (p *probeRun) health(ctx context.Context) {
	if p.inv.Faulty == nil {
		p.inv.Faulty = map[int]string{}
	}
	out, err := p.run(ctx, cmdECC)
	if err != nil {
		p.set("P7", false, "ECC 查询失败: "+err.Error())
		return
	}
	ecc, err := ParseECC(out)
	if err != nil {
		p.set("P7", false, err.Error())
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
	since, window := xidSince(24 * time.Hour)
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
		}
	}
	detail := fmt.Sprintf("%d 张卡健康", len(p.gpus)-len(p.inv.Faulty))
	for _, i := range sortedKeys(p.inv.Faulty) {
		detail += fmt.Sprintf("; GPU%d 标记故障: %s", i, p.inv.Faulty[i])
	}
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	p.set("P7", len(p.inv.Faulty) < len(p.gpus), detail)
}

// host P8 主机资源与驱动版本; 顺带读取 docker info(记录版本), 以 Docker 数据目录所在盘作为数据盘。
// docker info 读不到不判 P8 不通过(容器运行时不再检查), 只在详情中注明。
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
	detail := fmt.Sprintf("%s / %s, %d 核, %dGB 内存, %s 可用 %dGB", h.OSPretty, h.Kernel, h.CPUCores, h.MemGB, h.DiskPath, h.DiskFreeGB)
	if derr != nil {
		detail += "; 未读取到 docker info: " + truncate(derr.Error(), 200)
	} else {
		detail += "; Docker " + p.docker.ServerVersion
	}
	p.set("P8", true, detail)
}
