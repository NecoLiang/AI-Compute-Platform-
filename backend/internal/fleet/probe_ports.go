package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ports P11/P12: 在租约端口段的空闲端口起临时监听, 网关自检必须能连上(P11), 主服务出口必须连不上(P12)。
func (p *probeRun) ports(ctx context.Context) {
	image := p.s.cfg.ProbeImage
	if p.mode == ProbePortRecheck {
		cmd, err := pullCommand(image)
		if err != nil {
			p.portsFail("平台探针镜像配置非法: " + err.Error())
			return
		}
		if _, err := p.runFor(ctx, cmd, pullTimeout); err != nil {
			p.portsFail("拉取平台探针镜像失败: " + err.Error())
			return
		}
	}
	out, err := p.run(ctx, cmdPorts)
	if err != nil {
		p.portsFail("读取本机监听端口失败: " + err.Error())
		return
	}
	listening := ParseListeningPorts(out)
	leased, err := p.s.repo.leasedPorts(p.s.repo.db, p.n.ID)
	if err != nil {
		p.portsFail("读取租约端口占用失败: " + err.Error())
		return
	}
	start, end := portRange(p.n)
	port := 0
	// 自检端口从端口段末尾向下选取, 与租约端口分配(从段首开始)错开; 已被租约占用的端口(含容器暂停时未监听的)一律跳过。
	for i := end; i >= start; i-- {
		if !listening[i] && !leased[i] {
			port = i
			break
		}
	}
	if port == 0 {
		p.portsFail(fmt.Sprintf("租约端口段 %d-%d 内没有空闲端口", start, end))
		return
	}
	name := fmt.Sprintf("omnis-probe-%d-%s", p.n.ID, randomSuffix())
	cmd, err := portListenerCommand(image, name, port)
	if err != nil {
		p.portsFail(err.Error())
		return
	}
	defer p.cleanup(name)
	out, err = p.run(ctx, cmd)
	if err != nil {
		p.portsFail(fmt.Sprintf("端口 %d 上的临时监听容器未能启动: %s", port, errText(err)))
		return
	}
	if !strings.Contains(out, "@@up") {
		p.portsFail(fmt.Sprintf("端口 %d 上的临时监听容器已创建, 但 15 秒内宿主机本地未监听(容器内监听进程未起: 检查探针镜像是否含 python3)", port))
		return
	}
	host := *p.n.SSHHost

	// 交给网关的是主服务按目标地址策略解析并校验过的 IP 字面量, 网关不再自行解析(防 DNS rebinding)。
	target, rerr := p.s.dialer.resolveAllowed(ctx, host)
	switch {
	case rerr != nil:
		slog.Warn("探活 P11: 目标地址解析/校验失败", "node_id", p.n.ID, "host", host, "error", rerr)
		p.set("P11", false, "管理端口不可达")
	case p.s.gateway == nil:
		p.set("P11", false, "访问网关(cmd/accessgw, docs/25 S2 T-085)尚未接入, 无法经网关做自检登录; 该项接入前不能判定通过")
	default:
		if err := p.s.gateway.SelfTestPort(ctx, p.n.ID, target.String(), port); err != nil {
			slog.Warn("探活 P11: 经网关连接租约端口失败", "node_id", p.n.ID, "target", target.String(), "port", port, "error", err)
			p.set("P11", false, fmt.Sprintf("经网关连接 %s:%d 失败(检查是否已按上架说明放行网关出口 IP)", host, port))
		} else {
			p.set("P11", true, fmt.Sprintf("经网关连接 %s:%d 成功", host, port))
		}
	}

	d := p.s.dialer.with(5*time.Second, 1)
	conn, err := d.TCPCheck(ctx, host, port)
	switch {
	case err == nil:
		_ = conn.Close()
		p.set("P12", false, fmt.Sprintf("从平台主服务出口可以直接连上 %s:%d, 说明租约端口段未限制来源; 请按上架说明只放行网关出口 IP", host, port))
	case errors.Is(err, ErrAddrNotAllowed):
		slog.Warn("探活 P12: 目标地址命中拒绝名单", "node_id", p.n.ID, "error", err)
		p.set("P12", false, "管理端口不可达")
	default:
		p.set("P12", true, fmt.Sprintf("从平台主服务出口连接 %s:%d 失败(符合预期)。注: 只能证明主服务出口连不上, 不能证明对全网关闭", host, port))
	}
}

