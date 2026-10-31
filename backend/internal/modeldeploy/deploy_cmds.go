package modeldeploy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 节点命令构造。所有拼进命令的值都先过白名单(目录录入时已校验, 这里再校验一次, 防止库中旧行/脏数据注入)。

var (
	gpuUUIDRe   = regexp.MustCompile(`^GPU-[0-9a-fA-F-]{32,40}$`)
	resNameRe   = regexp.MustCompile(`^omnis-[a-z0-9-]{1,48}$`)
	cacheDirRe  = regexp.MustCompile(`^/[A-Za-z0-9._/@-]{1,300}$`)
	argKeyRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	pullNameFmt = "omnis-pull-m%d"
)

// engineNetworkAlias 买家在租约容器内访问引擎的主机名(docs/27 §27.4.2)。
const engineNetworkAlias = "omnis-engine"

// markerFile 拉取完成标记: 内容即 sha256sum 校验清单, 启动前比对其摘要与目录清单一致。
const markerFile = ".omnis-manifest.sha256"

func checkCacheDir(dir string) error {
	if !cacheDirRe.MatchString(dir) || strings.Contains(dir, "..") {
		return fmt.Errorf("模型缓存目录非法: %q", dir)
	}
	return nil
}

// modelDir 节点上的权重目录 <cache>/<model_key>@<revision>。
func modelDir(cacheDir string, e *ModelEntry) (string, error) {
	if err := checkCacheDir(cacheDir); err != nil {
		return "", err
	}
	if !modelKeyPattern.MatchString(e.ModelKey) || !revisionPattern.MatchString(e.Revision) {
		return "", fmt.Errorf("目录条目 %d 的 model_key/revision 非法", e.ID)
	}
	return strings.TrimRight(cacheDir, "/") + "/" + e.ModelKey + "@" + e.Revision, nil
}

