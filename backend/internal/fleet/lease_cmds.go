package fleet

// 租约生命周期(T-087/T-088)对节点执行的固定命令模板: stop / kill_sessions / purge 各步骤 / restart 用到的
// 检查、提交与启动 / 对账 / 挖矿特征扫描。与 commands.go、provision_cmds.go 同一原则: 参数只来自平台生成值
// (租约号派生的资源名、平台生成的提交镜像名、配置中的探针镜像), 逐个过白名单; 不拼接任何买家可控字符串。
// 节点与容器返回的输出一律视为不可信, 只做解析不回填进命令。

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// stopTimeout docker stop -t 30 加上连接开销。
const stopTimeout = 90 * time.Second

// commitRefRe restart 换卡重建时 docker commit 产生的本地镜像(docs/25 §25.7.2), purge 时一并删除。
var commitRefRe = regexp.MustCompile(`^omnis-commit/l-[a-z2-7]{8}:[0-9]{10}$`)

// commitRefOf 提交镜像名: omnis-commit/<lease_no>:<unix 秒>。
func commitRefOf(leaseNo string, unix int64) (string, error) {
	ref := fmt.Sprintf("omnis-commit/%s:%010d", leaseNo, unix)
	if !commitRefRe.MatchString(ref) {
		return "", fmt.Errorf("提交镜像名非法: %q", ref)
	}
	return ref, nil
}

func leaseContainer(leaseNo string) (string, error) {
	if !ValidLeaseNo(leaseNo) {
		return "", fmt.Errorf("租约号非法: %q", leaseNo)
	}
	c := containerNameOf(leaseNo)
	return c, checkLeaseRes(c)
}

// gpuIDsFormat docker inspect 取容器 --gpus device=… 的卡 UUID(逗号结尾的列表)。
const gpuIDsFormat = `{{range .HostConfig.DeviceRequests}}{{range .DeviceIDs}}{{.}},{{end}}{{end}}`

// stopContainerCommand stop: 容器存在 → docker stop -t 30(已有会话随容器 sshd 退出断开), 输出
// "@@stopped <卡UUID,…>"; 容器不存在 → "@@absent"。已停止的容器再次 stop 为无操作, 幂等。
func stopContainerCommand(leaseNo string) (string, error) {
	c, err := leaseContainer(leaseNo)
	if err != nil {
		return "", err
	}
	return "if docker inspect --type container " + c + " >/dev/null 2>&1; then " +
		"docker stop -t 30 " + c + " >/dev/null && echo \"@@stopped $(docker inspect --type container -f '" + gpuIDsFormat + "' " + c + ")\"; " +
		"else echo @@absent; fi", nil
}

// containerStateCommand 读容器是否存在、是否运行、挂载的卡: "@@absent" 或 "@@state <running> <卡UUID,…>"。
func containerStateCommand(leaseNo string) (string, error) {
	c, err := leaseContainer(leaseNo)
	if err != nil {
		return "", err
	}
	return "if docker inspect --type container " + c + " >/dev/null 2>&1; then " +
		"echo \"@@state $(docker inspect --type container -f '{{.State.Running}} " + gpuIDsFormat + "' " + c + ")\"; " +
		"else echo @@absent; fi", nil
}

// ContainerState containerStateCommand 的解析结果。
type ContainerState struct {
	Exists  bool
	Running bool
	GPUs    []string
}

// parseContainerState 解析 containerStateCommand 输出; 卡 UUID 逐个过白名单, 不合法即报错。
func parseContainerState(out string) (ContainerState, error) {
	for _, l := range nonEmptyLines(out) {
		if l == "@@absent" {
			return ContainerState{}, nil
		}
		if !strings.HasPrefix(l, "@@state ") {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(l, "@@state "))
		if len(f) == 0 || (f[0] != "true" && f[0] != "false") {
			return ContainerState{}, fmt.Errorf("容器状态输出无法解析: %q", truncate(l, 120))
		}
		st := ContainerState{Exists: true, Running: f[0] == "true"}
		if len(f) > 1 {
			gpus, err := parseGPUIDs(f[1])
			if err != nil {
				return ContainerState{}, err
			}
			st.GPUs = gpus
		}
		return st, nil
	}
	return ContainerState{}, fmt.Errorf("容器状态无结论输出: %q", truncate(strings.TrimSpace(out), 120))
}