func (p *probeRun) portsFail(msg string) {
	p.set("P11", false, msg)
	p.set("P12", false, msg)
}

// cleanup 删除探活临时容器; 使用独立超时, 探活整体超时后仍尽力清理。
func (p *probeRun) cleanup(name string) {
	cmd, err := cleanupCommand(name)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = p.client.Run(ctx, cmd)
}

// bench P13: 平台探针镜像内每张空闲健康卡 60s 矩阵乘, 期间采样降频原因, 结束后复扫 Xid 与 ECC。
func (p *probeRun) bench(ctx context.Context) {
	free := p.freeUUIDs()
	if len(free) == 0 {
		p.set("P13", true, "无空闲健康卡可压测(在租卡不做侵入项)")
		return
	}
	name := fmt.Sprintf("omnis-probe-%d-%s", p.n.ID, randomSuffix())
	cmd, err := benchCommand(p.s.cfg.ProbeImage, name, free, p.s.benchSecs)
	if err != nil {
		p.set("P13", false, err.Error())
		return
	}
	sample, err := throttleSampleCommand(p.s.sampleN, p.s.sampleGap)
	if err != nil {
		p.set("P13", false, err.Error())
		return
	}
	defer p.cleanup(name)
	type res struct {
		out string
		err error
	}
	sch := make(chan res, 1)
	go func() {
		out, err := p.runFor(ctx, sample, time.Duration(p.s.sampleN*p.s.sampleGap+60)*time.Second)
		sch <- res{out, err}
	}()
	out, berr := p.runFor(ctx, cmd, time.Duration(p.s.benchSecs+300)*time.Second)
	sr := <-sch

	results, perr := ParseBench(out)
	if berr != nil && perr != nil {
		p.set("P13", false, "压测容器执行失败: "+berr.Error())
		return
	}
	if perr != nil {
		p.set("P13", false, perr.Error())
		return
	}
	throttle := map[string]uint64{}
	var notes []string
	if sr.err != nil {
		notes = append(notes, "降频采样失败: "+sr.err.Error())
	} else if throttle, err = ParseThrottle(sr.out); err != nil {
		notes = append(notes, err.Error())
		throttle = map[string]uint64{}
	}
	p.health(ctx, 10*time.Minute, "") // 压测后复扫: 新出现的致命 Xid / ECC 直接标故障

	byUUID := map[string]BenchResult{}
	for _, r := range results {
		byUUID[r.UUID] = r
	}
	var baseline float64
	if p.cat != nil && p.cat.ProbeBaselineTFLOPS != nil {
		baseline = *p.cat.ProbeBaselineTFLOPS
	}
	ratio := p.s.cfg.Probe.MinPerfRatio
	passed := 0
	var lines []string
	for _, g := range p.gpus {
		if !containsStr(free, g.UUID) {
			continue
		}
		r, ok := byUUID[g.UUID]
		var why string
		switch {
		case p.inv.Faulty[g.Index] != "":
			why = p.inv.Faulty[g.Index]
		case !ok:
			why = "压测无结果(疑似掉卡)"
		case !r.OK:
			why = "压测出错: " + truncate(r.Error, 200)
		case throttle[g.UUID]&throttleFailMask != 0:
			why = "压测期间降频: " + DescribeThrottle(throttle[g.UUID]&throttleFailMask)
		case baseline > 0 && r.TFLOPS < baseline*ratio:
			why = fmt.Sprintf("实测 %.1f TFLOPS 低于基准 %.1f 的 %.0f%%", r.TFLOPS, baseline, ratio*100)
		}
		if why != "" {
			p.inv.Faulty[g.Index] = why
			delete(p.freeIdx, g.Index)
			lines = append(lines, fmt.Sprintf("GPU%d 不通过(标记故障): %s", g.Index, why))
			continue
		}
		passed++
		lines = append(lines, fmt.Sprintf("GPU%d %.1f TFLOPS", g.Index, r.TFLOPS))
	}
	if baseline == 0 {
		notes = append(notes, "gpu_catalog 未填写该型号探活基准, 只校验稳定性、不比对算力")
	}
	detail := fmt.Sprintf("压测 %d 张空闲卡 %ds, %d 张通过; %s", len(free), p.s.benchSecs, passed, strings.Join(lines, "; "))
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	p.set("P13", passed > 0, detail)
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
