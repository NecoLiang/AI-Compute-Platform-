package fleet

// 平台对节点执行的命令全部是本文件的固定模板(docs/25 §25.4.3 末段): 参数只来自平台生成值、
// 配置与数据库, 且逐个经白名单校验; 不拼接任何用户输入。宿主机返回的值(如 DockerRootDir)
// 同样视为不可信, 回填到命令前必须过校验。

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	cmdGPUList     = "nvidia-smi --query-gpu=index,uuid,name,memory.total,pci.bus_id,driver_version --format=csv,noheader,nounits"
	cmdSMIHeader   = "nvidia-smi"
	cmdTopo        = "nvidia-smi topo -m"
	cmdECC         = "nvidia-smi --query-gpu=index,ecc.errors.uncorrected.volatile.total,retired_pages.pending --format=csv,noheader,nounits"
	cmdRemap       = "nvidia-smi --query-remapped-rows=gpu_uuid,remapped_rows.pending,remapped_rows.failure --format=csv,noheader,nounits"
	cmdDocker      = "docker info --format '{{json .}}'"
	cmdPorts       = "ss -Htln"
	cmdDockerAlive = "docker ps -q --filter label=omnis.lease"
	// cmdLightGPUApps 轻探活读宿主机 GPU 计算进程; 前后任一次看到平台探针容器(omnis.probe=1, 如防火墙下发/挖矿扫描)则只输出 @@probe, 本轮不判占用。
	cmdLightGPUApps = `p1=$(docker ps -q --filter label=omnis.probe=1) || exit 1; ` +
		`a=$(nvidia-smi --query-compute-apps=gpu_uuid,pid --format=csv,noheader) || exit 1; ` +
		`p2=$(docker ps -q --filter label=omnis.probe=1) || exit 1; ` +
		`if [ -n "$p1$p2" ]; then echo @@probe; else printf '%s\n' "$a"; fi`
	// cmdTelemetry 周期轻量探活唯一的 nvidia-smi 查询(与 docker 守护进程存活一并检查): 逐卡显存已用/总量、温度、功耗、
	// 降频原因位图、ECC 不可纠正/可纠正(volatile, 告警口径)、利用率。执行或解析失败即本次轻探活失败;
	// 成功则落 gpu_slots 遥测列(docs/27 §27.3 F3 输入)并用于掉卡/ECC 告警与节点平均利用率。
	cmdTelemetry = "nvidia-smi --query-gpu=uuid,memory.used,memory.total,temperature.gpu,power.draw,clocks_throttle_reasons.active," +
		"ecc.errors.uncorrected.volatile.total,ecc.errors.corrected.volatile.total,utilization.gpu,utilization.memory --format=csv,noheader,nounits"
	// cmdTelemetryEvent 新驱动把 clocks_throttle_reasons 更名为 clocks_event_reasons; 前者报 "is not a valid field" 时改用此式。
	cmdTelemetryEvent = "nvidia-smi --query-gpu=uuid,memory.used,memory.total,temperature.gpu,power.draw,clocks_event_reasons.active," +
		"ecc.errors.uncorrected.volatile.total,ecc.errors.corrected.volatile.total,utilization.gpu,utilization.memory --format=csv,noheader,nounits"
)

