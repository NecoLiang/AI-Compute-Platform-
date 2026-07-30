package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeySize 是 AES-256 要求的密钥长度(字节)。
const KeySize = 32

// ErrKeyNotConfigured 在未配置凭证加密密钥时返回。调用方必须处理该错误并中止流程,
// 严禁降级为明文存储。
var ErrKeyNotConfigured = errors.New("凭证加密密钥未配置: 需在 config.yaml security.credential_key 配置32字节(64位hex)密钥，生产环境应从 KMS 注入")

// ErrInvalidKeySize 在密钥长度不等于 32 字节时返回。
var ErrInvalidKeySize = errors.New("凭证加密密钥长度非法: AES-256 要求 32 字节(64位hex)")

// ErrInvalidCiphertext 在密文格式非法或长度不足时返回。
var ErrInvalidCiphertext = errors.New("密文格式非法或已损坏")

// checkKey 校验密钥, 空 key 与错误长度 key 分别返回明确错误。
func checkKey(key []byte) error {
	if len(key) == 0 { return ErrKeyNotConfigured }
	if len(key) != KeySize { return fmt.Errorf("%w: 当前 %d 字节", ErrInvalidKeySize, len(key)) }
	return nil
}

// Encrypt 用 AES-256-GCM 加密。key 为空时返回 ErrKeyNotConfigured, 调用方必须处理,
// 禁止降级存明文。nonce 每次由 crypto/rand 随机生成并前置拼接在密文之前,
// 最终输出为 base64(standard) 编码的 nonce||ciphertext||tag。
func Encrypt(plaintext string, key []byte) (string, error) {
	if err := checkKey(key); err != nil { return "", err }

	block, err := aes.NewCipher(key)
	if err != nil { return "", err }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return "", err }

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil { return "", err }

	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt 解密 Encrypt 的输出。key 为空时返回 ErrKeyNotConfigured。
// 密文被篡改或 key 不匹配时 GCM 认证失败并返回错误, 绝不返回可疑明文。
func Decrypt(ciphertext string, key []byte) (string, error) {
	if err := checkKey(key); err != nil { return "", err }

	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil { return "", fmt.Errorf("%w: %v", ErrInvalidCiphertext, err) }

	block, err := aes.NewCipher(key)
	if err != nil { return "", err }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return "", err }

	if len(raw) < gcm.NonceSize()+gcm.Overhead() { return "", ErrInvalidCiphertext }
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]

	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil { return "", fmt.Errorf("%w: 认证失败(密钥不匹配或密文被篡改)", ErrInvalidCiphertext) }
	return string(plain), nil
}

// ParseKeyHex 把配置里的 64 位 hex 字符串解析成 32 字节密钥。
// 空字符串返回 ErrKeyNotConfigured, 便于配置未填时在启动/使用处得到明确提示。
func ParseKeyHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" { return nil, ErrKeyNotConfigured }
	key, err := hex.DecodeString(s)
	if err != nil { return nil, fmt.Errorf("%w: 非法 hex 字符串: %v", ErrInvalidKeySize, err) }
	if len(key) != KeySize { return nil, fmt.Errorf("%w: 当前 %d 字节", ErrInvalidKeySize, len(key)) }
	return key, nil
}

// RandomHex 生成 n 字节的密码学安全随机数并返回其 hex 编码(长度为 2n)。
// 统一走 crypto/rand, 禁止 math/rand。
func RandomHex(n int) (string, error) {
	if n <= 0 { return "", errors.New("随机字节数必须大于 0") }
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil { return "", err }
	return hex.EncodeToString(b), nil
}
