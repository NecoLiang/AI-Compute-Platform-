package fleet

// 平台对节点执行的命令全部是本文件的固定模板(docs/25 §25.4.3 末段): 参数只来自平台生成值、
// 配置与数据库, 且逐个经白名单校验; 不拼接任何用户输入。宿主机返回的值(如 DockerRootDir)
// 同样视为不可信, 回填到命令前必须过校验。

import (
	"encoding/base64"
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

// gpusFlag --gpus 参数: 指定空闲卡; 无空闲卡时 "0" 只验运行时(docs/25 §25.4.4 不触碰在租资源)。
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

// runtimeCheckCommand P9: 探针容器只挂空闲卡, 列出容器内可见 GPU。
func runtimeCheckCommand(image string, freeUUIDs []string) (string, error) {
	if err := checkImage(image); err != nil {
		return "", err
	}
	g, err := gpusFlag(freeUUIDs)
	if err != nil {
		return "", err
	}
	if len(freeUUIDs) == 0 {
		return "docker run --rm --network none --gpus " + g + " " + image + " true", nil
	}
	return "docker run --rm --network none --gpus " + g + " " + image + " nvidia-smi -L", nil
}

// quotaCheckCommand P10: --storage-opt size=1G 能否启动(overlay2 over xfs+pquota)。
func quotaCheckCommand(image string) (string, error) {
	if err := checkImage(image); err != nil {
		return "", err
	}
	return "docker run --rm --network none --storage-opt size=1G " + image + " true", nil
}

// pyExec 把固定 Python 脚本以 base64 交给容器内 python3 执行, 规避多层引号。
func pyExec(script string) string {
	return `python3 -c 'exec(__import__("base64").b64decode("` + base64.StdEncoding.EncodeToString([]byte(script)) + `"))'`
}

// listenerScript 端口自检临时监听: 接受连接后立即关闭。
const listenerScript = `import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 8000))
s.listen(16)
while True:
    c, _ = s.accept()
    c.close()
`

func checkPort(port int) error {
	if port < 1024 || port > 65535 {
		return fmt.Errorf("端口非法: %d", port)
	}
	return nil
}

// portListenerCommand P11/P12: 在租约端口段的空闲端口上起临时监听容器, 并在宿主机本地确认已监听。
// 容器自带 --rm 与 omnis.probe 标签, 超时被杀时由 cleanupCommand 兜底删除。
func portListenerCommand(image, name string, port int) (string, error) {
	if err := checkImage(image); err != nil {
		return "", err
	}
	if !containerRe.MatchString(name) {
		return "", fmt.Errorf("容器名非法: %q", name)
	}
	if err := checkPort(port); err != nil {
		return "", err
	}
	return fmt.Sprintf(`docker run -d --rm --name %s --label omnis.probe=1 -p %d:8000 %s %s >/dev/null && `+
		`for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do if timeout 2 bash -c '</dev/tcp/127.0.0.1/%d' 2>/dev/null; then echo @@up; exit 0; fi; sleep 1; done; echo @@down`,
		name, port, image, pyExec(listenerScript), port), nil
}

func cleanupCommand(name string) (string, error) {
	if !containerRe.MatchString(name) {
		return "", fmt.Errorf("容器名非法: %q", name)
	}
	return "docker rm -f " + name + " >/dev/null 2>&1; true", nil
}

// benchScript P13: 每张可见卡一个线程做 FP16 8192² 矩阵乘, 持续 argv[1] 秒, 输出一行 JSON。
// CUDA_DEVICE_ORDER=PCI_BUS_ID 保证与 nvidia-smi -L 的顺序一致, 以便按 UUID 对应。
const benchScript = `import os, sys, json, time, subprocess, threading
os.environ["CUDA_DEVICE_ORDER"] = "PCI_BUS_ID"
secs = float(sys.argv[1]) if len(sys.argv) > 1 else 60.0
uuids = []
for l in subprocess.check_output(["nvidia-smi", "-L"], text=True).splitlines():
    if "UUID: " in l:
        uuids.append(l.split("UUID: ")[1].rstrip(")").strip())
import torch
res = [None] * torch.cuda.device_count()
def run(i):
    r = {"index": i, "uuid": uuids[i] if i < len(uuids) else "", "ok": False, "tflops": 0.0, "error": ""}
    try:
        torch.cuda.set_device(i)
        n = 8192
        a = torch.randn(n, n, device="cuda", dtype=torch.float16)
        b = torch.randn(n, n, device="cuda", dtype=torch.float16)
        for _ in range(3):
            c = a @ b
        torch.cuda.synchronize()
        it, t0 = 0, time.time()
        while time.time() - t0 < secs:
            for _ in range(10):
                c = a @ b
            torch.cuda.synchronize()
            it += 10
        dt = time.time() - t0
        if not bool(torch.isfinite(c).all()):
            raise RuntimeError("non-finite result")
        r["tflops"] = round(2.0 * n ** 3 * it / dt / 1e12, 1)
        r["ok"] = True
    except Exception as e:
        r["error"] = str(e)[:200]
    res[i] = r
ts = [threading.Thread(target=run, args=(i,)) for i in range(len(res))]
for t in ts:
    t.start()
for t in ts:
    t.join()
print(json.dumps({"results": [r for r in res if r is not None]}))
`

// benchCommand P13: 只压空闲卡; 容器名固定以便超时后清理。
func benchCommand(image, name string, freeUUIDs []string, seconds int) (string, error) {
	if err := checkImage(image); err != nil {
		return "", err
	}
	if !containerRe.MatchString(name) {
		return "", fmt.Errorf("容器名非法: %q", name)
	}
	if len(freeUUIDs) == 0 {
		return "", fmt.Errorf("无空闲卡可压测")
	}
	if seconds < 5 || seconds > 600 {
		return "", fmt.Errorf("压测时长非法: %d", seconds)
	}
	g, err := gpusFlag(freeUUIDs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("docker run --rm --name %s --label omnis.probe=1 --network none --gpus %s %s %s %d",
		name, g, image, pyExec(benchScript), seconds), nil
}

// throttleSampleCommand 压测期间每 interval 秒采样一次降频原因, 共 rounds 轮。
func throttleSampleCommand(rounds, interval int) (string, error) {
	if rounds < 1 || rounds > 60 || interval < 1 || interval > 60 {
		return "", fmt.Errorf("采样参数非法")
	}
	return fmt.Sprintf("for i in $(seq 1 %d); do sleep %d; echo @@t$i; nvidia-smi --query-gpu=uuid,clocks_throttle_reasons.active --format=csv,noheader; done",
		rounds, interval), nil
}

// installKeyCommand 一次性引导: 幂等追加平台公钥行到 ~/.ssh/authorized_keys。
func installKeyCommand(line, keyBody string) (string, error) {
	if !authorizedLineRe.MatchString(line) || keyBody == "" || !authorizedLineRe.MatchString(keyBody) {
		return "", fmt.Errorf("公钥行含非法字符")
	}
	return "umask 077; mkdir -p ~/.ssh && touch ~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys && " +
		"(grep -qF '" + keyBody + "' ~/.ssh/authorized_keys || printf '%s\\n' '" + line + "' >> ~/.ssh/authorized_keys) && echo @@installed", nil
}