// parseStopped 解析 stopContainerCommand 输出: (容器存在, 原卡 UUID)。
func parseStopped(out string) (bool, []string, error) {
	for _, l := range nonEmptyLines(out) {
		if l == "@@absent" {
			return false, nil, nil
		}
		if l == "@@stopped" {
			return true, nil, nil
		}
		if strings.HasPrefix(l, "@@stopped ") {
			gpus, err := parseGPUIDs(strings.TrimSpace(strings.TrimPrefix(l, "@@stopped ")))
			return true, gpus, err
		}
	}
	return false, nil, fmt.Errorf("停止容器无结论输出: %q", truncate(strings.TrimSpace(out), 120))
}

func parseGPUIDs(s string) ([]string, error) {
	var out []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		if !uuidRe.MatchString(u) {
			return nil, fmt.Errorf("容器挂载的 GPU 标识非法: %q", truncate(u, 60))
		}
		out = append(out, u)
	}
	return out, nil
}

// killSessionsScript 容器内结束全部 sshd 会话进程(PID 1 为容器主 sshd, 保留), 输出 "@@killed <n>"。
// 脚本不含单引号, 直接作为 sh -c 的单引号参数(不依赖容器内 base64: 买家在容器内有 root, 可删改工具)。
const killSessionsScript = `n=0; for d in /proc/[0-9]*; do p=${d#/proc/}; [ "$p" = 1 ] && continue; ` +
	`c=$(cat "$d/comm" 2>/dev/null) || continue; case "$c" in sshd|sshd-session|sshd-auth) ;; *) continue;; esac; ` +
	`kill -KILL "$p" 2>/dev/null && n=$((n+1)); done; echo "@@killed $n"`

// killSessionsCommand kill_sessions(冻结, docs/25 §25.7.3): 不停容器(保全现场), 只断开已有会话;
// 容器未运行 → "@@notrunning"(没有会话可断)。
func killSessionsCommand(leaseNo string) (string, error) {
	c, err := leaseContainer(leaseNo)
	if err != nil {
		return "", err
	}
	if strings.Contains(killSessionsScript, "'") {
		return "", fmt.Errorf("内部错误: 会话清理脚本含单引号")
	}
	return "if [ \"$(docker inspect --type container -f '{{.State.Running}}' " + c + " 2>/dev/null)\" = true ]; then " +
		"docker exec " + c + " sh -c '" + killSessionsScript + "'; else echo @@notrunning; fi", nil
}

// wallCommand 到期提醒的容器内广播(尽力而为, 失败不影响站内通知)。消息为平台固定模板 + 数字。
func wallCommand(leaseNo string, minutes int) (string, error) {
	c, err := leaseContainer(leaseNo)
	if err != nil {
		return "", err
	}
	if minutes < 1 || minutes > 100000 {
		return "", fmt.Errorf("提醒分钟数非法: %d", minutes)
	}
	msg := fmt.Sprintf("[omnisline] lease expires in %d minutes; renew in console to keep this environment.", minutes)
	return "if [ \"$(docker inspect --type container -f '{{.State.Running}}' " + c + " 2>/dev/null)\" = true ]; then " +
		"docker exec " + c + " sh -c 'command -v wall >/dev/null 2>&1 && echo \"" + msg + "\" | wall'; echo @@sent; " +
		"else echo @@notrunning; fi", nil
}

// ===== purge 各步骤(每步单独执行, 回执记录每步退出码) =====

// PurgeStep 清除回执中的一步(docs/25 §25.7.5)。
type PurgeStep struct {
	Step     string `json:"step"`   // container / volume / network / commit_image
	Target   string `json:"target"` // 资源名
	ExitCode int    `json:"exit_code"`
	Outcome  string `json:"outcome"`      // removed / absent / error
	ID       string `json:"id,omitempty"` // 被删除容器的 ID
	Detail   string `json:"detail,omitempty"`
}

var dockerIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func purgeContainerCommand(name string) (string, error) {
	if err := checkLeaseRes(name); err != nil {
		return "", err
	}
	return "if id=$(docker inspect --type container -f '{{.Id}}' " + name + " 2>/dev/null); then " +
		"docker rm -f " + name + " >/dev/null && echo \"@@removed $id\"; else echo @@absent; fi", nil
}

func purgeVolumeCommand(name string) (string, error) {
	if err := checkLeaseRes(name); err != nil {
		return "", err
	}
	return "if docker volume inspect " + name + " >/dev/null 2>&1; then " +
		"docker volume rm -f " + name + " >/dev/null && echo @@removed; else echo @@absent; fi", nil
}