// manifestChecklist 由目录清单生成 sha256sum -c 校验清单("<sha>  <path>\n"), 以及它自身的摘要(完成标记的期望值)。
func manifestChecklist(e *ModelEntry) (string, string, error) {
	var files []ManifestFile
	if err := json.Unmarshal(e.Manifest, &files); err != nil || len(files) == 0 {
		return "", "", fmt.Errorf("目录条目 %d 清单无法解析", e.ID)
	}
	var sb strings.Builder
	for _, f := range files {
		if !validManifestPath(f.Path) || strings.ContainsAny(f.Path, "\n\r\x00") || !sha256Pattern.MatchString(f.SHA256) {
			return "", "", fmt.Errorf("目录条目 %d 清单含非法路径或摘要: %q", e.ID, f.Path)
		}
		sb.WriteString(f.SHA256)
		sb.WriteString("  ")
		sb.WriteString(f.Path)
		sb.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return sb.String(), hex.EncodeToString(sum[:]), nil
}

// markerCheckCommand 输出完成标记的摘要(64hex), 无标记输出 @@nomarker。
func markerCheckCommand(dir string) string {
	m := dir + "/" + markerFile
	return "if [ -f " + m + " ]; then sha256sum < " + m + " | cut -d' ' -f1; else echo @@nomarker; fi"
}

// liveCheckCommand checking 阶段在节点上实时读: 各卡显存占用(F3)、缓存区剩余(F4)、监听端口(端口分配)。
func liveCheckCommand(cacheDir string) (string, error) {
	if err := checkCacheDir(cacheDir); err != nil {
		return "", err
	}
	return "echo @@smi; nvidia-smi --query-gpu=uuid,memory.used,memory.total --format=csv,noheader,nounits; " +
		"echo @@df; mkdir -p " + cacheDir + " && df -B1 --output=avail " + cacheDir + " | tail -n 1; " +
		"echo @@ports; ss -Htln; echo @@end", nil
}

type liveCheck struct {
	VRAM       map[string][2]int64 // uuid → [used MiB, total MiB]
	CacheAvail int64               // -1 = 未读到
	Listening  string              // ss -Htln 原文, 交由 Runtime.ListeningPorts 解析
}

func parseLiveCheck(out string) (liveCheck, error) {
	lc := liveCheck{VRAM: map[string][2]int64{}, CacheAvail: -1}
	sec := ""
	var ports []string
	ended := false
	for _, raw := range strings.Split(out, "\n") {
		l := strings.TrimSpace(raw)
		switch l {
		case "@@smi", "@@df", "@@ports":
			sec = l
			continue
		case "@@end":
			ended = true
			sec = ""
			continue
		}
		if l == "" {
			continue
		}
		switch sec {
		case "@@smi":
			f := strings.Split(l, ",")
			if len(f) != 3 {
				continue
			}
			u := strings.TrimSpace(f[0])
			used, err1 := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
			total, err2 := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
			if gpuUUIDRe.MatchString(u) && err1 == nil && err2 == nil {
				lc.VRAM[u] = [2]int64{used, total}
			}
		case "@@df":
			if v, err := strconv.ParseInt(l, 10, 64); err == nil && v >= 0 {
				lc.CacheAvail = v
			}
		case "@@ports":
			ports = append(ports, raw)
		}
	}
	if !ended {
		return lc, fmt.Errorf("节点实时检查输出不完整")
	}
	lc.Listening = strings.Join(ports, "\n")
	return lc, nil
}

// renderEngineArgs 目录模板参数 → 命令行(键排序保证确定性)。true → --k; false → --no-k; 数字/字符串 → --k=v。
func renderEngineArgs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("engine_args 无法解析")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		if !EngineArgWhitelist[k] || !argKeyRe.MatchString(k) {
			return nil, fmt.Errorf("engine_args 含白名单外的键 %q", k)
		}
		switch v := m[k].(type) {
		case bool:
			if v {
				out = append(out, "--"+k)
			} else {
				out = append(out, "--no-"+k)
			}
		case float64:
			if v < 0 {
				return nil, fmt.Errorf("engine_args.%s 不得为负", k)
			}
			out = append(out, "--"+k+"="+strconv.FormatFloat(v, 'f', -1, 64))
		case string:
			if !argValuePattern.MatchString(v) || strings.HasPrefix(v, "-") {
				return nil, fmt.Errorf("engine_args.%s 取值非法", k)
			}
			out = append(out, "--"+k+"="+v)
		default:
			return nil, fmt.Errorf("engine_args.%s 取值类型非法", k)
		}
	}
	return out, nil
}

// engineSpec 引擎容器参数(docs/27 §27.4.1)。
type engineSpec struct {
	DeployNo    string
	LeaseNo     string
	Network     string
	GPUUUIDs    []string
	ModelDir    string
	ModelKey    string
	Image       string
	Port        int
	CPUs        int
	MemoryGB    int
	TP, DP      int
	MaxModelLen int
	GPUMemUtil  float64
	EngineArgs  json.RawMessage
}

func engineRunCommand(s engineSpec) (string, error) {
	switch {
	case !ValidDeployNo(s.DeployNo):
		return "", fmt.Errorf("部署号非法")
	case !leaseNoRe.MatchString(s.LeaseNo) || !resNameRe.MatchString(s.Network):
		return "", fmt.Errorf("租约号/网络名非法")
	case len(s.GPUUUIDs) == 0:
		return "", fmt.Errorf("未选定 GPU")
	case !engineImagePattern.MatchString(s.Image):
		return "", fmt.Errorf("引擎镜像须按 digest 固定")
	case !modelKeyPattern.MatchString(s.ModelKey):
		return "", fmt.Errorf("model_key 非法")
	case s.Port < 1024 || s.Port > 65535 || s.CPUs < 1 || s.MemoryGB < 1 || s.TP < 1 || s.DP < 1 || s.MaxModelLen < 1:
		return "", fmt.Errorf("引擎参数越界")
	case s.GPUMemUtil < 0.5 || s.GPUMemUtil > 0.99:
		return "", fmt.Errorf("gpu_memory_utilization 越界")
	}
	if err := checkCacheDir(s.ModelDir); err != nil {
		return "", err
	}
	for _, u := range s.GPUUUIDs {
		if !gpuUUIDRe.MatchString(u) {
			return "", fmt.Errorf("GPU 标识非法: %q", u)
		}
	}
	args, err := renderEngineArgs(s.EngineArgs)
	if err != nil {
		return "", err
	}
	parts := []string{
		"docker run -d --name " + EngineContainer(s.DeployNo),
		"--label omnis.lease=" + s.LeaseNo, "--label omnis.deploy=" + s.DeployNo,
		`--gpus '"device=` + strings.Join(s.GPUUUIDs, ",") + `"'`,
		"--network " + s.Network, "--network-alias " + engineNetworkAlias,
		"-v " + s.ModelDir + ":/model:ro",
		fmt.Sprintf("-p 127.0.0.1:%d:8000", s.Port),
		"--shm-size 16g --restart no",
		fmt.Sprintf("--cpus %d --memory %dg --memory-swap %dg", s.CPUs, s.MemoryGB, s.MemoryGB),
		s.Image,
		"--model /model --served-model-name " + s.ModelKey,
		fmt.Sprintf("--tensor-parallel-size %d --data-parallel-size %d --max-model-len %d --gpu-memory-utilization %.2f",
			s.TP, s.DP, s.MaxModelLen, s.GPUMemUtil),
	}
	parts = append(parts, args...)
	return strings.Join(parts, " "), nil
}

