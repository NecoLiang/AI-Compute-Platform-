package fleet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// 平台镜像冒烟(deploy/images, .github/workflows/fleet-images.yml): 在本机 Docker(Linux, 无 GPU)上用生产的命令模板
// 原样执行一遍不依赖 GPU 的部分, 验证镜像满足模板的全部假设。依赖 GPU 的 P9/P13 与 --storage-opt 配额(P10)
// 只能在真机上验收, 这里只检查镜像内的前提(CUDA 版 torch、压测脚本可编译、dd 小写入可用)。
//
//	SMOKE_PROBE_IMAGE  探针镜像 ref@sha256(本机已拉取或可拉取)
//	SMOKE_BASE_IMAGE   租约基础镜像 ref@sha256
//
// 会改动本机 iptables DOCKER-USER(端口段 47000-47099, 结束时删除), 只应在一次性 CI 机器或测试 VM 上运行。
func TestFleetImagesSmoke(t *testing.T) {
	probe, base := os.Getenv("SMOKE_PROBE_IMAGE"), os.Getenv("SMOKE_BASE_IMAGE")
	if probe == "" || base == "" {
		t.Skip("set SMOKE_PROBE_IMAGE and SMOKE_BASE_IMAGE to run the fleet image smoke test")
	}
	require.NoError(t, checkImage(probe))
	require.NoError(t, checkImage(base))
	host := func(t *testing.T, cmd string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
		return string(out), err
	}
	mustHost := func(t *testing.T, cmd string) string {
		t.Helper()
		out, err := host(t, cmd)
		require.NoError(t, err, out)
		return out
	}

	t.Run("probe_python_torch", func(t *testing.T) {
		out := mustHost(t, "docker run --rm --network none "+probe+` python3 -c 'import torch; print("@@cuda", torch.version.cuda)'`)
		assert.Contains(t, out, "@@cuda 12.8")
		// P13 压测脚本在探针镜像的 python3 下能编译(运行需要 GPU)。
		out = mustHost(t, "docker run --rm --network none "+probe+" "+
			pyExec(`compile(__import__("base64").b64decode("`+base64.StdEncoding.EncodeToString([]byte(benchScript))+`").decode(), "bench", "exec"); print("@@compiled")`))
		assert.Contains(t, out, "@@compiled")
	})

	t.Run("probe_quota_command_shape", func(t *testing.T) {
		// 本机多为 overlay2 over ext4: --storage-opt 直接报错或被忽略; 只要求不是"小写入失败"(镜像内 sh/dd 可用)。
		cmd, err := quotaCheckCommand(probe)
		require.NoError(t, err)
		out, _ := host(t, cmd)
		t.Logf("P10 on this host: %s", strings.TrimSpace(out))
		assert.NotContains(t, out, "@@small-fail")
		out = mustHost(t, "docker run --rm --network none "+probe+
			` sh -c 'dd if=/dev/zero of=/tmp/q bs=1M count=8 >/dev/null 2>&1 && echo @@ok'`)
		assert.Contains(t, out, "@@ok")
	})

	t.Run("probe_port_listener", func(t *testing.T) {
		name := "omnis-probe-1-" + randHex(t, 4)
		cmd, err := portListenerCommand(probe, name, 47010)
		require.NoError(t, err)
		clean, err := cleanupCommand(name)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = host(t, clean) })
		out := mustHost(t, cmd)
		assert.Contains(t, out, "@@up")
		c, err := net.DialTimeout("tcp", "127.0.0.1:47010", 3*time.Second)
		require.NoError(t, err)
		_ = c.Close()
	})

	t.Run("probe_host_root_chroot", func(t *testing.T) {
		cmd, err := hostRootCommand(probe, "echo \"@@root $(id -u)\"; command -v sh >/dev/null && echo @@hostsh")
		require.NoError(t, err)
		out := mustHost(t, cmd)
		assert.Contains(t, out, "@@root 0")
		assert.Contains(t, out, "@@hostsh")
	})

	t.Run("lease_container_ssh_and_mining_scan", func(t *testing.T) {
		out := mustHost(t, "docker run --rm --network none "+base+` sh -c 'ls /etc/ssh/ssh_host_* 2>/dev/null | wc -l'`)
		assert.Equal(t, "0", strings.TrimSpace(out), "base image must not ship sshd host keys")

		lease, err := newLeaseNo()
		require.NoError(t, err)
		c, v, nw := containerNameOf(lease), volumeNameOf(lease), networkNameOf(lease)
		t.Cleanup(func() {
			_, _ = host(t, "docker rm -f "+c+" >/dev/null 2>&1; docker volume rm "+v+" >/dev/null 2>&1; docker network rm "+nw+" >/dev/null 2>&1")
		})
		// 本机经端口映射连入时, 容器 sshd 看到的来源是租约网络的网关地址: 预先建网络(与 createContainerCommand 同名同标签,
		// 其 inspect || create 会沿用), 把该地址当作"网关出口 IP"写进 authorized_keys 的 from=。
		mustHost(t, "docker network create --label omnis.lease="+lease+" "+nw+" >/dev/null")
		gwIP := strings.TrimSpace(mustHost(t, "docker network inspect -f '{{(index .IPAM.Config 0).Gateway}}' "+nw))
		require.NotNil(t, net.ParseIP(gwIP), gwIP)

		up := newSigner(t)
		init, err := containerInitScript(string(ssh.MarshalAuthorizedKey(up.PublicKey())), []string{gwIP})
		require.NoError(t, err)
		uuids := []string{"GPU-" + randHex(t, 4) + "-" + randHex(t, 2) + "-" + randHex(t, 2) + "-" + randHex(t, 2) + "-" + randHex(t, 6)}
		const port = 47020
		cmd, err := createContainerCommand(leaseContainerSpec{LeaseNo: lease, Image: base, UUIDs: uuids, CPUs: 2, MemGB: 2,
			Port: port, InitShell: init})
		require.NoError(t, err)
		g, err := gpusFlag(uuids)
		require.NoError(t, err)
		noGPU := strings.Replace(cmd, " --gpus "+g, "", 1) // 本机无 NVIDIA 运行时: 只去掉挂卡参数, 其余与生产命令一致
		require.NotEqual(t, cmd, noGPU)
		assert.Contains(t, mustHost(t, noGPU), "@@created")

		hk, err := hostKeyCommand(lease)
		require.NoError(t, err)
		fp, err := parseHostKeyFP(mustHost(t, hk))
		require.NoError(t, err)

		dial := func(auth ssh.AuthMethod) (*ssh.Client, error) {
			return ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port), &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{auth},
				Timeout: 10 * time.Second, HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
					if got := ssh.FingerprintSHA256(k); got != fp {
						return fmt.Errorf("host key %s != %s", got, fp)
					}
					return nil
				}})
		}
		var cl *ssh.Client
		require.Eventually(t, func() bool { cl, err = dial(ssh.PublicKeys(up)); return err == nil }, 30*time.Second, time.Second,
			"upstream key login: %v", err)
		defer cl.Close()
		run := func(cmd string) string {
			s, err := cl.NewSession()
			require.NoError(t, err)
			defer s.Close()
			b, err := s.CombinedOutput(cmd)
			require.NoError(t, err, string(b))
			return string(b)
		}
		// ssh 登录后的非交互 shell 不继承镜像 ENV: PATH 与 CUDA 库路径来自 /etc/environment。
		got := run(`command -v python3 pip >/dev/null && python3 -c 'import torch; print("@@torch", torch.__version__)' && pwd`)
		assert.Contains(t, got, "@@torch 2.7.1")
		assert.Contains(t, run("echo $LD_LIBRARY_PATH"), "/usr/local/nvidia/lib64")

		_, err = dial(ssh.PublicKeys(newSigner(t)))
		require.Error(t, err, "only the gateway upstream key may log in")
		_, err = dial(ssh.Password("x"))
		require.Error(t, err, "password auth must be disabled")

		scan, err := miningScanCommand(probe, lease)
		require.NoError(t, err)
		out = mustHost(t, scan)
		for _, want := range []string{"@@top", "sshd", "@@tcp", "@@end"} {
			assert.Contains(t, out, want)
		}
		assert.Regexp(t, `:0016 [0-9A-F:]+ 0A `, out, "sshd listening on :22 must appear in the container's /proc/net/tcp")
	})

	t.Run("probe_firewall_rules", func(t *testing.T) {
		const start, end = 47000, 47099
		t.Cleanup(func() {
			_, _ = host(t, "docker run --rm --network host --cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW --entrypoint /bin/sh "+probe+
				` -c 'for I in iptables-nft iptables-legacy; do $I -w -S DOCKER-USER 2>/dev/null | grep -F -- "--ctorigdstport 47000:47099 " | sed "s/^-A /-D /" | while read -r r; do $I -w $r; done; done'`)
		})
		cmd, err := firewallCommand(probe, []string{"203.0.113.7", "198.51.100.9"}, start, end)
		require.NoError(t, err)
		assert.Contains(t, mustHost(t, cmd), "@@applied")
		assert.Contains(t, mustHost(t, cmd), "@@present", "second run must find exactly the expected rules")
		// 换网关 IP 后旧规则被整体替换。
		cmd2, err := firewallCommand(probe, []string{"203.0.113.8"}, start, end)
		require.NoError(t, err)
		assert.Contains(t, mustHost(t, cmd2), "@@applied")
		assert.Contains(t, mustHost(t, cmd2), "@@present")
		assert.Contains(t, mustHost(t, cmd), "@@applied")
	})
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}
