package fleet

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeNode 测试用 SSH 服务器: 真实的 x/crypto/ssh 握手与认证(密码 + 公钥), exec 请求按命令模板返回
// 按 nvidia-smi / docker 真实格式整理的 2×H100 输出。只在测试中使用, 监听 127.0.0.1。

type fakeReply struct {
	Stdout, Stderr string
	Code           int
}

type fakeNode struct {
	t        *testing.T
	ln       net.Listener
	password string

	mu         sync.Mutex
	hostKey    ssh.Signer
	authorized []ssh.PublicKey
	overrides  map[string]fakeReply // 命令前缀 → 覆盖返回
	hooks      map[string]func()
	cmds       []string
	pwLogins   int
}

const (
	fakeUUID0 = "GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11"
	fakeUUID1 = "GPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d"
)

func fakeGPUList() string {
	lines := strings.SplitN(h100GPUList, "\n", 3)
	return lines[0] + "\n" + lines[1] + "\n"
}

const fakeTopo = "\t\x1b[4mGPU0\tGPU1\tNIC0\tCPU Affinity\tNUMA Affinity\tGPU NUMA ID\x1b[0m\n" +
	"GPU0\t X \tNV18\tPXB\t0-55,112-167\t0\t\tN/A\n" +
	"GPU1\tNV18\t X \tSYS\t0-55,112-167\t0\t\tN/A\n" +
	"NIC0\tPXB\tSYS\t X \n\nLegend:\n\n  X    = Self\n  NV#  = Connection traversing a bonded set of # NVLinks\n"

const fakeDockerInfo = `{"ID":"7c1f","Containers":3,"Driver":"overlay2","DriverStatus":[["Backing Filesystem","xfs"],["Supports d_type","true"]],"ServerVersion":"24.0.7","DockerRootDir":"/data/docker","CgroupVersion":"2","Runtimes":{"nvidia":{"path":"nvidia-container-runtime"},"runc":{"path":"runc"}}}` + "\n"

const fakeHostInfo = "@@nproc\n224\n@@mem\nMemTotal:       2113401588 kB\n@@arch\nx86_64\n@@kernel\n5.15.0-113-generic\n" +
	"@@os\nPRETTY_NAME=\"Ubuntu 22.04.4 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"22.04\"\nID=ubuntu\nID_LIKE=debian\n" +
	"@@df\nFilesystem     1073741824-blocks  Used Available Capacity Mounted on\n/dev/nvme0n1p1             13968G  1022G    12946G       8% /data\n"

const fakeSMIHeader = "Tue Oct 28 10:00:00 2026\n+---------------------------------------------------------------------------------------+\n" +
	"| NVIDIA-SMI 535.104.05             Driver Version: 535.104.05   CUDA Version: 12.2     |\n"

var fakeDefaults = []struct {
	prefix string
	exact  bool
	reply  fakeReply
}{
	{cmdGPUList, true, fakeReply{Stdout: fakeGPUList()}},
	{cmdSMIHeader, true, fakeReply{Stdout: fakeSMIHeader}},
	{cmdTopo, true, fakeReply{Stdout: fakeTopo}},
	{cmdECC, true, fakeReply{Stdout: "0, 0, No\n1, 0, No\n"}},
	{cmdRemap, true, fakeReply{Stdout: fakeUUID0 + ", 0, 0\n" + fakeUUID1 + ", 0, 0\n"}},
	{cmdDocker, true, fakeReply{Stdout: fakeDockerInfo}},
	{cmdPorts, true, fakeReply{Stdout: "LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*\nLISTEN 0      4096            [::]:22            [::]:*\n"}},
	{cmdLight, true, fakeReply{Stdout: "0, " + fakeUUID0 + ", 37, 12, 41, 0\n1, " + fakeUUID1 + ", 3, 0, 33, 0\n"}},
	{cmdDockerAlive, true, fakeReply{}},
	{`echo "@@id`, false, fakeReply{Stdout: "@@id uid=0 groups=root\n"}},
	{"echo @@nproc", false, fakeReply{Stdout: fakeHostInfo}},
	{"docker pull -q ", false, fakeReply{Stdout: "registry.example.com/omnis/probe@sha256:" + strings.Repeat("ab", 32) + "\n"}},
	{"docker run --rm --network none --storage-opt", false, fakeReply{}},
	{"docker run --rm --network none --gpus", false, fakeReply{Stdout: "GPU 0: NVIDIA H100 80GB HBM3 (UUID: " + fakeUUID0 + ")\nGPU 1: NVIDIA H100 80GB HBM3 (UUID: " + fakeUUID1 + ")\n"}},
	{"docker run -d --rm --name omnis-probe-", false, fakeReply{Stdout: "@@up\n"}},
	{"docker rm -f omnis-probe-", false, fakeReply{}},
	{"docker run --rm --name omnis-probe-", false, fakeReply{Stdout: "some torch warning\n" +
		`{"results":[{"index":0,"uuid":"` + fakeUUID0 + `","ok":true,"tflops":648.2,"error":""},{"index":1,"uuid":"` + fakeUUID1 + `","ok":true,"tflops":651.7,"error":""}]}` + "\n"}},
	{"for i in $(seq", false, fakeReply{Stdout: "@@t1\n" + fakeUUID0 + ", 0x0000000000000004\n" + fakeUUID1 + ", 0x0000000000000000\n"}},
}