var leaseNoRe = regexp.MustCompile(`^l-[a-z2-7]{8}$`)

// ensureImageCommand 镜像不在本地时从平台仓库拉取(按 digest)。
func ensureImageCommand(image string) (string, error) {
	if !engineImagePattern.MatchString(image) {
		return "", fmt.Errorf("镜像须按 digest 固定")
	}
	return "docker image inspect " + image + " >/dev/null 2>&1 || docker pull -q " + image + " >/dev/null; echo @@image", nil
}

// leaseQuotaCommand docker update 租约容器配额(部署前缩减 / 停止后归还)。
// 容器不存在输出 @@absent(租约已清除, 无需归还); 成功输出 @@updated; docker update 失败则非零退出(stderr 原样)。
func leaseQuotaCommand(container string, cpus, memGB int) (string, error) {
	if !resNameRe.MatchString(container) || cpus < 1 || memGB < 1 {
		return "", fmt.Errorf("租约容器配额参数非法")
	}
	return fmt.Sprintf("if docker inspect --type container %s >/dev/null 2>&1; then "+
		"docker update --cpus %d --memory %dg --memory-swap %dg %s >/dev/null && echo @@updated; else echo @@absent; fi",
		container, cpus, memGB, memGB, container), nil
}

// removeEngineCommand 删除引擎容器(不存在也成功)。
func removeEngineCommand(deployNo string) (string, error) {
	if !ValidDeployNo(deployNo) {
		return "", fmt.Errorf("部署号非法")
	}
	c := EngineContainer(deployNo)
	return "if docker inspect --type container " + c + " >/dev/null 2>&1; then docker rm -f " + c +
		" >/dev/null && echo @@removed; else echo @@absent; fi", nil
}

// engineLogsCommand 引擎日志末尾 n 行(stdout+stderr 合并)。
func engineLogsCommand(deployNo string, n int) (string, error) {
	if !ValidDeployNo(deployNo) || n < 1 || n > 1000 {
		return "", fmt.Errorf("日志参数非法")
	}
	c := EngineContainer(deployNo)
	return fmt.Sprintf("if docker inspect --type container %s >/dev/null 2>&1; then docker logs --tail %d %s 2>&1; else echo @@absent; fi", c, n, c), nil
}

// pullContainerName 同节点同模型唯一。
func pullContainerName(modelID int64) string { return fmt.Sprintf(pullNameFmt, modelID) }

// pullChecklistPath 校验清单在节点上的位置(拉取容器以同一路径挂载缓存目录)。
func pullChecklistPath(cacheDir string, e *ModelEntry) string {
	return strings.TrimRight(cacheDir, "/") + "/.omnis-pull/" + e.ModelKey + "@" + e.Revision + ".sha256"
}

