// Command accessgw 访问网关: sshpiperd(v1.6.1) 的 gRPC 插件(docs/25 §25.6, T-085)。
//
// 部署: sshpiperd --port 2222 --server-key /etc/omnis/accessgw/host_ed25519 /usr/local/bin/accessgw \
//
//	--main-url http://<主服务内网>:<accessgw.internal_listen 端口> --upstream-key-file /etc/omnis/accessgw/upstream_ed25519
//
// 环境变量 ACCESSGW_HMAC_SECRET 提供 accessgw.hmac_secret(不走命令行, 避免出现在进程列表)。
// 插件本身只做 libplugin 回调到 pkg/accessgw.Gateway 的转接; 鉴权全部委托主服务内部接口, 主服务不可达时拒绝新连接。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	"tokenfactory/pkg/accessgw"

	"github.com/tg123/sshpiper/libplugin"
	"github.com/urfave/cli/v2"
	"golang.org/x/crypto/ssh"
)

func main() {
	libplugin.CreateAndRunPluginTemplate(&libplugin.PluginTemplate{
		Name:  "omnis-accessgw",
		Usage: "omnisline 托管租约访问网关(鉴权委托主服务内部接口)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "main-url", Usage: "主服务内部接口地址 http(s)://host:port", EnvVars: []string{"ACCESSGW_MAIN_URL"}, Required: true},
			&cli.StringFlag{Name: "upstream-key-file", Usage: "网关登录租约容器用的私钥(公钥即 accessgw.upstream_pubkey)",
				EnvVars: []string{"ACCESSGW_UPSTREAM_KEY_FILE"}, Required: true},
			&cli.StringFlag{Name: "upstream-user", Value: "root", EnvVars: []string{"ACCESSGW_UPSTREAM_USER"}},
		},
		CreateConfig: func(c *cli.Context) (*libplugin.SshPiperPluginConfig, error) {
			secret := os.Getenv("ACCESSGW_HMAC_SECRET")
			if len(secret) < 32 {
				return nil, errors.New("环境变量 ACCESSGW_HMAC_SECRET 未设置或不足 32 字节")
			}
			client, err := accessgw.NewClient(c.String("main-url"), []byte(secret), 5*time.Second)
			if err != nil {
				return nil, err
			}
			key, err := loadUpstreamKey(c.String("upstream-key-file"))
			if err != nil {
				return nil, err
			}
			gw := accessgw.NewGateway(client, slog.Default())
			go gw.Run(context.Background())
			cfg := NewPluginConfig(gw, key, c.String("upstream-user"))
			return &cfg, nil
		},
	})
}

func loadUpstreamKey(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s 权限过宽(%o), 须为 0400/0600", path, st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if _, err := ssh.ParsePrivateKey(raw); err != nil {
		return nil, fmt.Errorf("上游私钥无法解析(不支持口令保护): %w", err)
	}
	return raw, nil
}

// NewPluginConfig libplugin 回调 → Gateway。只开放 publickey 与 password 两种认证方式。
func NewPluginConfig(gw *accessgw.Gateway, upstreamKey []byte, upstreamUser string) libplugin.SshPiperPluginConfig {
	upstream := func(r *accessgw.Route) *libplugin.Upstream {
		// v1.6.1 中 host/port 字段已弃用, 用 uri(tcp://host:port, IPv6 带方括号)。上游主机密钥由 VerifyHostKey 回调校验
		// (该 RPC 优先于 known_hosts_data)。
		// 整机交付(T-089)由主服务指定上游账户(omnis-<lease_no>, 网关已校验格式); 容器交付用默认账户。
		user := upstreamUser
		if r.User != "" {
			user = r.User
		}
		return &libplugin.Upstream{Uri: "tcp://" + net.JoinHostPort(r.Host, strconv.Itoa(r.Port)), UserName: user,
			Auth: libplugin.CreatePrivateKeyAuth(upstreamKey)}
	}
	return libplugin.SshPiperPluginConfig{
		NextAuthMethodsCallback: func(libplugin.ConnMetadata) ([]string, error) {
			return []string{"publickey", "password"}, nil
		},
		PublicKeyCallback: func(conn libplugin.ConnMetadata, key []byte) (*libplugin.Upstream, error) {
			r, err := gw.AuthPublicKey(context.Background(), conn.UniqueID(), conn.User(), conn.RemoteAddr(), key)
			if err != nil {
				return nil, err
			}
			return upstream(r), nil
		},
		PasswordCallback: func(conn libplugin.ConnMetadata, password []byte) (*libplugin.Upstream, error) {
			r, err := gw.AuthPassword(context.Background(), conn.UniqueID(), conn.User(), conn.RemoteAddr(), password)
			if err != nil {
				return nil, err
			}
			return upstream(r), nil
		},
		VerifyHostKeyCallback: func(conn libplugin.ConnMetadata, _, _ string, key []byte) error {
			return gw.VerifyHostKey(conn.UniqueID(), key)
		},
		PipeStartCallback: func(conn libplugin.ConnMetadata) { gw.PipeStart(conn.UniqueID()) },
		PipeErrorCallback: func(conn libplugin.ConnMetadata, err error) { gw.PipeEnd(conn.UniqueID(), err) },
		PipeCreateErrorCallback: func(remoteAddr string, err error) {
			slog.Warn("建立管道失败", "remote", remoteAddr, "error", err)
		},
	}
}