func newFakeNode(t *testing.T, password string) *fakeNode {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNode{t: t, ln: ln, password: password, overrides: map[string]fakeReply{}, hooks: map[string]func(){}}
	f.RotateHostKey()
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeNode) Port() int { return f.ln.Addr().(*net.TCPAddr).Port }

// RotateHostKey 模拟重装系统: 换一把主机密钥, 返回新指纹。
func (f *fakeNode) RotateHostKey() string {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.hostKey = s
	f.mu.Unlock()
	return ssh.FingerprintSHA256(s.PublicKey())
}

func (f *fakeNode) Fingerprint() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ssh.FingerprintSHA256(f.hostKey.PublicKey())
}

func (f *fakeNode) Override(prefix string, r fakeReply) {
	f.mu.Lock()
	f.overrides[prefix] = r
	f.mu.Unlock()
}

func (f *fakeNode) ClearOverrides() {
	f.mu.Lock()
	f.overrides = map[string]fakeReply{}
	f.mu.Unlock()
}

func (f *fakeNode) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeNode) AuthorizedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.authorized)
}

func (f *fakeNode) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeNode) handle(c net.Conn) {
	defer c.Close()
	f.mu.Lock()
	hk := f.hostKey
	f.mu.Unlock()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if f.password != "" && string(pw) == f.password {
				f.mu.Lock()
				f.pwLogins++
				f.mu.Unlock()
				return nil, nil
			}
			return nil, fmt.Errorf("密码错误")
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, k := range f.authorized {
				if bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil, nil
				}
			}
			return nil, fmt.Errorf("公钥未授权")
		},
	}
	cfg.AddHostKey(hk)
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go f.session(ch, creqs)
	}
}

func (f *fakeNode) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		if len(req.Payload) < 4 {
			_ = req.Reply(false, nil)
			return
		}
		n := binary.BigEndian.Uint32(req.Payload)
		cmd := string(req.Payload[4 : 4+n])
		_ = req.Reply(true, nil)
		r := f.exec(cmd)
		_, _ = ch.Write([]byte(r.Stdout))
		_, _ = ch.Stderr().Write([]byte(r.Stderr))
		status := make([]byte, 4)
		binary.BigEndian.PutUint32(status, uint32(r.Code))
		_, _ = ch.SendRequest("exit-status", false, status)
		return
	}
}

func (f *fakeNode) exec(cmd string) fakeReply {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	var hook func()
	for p, fn := range f.hooks {
		if strings.HasPrefix(cmd, p) {
			hook = fn
		}
	}
	if hook != nil {
		f.mu.Unlock()
		hook()
		f.mu.Lock()
	}
	for p, r := range f.overrides {
		if strings.HasPrefix(cmd, p) {
			f.mu.Unlock()
			return r
		}
	}
	f.mu.Unlock()
	if strings.HasPrefix(cmd, "umask 077;") {
		return f.installKey(cmd)
	}
	for _, d := range fakeDefaults {
		if (d.exact && cmd == d.prefix) || (!d.exact && strings.HasPrefix(cmd, d.prefix)) {
			return d.reply
		}
	}
	return fakeReply{Stderr: "sh: 1: " + strings.Fields(cmd + " x")[0] + ": not found\n", Code: 127}
}

// installKey 按真实 shell 语义解释 installKeyCommand: 取出 printf 写入的公钥行加入授权列表。
func (f *fakeNode) installKey(cmd string) fakeReply {
	const marker = `printf '%s\n' '`
	i := strings.Index(cmd, marker)
	if i < 0 {
		return fakeReply{Stderr: "bad install command", Code: 2}
	}
	rest := cmd[i+len(marker):]
	j := strings.Index(rest, "' >>")
	if j < 0 {
		return fakeReply{Stderr: "bad install command", Code: 2}
	}
	key, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(rest[:j]))
	if err != nil {
		return fakeReply{Stderr: err.Error(), Code: 1}
	}
	if len(opts) == 0 || !strings.HasPrefix(opts[0], "from=") {
		return fakeReply{Stderr: "missing from= option", Code: 1}
	}
	f.mu.Lock()
	dup := false
	for _, k := range f.authorized {
		dup = dup || bytes.Equal(k.Marshal(), key.Marshal())
	}
	if !dup {
		f.authorized = append(f.authorized, key)
	}
	f.mu.Unlock()
	return fakeReply{Stdout: "@@installed\n"}
}

// OnCommand 命令执行前回调(模拟探活进行中外部状态变化)。
func (f *fakeNode) OnCommand(prefix string, fn func()) {
	f.mu.Lock()
	f.hooks[prefix] = fn
	f.mu.Unlock()
}