// writeChecklistCommands 把校验清单分块写到节点(单个参数 ≤ 96KiB, 远低于 Linux 单参数 128KiB 上限),
// 最后一条输出写入结果的摘要供比对。
func writeChecklistCommands(cacheDir string, e *ModelEntry, checklist string) ([]string, error) {
	if err := checkCacheDir(cacheDir); err != nil {
		return nil, err
	}
	p := pullChecklistPath(cacheDir, e)
	cmds := []string{"mkdir -p " + strings.TrimRight(cacheDir, "/") + "/.omnis-pull && : > " + p}
	const chunk = 72 * 1024 // 编码后 96KiB
	for i := 0; i < len(checklist); i += chunk {
		end := i + chunk
		if end > len(checklist) {
			end = len(checklist)
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(checklist[i:end]))
		cmds = append(cmds, "printf %s "+b64+" | base64 -d >> "+p)
	}
	cmds = append(cmds, "sha256sum < "+p+" | cut -d' ' -f1")
	return cmds, nil
}

// pullRunCommand 启动拉取容器(detached): ModelScope 固定 revision 下载到 .partial → 逐文件 sha256 校验 →
// 写完成标记 → 原子改名。成功时容器以 0 退出且最后输出 @@done。
func pullRunCommand(cacheDir, pullImage string, cacheID int64, e *ModelEntry) (string, error) {
	dir, err := modelDir(cacheDir, e)
	if err != nil {
		return "", err
	}
	if !engineImagePattern.MatchString(pullImage) {
		return "", fmt.Errorf("拉取镜像须按 digest 固定")
	}
	if !repoPattern.MatchString(e.Repo) {
		return "", fmt.Errorf("repo 非法")
	}
	cd := strings.TrimRight(cacheDir, "/")
	m := pullChecklistPath(cacheDir, e)
	script := "set -eu; D=" + dir + "; P=" + dir + ".partial; M=" + m + "; mkdir -p \"$P\"; " +
		"modelscope download --model " + e.Repo + " --revision " + e.Revision + " --local_dir \"$P\"; " +
		"cd \"$P\"; sha256sum -c --strict --quiet \"$M\"; cp \"$M\" \"$P/" + markerFile + "\"; " +
		"cd /; rm -rf \"$D\"; mv \"$P\" \"$D\"; echo @@done"
	name := pullContainerName(e.ID)
	return fmt.Sprintf("docker rm -f %s >/dev/null 2>&1; docker run -d --name %s --label omnis.pull=%d -v %s:%s --entrypoint sh %s -c '%s' >/dev/null && echo @@started",
		name, name, cacheID, cd, cd, pullImage, script), nil
}

// pullStatusCommand 拉取容器状态 + 已下载字节: "@@pull <status> <exit>" / "@@pull absent"; "@@bytes <n>"。
func pullStatusCommand(cacheDir string, e *ModelEntry) (string, error) {
	dir, err := modelDir(cacheDir, e)
	if err != nil {
		return "", err
	}
	n := pullContainerName(e.ID)
	return "if s=$(docker inspect --type container -f '{{.State.Status}} {{.State.ExitCode}}' " + n + " 2>/dev/null); then echo \"@@pull $s\"; else echo '@@pull absent'; fi; " +
		"echo \"@@bytes $(du -sb " + dir + ".partial 2>/dev/null | cut -f1)\"", nil
}

// pullFinishCommand 读拉取容器日志末尾后删除容器。
func pullFinishCommand(modelID int64) string {
	n := pullContainerName(modelID)
	return "docker logs --tail 30 " + n + " 2>&1; docker rm -f " + n + " >/dev/null 2>&1; echo @@end"
}

// evictCommand 删除权重目录(含未完成的 .partial)。
func evictCommand(cacheDir string, e *ModelEntry) (string, error) {
	dir, err := modelDir(cacheDir, e)
	if err != nil {
		return "", err
	}
	return "rm -rf " + dir + " " + dir + ".partial && echo @@evicted", nil
}

// cacheAvailCommand 缓存区可用字节。
func cacheAvailCommand(cacheDir string) (string, error) {
	if err := checkCacheDir(cacheDir); err != nil {
		return "", err
	}
	return "mkdir -p " + cacheDir + " && df -B1 --output=avail " + cacheDir + " | tail -n 1", nil
}