var (
	safePathRe  = regexp.MustCompile(`^/[A-Za-z0-9._/-]{0,200}$`)
	imageRefRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,200}@sha256:[0-9a-f]{64}$`)
	containerRe = regexp.MustCompile(`^omnis-probe-[0-9]+-[0-9a-f]{8}$`)
	sinceRe     = regexp.MustCompile(`^-[0-9]{1,3}(h|min)$`)
)

// xidCommand 读取内核日志中最近 since 时间窗的 Xid(docs/25 §25.4.4、docs/27 §27.3 F6)。输出:
//
//	@@id uid=<n> groups=<g …>   管理账户身份, 供 ParseXid 判断是否有权限读内核日志
//	@@now <unix 秒>             节点时钟, 用于把事件时间换算到平台时钟
//	@@src journal|dmesg         数据来源: 有 journalctl 用 journalctl -k, 否则退回 dmesg
//	@@first <ISO 时间>          当前可读到的最早一条内核消息(journalctl -k 只含本次启动, dmesg 环形缓冲会被覆盖),
//	                            用于判断时间窗是否被完整覆盖
//	@@err <stderr 行>           命令的错误输出(权限不足、选项不支持等), 有则本次扫描记"未知"
//	<NVRM: Xid 行>              最多 xidLineCap+1 行, 超过 xidLineCap 视为截断
//
// stderr 通过 fd3 交换单独加 @@err 前缀, 避免内核消息正文里的 "not found" 等字样被误判为命令错误。
// dmesg 没有按时间过滤的参数, 时间窗由 ParseXid 按事件时间在平台侧过滤。
func xidCommand(since string) (string, error) {
	if !sinceRe.MatchString(since) {
		return "", fmt.Errorf("非法时间窗: %q", since)
	}
	return fmt.Sprintf(`echo "@@id uid=$(id -u) groups=$(id -Gn)"; echo "@@now $(date +%%s)"; `+
		`if command -v journalctl >/dev/null 2>&1; then echo "@@src journal"; `+
		`echo "@@first $(journalctl -k -q --no-pager -o short-iso 2>/dev/null | head -n 1 | cut -d' ' -f1)"; `+
		`{ journalctl -k --since '%s' --no-pager -o short-iso 2>&1 1>&3 3>&- | sed 's/^/@@err /'; } 3>&1 | grep -E '^@@err|NVRM: Xid' | tail -n %d; `+
		`else echo "@@src dmesg"; `+
		`echo "@@first $(dmesg --time-format iso 2>/dev/null | head -n 1 | cut -d' ' -f1)"; `+
		`{ dmesg --time-format iso 2>&1 1>&3 3>&- | sed 's/^/@@err /'; } 3>&1 | grep -E '^@@err|NVRM: Xid' | tail -n %d; fi`,
		since, xidLineCap+1, xidLineCap+1), nil
}

// xidLineCap 单次扫描最多保留的 Xid 行数; 超出时只保留最新的 xidLineCap 行, 覆盖起点相应后移(见 ParseXid)。
const xidLineCap = 500

// xidSince 把时间窗换算为 journalctl --since 参数(满足 sinceRe): 分钟向上取整, 至少 2 分钟, 最多 24 小时。
func xidSince(window time.Duration) (string, time.Duration) {
	m := int((window + time.Minute - 1) / time.Minute)
	if m < 2 {
		m = 2
	}
	if m > 999 {
		h := (m + 59) / 60
		if h > 24 {
			h = 24
		}
		return fmt.Sprintf("-%dh", h), time.Duration(h) * time.Hour
	}
	return fmt.Sprintf("-%dmin", m), time.Duration(m) * time.Minute
}

// cacheDFCommand 模型缓存目录所在文件系统的可用/总字节数(docs/27 §27.4.3 F4 输入)。
// 目录不存在时输出 @@missing, 不建目录(建目录属模型部署流程)。
func cacheDFCommand(dir string) (string, error) {
	if !safePathRe.MatchString(dir) || strings.Contains(dir, "..") {
		return "", fmt.Errorf("模型缓存目录路径非法: %q", truncate(dir, 80))
	}
	return "if [ -d '" + dir + "' ]; then df -B1 --output=avail,size '" + dir + "'; else echo @@missing; fi", nil
}

// hostInfoCommand P8; dockerRoot 来自宿主机 docker info, 必须过路径白名单。
func hostInfoCommand(dockerRoot string) (string, error) {
	if !safePathRe.MatchString(dockerRoot) || strings.Contains(dockerRoot, "..") {
		return "", fmt.Errorf("Docker 数据目录路径非法: %q", truncate(dockerRoot, 80))
	}
	return "echo @@nproc; nproc; echo @@mem; grep MemTotal /proc/meminfo; echo @@arch; uname -m; " +
		"echo @@kernel; uname -r; echo @@os; cat /etc/os-release; echo @@df; df -P -BG '" + dockerRoot + "'", nil
}

func checkImage(ref string) error {
	if !imageRefRe.MatchString(ref) {
		return fmt.Errorf("镜像引用必须是 ref@sha256:<digest>: %q", truncate(ref, 80))
	}
	return nil
}

func checkUUIDs(uuids []string) error {
	for _, u := range uuids {
		if !uuidRe.MatchString(u) {
			return fmt.Errorf("GPU UUID 非法: %q", truncate(u, 60))
		}
	}
	return nil
}

// gpusFlag --gpus 参数: 只挂指定的卡(租约容器), 不触碰其他卡; 空列表为 "0"。
func gpusFlag(uuids []string) (string, error) {
	if len(uuids) == 0 {
		return "0", nil
	}
	if err := checkUUIDs(uuids); err != nil {
		return "", err
	}
	return `'"device=` + strings.Join(uuids, ",") + `"'`, nil
}

func pullCommand(image string) (string, error) {
	if err := checkImage(image); err != nil {
		return "", err
	}
	return "docker pull -q " + image, nil
}

func checkPort(port int) error {
	if port < 1024 || port > 65535 {
		return fmt.Errorf("端口非法: %d", port)
	}
	return nil
}

// installKeyCommand 一次性引导: 幂等追加平台公钥行到 ~/.ssh/authorized_keys。
func installKeyCommand(line, keyBody string) (string, error) {
	if !authorizedLineRe.MatchString(line) || keyBody == "" || !authorizedLineRe.MatchString(keyBody) {
		return "", fmt.Errorf("公钥行含非法字符")
	}
	return "umask 077; mkdir -p ~/.ssh && touch ~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys && " +
		"(grep -qF '" + keyBody + "' ~/.ssh/authorized_keys || printf '%s\\n' '" + line + "' >> ~/.ssh/authorized_keys) && echo @@installed", nil
}
