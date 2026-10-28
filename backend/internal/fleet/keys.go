package fleet

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"regexp"
	"strings"

	tfcrypto "tokenfactory/pkg/crypto"

	"golang.org/x/crypto/ssh"
)

// 节点管理密钥(docs/25 §25.4.2 ②): 每个节点独立的 ed25519 密钥对, 私钥 AES-256-GCM 加密入库,
// 公钥带 from="控制面IP" 限制写入节点 authorized_keys。单节点泄露不影响其他节点; 节点删除即作废。

// generateNodeKey 生成节点管理密钥, 返回 authorized_keys 格式公钥(不含 options)与加密后的私钥。
func generateNodeKey(nodeID int64, credKey []byte) (pubLine, encPriv string, err error) {
	if len(credKey) == 0 {
		return "", "", tfcrypto.ErrKeyNotConfigured
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	comment := fmt.Sprintf("omnis-node-%d", nodeID)
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	enc, err := tfcrypto.Encrypt(string(pem.EncodeToMemory(block)), credKey)
	if err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment
	return line, enc, nil
}

// loadNodeSigner 解密节点管理私钥。
func loadNodeSigner(encPriv string, credKey []byte) (ssh.Signer, error) {
	pemText, err := tfcrypto.Decrypt(encPriv, credKey)
	if err != nil {
		return nil, fmt.Errorf("节点管理私钥解密失败: %w", err)
	}
	return ssh.ParsePrivateKey([]byte(pemText))
}

var authorizedLineRe = regexp.MustCompile(`^[A-Za-z0-9 ="+/.,:_-]+$`)

// authorizedKeysLine 写入节点 authorized_keys 的完整行: 限制来源 IP, 禁止转发/代理/pty。
// 平台只执行非交互命令, 不需要 pty。
func authorizedKeysLine(pubLine string, controllerIPs []string) (string, error) {
	if len(controllerIPs) == 0 {
		return "", fmt.Errorf("fleet.controller_egress_ips 未配置")
	}
	for _, ip := range controllerIPs {
		if net.ParseIP(ip) == nil {
			if _, _, err := net.ParseCIDR(ip); err != nil {
				return "", fmt.Errorf("控制面出口 IP 非法: %q", ip)
			}
		}
	}
	line := fmt.Sprintf(`from="%s",no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-pty %s`,
		strings.Join(controllerIPs, ","), pubLine)
	if !authorizedLineRe.MatchString(line) {
		return "", fmt.Errorf("公钥行含非法字符")
	}
	return line, nil
}

// pubKeyBody 取 "ssh-ed25519 AAAA… comment" 的 base64 部分, 用于幂等写入时 grep。
func pubKeyBody(pubLine string) string {
	f := strings.Fields(pubLine)
	if len(f) < 2 {
		return ""
	}
	return f[1]
}