func purgeNetworkCommand(name string) (string, error) {
	if err := checkLeaseRes(name); err != nil {
		return "", err
	}
	return "if docker network inspect " + name + " >/dev/null 2>&1; then " +
		"docker network rm " + name + " >/dev/null && echo @@removed; else echo @@absent; fi", nil
}

func removeCommitImageCommand(ref string) (string, error) {
	if !commitRefRe.MatchString(ref) {
		return "", fmt.Errorf("提交镜像名非法: %q", truncate(ref, 80))
	}
	return "if docker image inspect " + ref + " >/dev/null 2>&1; then " +
		"docker rmi -f " + ref + " >/dev/null && echo @@removed; else echo @@absent; fi", nil
}

// parsePurgeOut 解析单步输出: removed(带容器 ID 时校验格式)/absent; 无结论视为 error。
func parsePurgeOut(out string) (outcome, id string) {
	for _, l := range nonEmptyLines(out) {
		switch {
		case l == "@@absent":
			return "absent", ""
		case l == "@@removed":
			return "removed", ""
		case strings.HasPrefix(l, "@@removed "):
			v := strings.TrimSpace(strings.TrimPrefix(l, "@@removed "))
			if !dockerIDRe.MatchString(v) {
				v = ""
			}
			return "removed", v
		}
	}
	return "error", ""
}

// ===== restart(docs/25 §25.7.2) =====

// startContainerCommand 原卡空闲: 启动宽限期内停止的原容器(入口脚本重放, 主机密钥不变)。
func startContainerCommand(leaseNo string) (string, error) {
	c, err := leaseContainer(leaseNo)
	if err != nil {
		return "", err
	}
	return "docker start " + c + " >/dev/null && echo @@started", nil
}

// commitContainerCommand 原卡被占: 把原容器提交为本地镜像, 随后以新卡重建(挂载原数据卷)。
func commitContainerCommand(leaseNo, ref string) (string, error) {
	c, err := leaseContainer(leaseNo)
	if err != nil {
		return "", err
	}
	if !commitRefRe.MatchString(ref) || !strings.HasPrefix(ref, "omnis-commit/"+leaseNo+":") {
		return "", fmt.Errorf("提交镜像名非法: %q", truncate(ref, 80))
	}
	return "docker commit " + c + " " + ref + " >/dev/null && echo @@committed", nil
}

// ===== 对账 =====

// cmdLeaseContainers 节点上全部租约容器(按 omnis.lease 标签)的名字与状态。
const cmdLeaseContainers = "docker ps -a --filter label=omnis.lease --format '{{.Names}} {{.State}}'"

// parseLeaseContainers 名字 → 状态(running/exited/created/…); 名字不合法的行忽略。
func parseLeaseContainers(out string) map[string]string {
	res := map[string]string{}
	for _, l := range nonEmptyLines(out) {
		f := strings.Fields(l)
		if len(f) != 2 || !strings.HasPrefix(f[0], "omnis-") || !ValidLeaseNo(strings.TrimPrefix(f[0], "omnis-")) {
			continue
		}
		res[f[0]] = f[1]
	}
	return res
}

// ===== 挖矿特征扫描(docs/25 §25.7.3) =====

// miningScanCommand 进程表(宿主机侧 docker top, 容器内无法篡改 ps)与容器网络命名空间的 TCP 连接表
// (以平台探针镜像加入容器网络命名空间读取 /proc/net/tcp{,6}, 不依赖容器内的 cat)。容器未运行 → "@@notrunning"。
func miningScanCommand(probeImage, leaseNo string) (string, error) {
	c, err := leaseContainer(leaseNo)
	if err != nil {
		return "", err
	}
	if err := checkImage(probeImage); err != nil {
		return "", err
	}
	return "if [ \"$(docker inspect --type container -f '{{.State.Running}}' " + c + " 2>/dev/null)\" != true ]; then echo @@notrunning; exit 0; fi; " +
		"echo @@top; docker top " + c + " -eo pid,comm,args || exit 1; " +
		"echo @@tcp; docker run --rm --network container:" + c + " --cap-drop ALL --label omnis.probe=1 --entrypoint cat " +
		probeImage + " /proc/net/tcp /proc/net/tcp6 || exit 1; echo @@end", nil
}
