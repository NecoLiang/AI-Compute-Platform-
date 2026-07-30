package crypto

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// testKey 返回一个确定的 32 字节测试密钥。仅用于测试, 绝不可用于生产。
func testKey() []byte {
	k, err := ParseKeyHex(strings.Repeat("ab", KeySize))
	if err != nil { panic(err) }
	return k
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := testKey()
	cases := []struct{ name, plaintext string }{
		{"空字符串", ""},
		{"短ASCII", "root"},
		{"中文", "机房交付凭证：华东一区"},
		{"JSON交付信息", `{"ip_address":"10.0.0.1","ssh_port":22,"username":"root","password":"S3cr3t!"}`},
		{"含特殊字符", "p@$$w0rd\n\t\"\\'<>&"},
		{"长文本", strings.Repeat("0123456789", 500)},
		{"48位hex凭证", "0123456789abcdef0123456789abcdef0123456789abcdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, err := Encrypt(tc.plaintext, key)
			assert.NoError(t, err)
			if tc.plaintext != "" {
				assert.NotContains(t, ct, tc.plaintext, "密文中不得出现明文")
			}

			pt, err := Decrypt(ct, key)
			assert.NoError(t, err)
			assert.Equal(t, tc.plaintext, pt)
		})
	}
}

// 未配置密钥必须报错, 严禁降级为明文存储。
func TestEncrypt_EmptyKeyIsRefused(t *testing.T) {
	for _, key := range [][]byte{nil, {}} {
		ct, err := Encrypt("敏感凭证", key)
		assert.Error(t, err)
		assert.True(t, errors.Is(err, ErrKeyNotConfigured), "必须返回 ErrKeyNotConfigured")
		assert.Empty(t, ct, "报错时不得返回任何内容")
	}

	pt, err := Decrypt("whatever", nil)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrKeyNotConfigured))
	assert.Empty(t, pt)
}

func TestEncrypt_InvalidKeySize(t *testing.T) {
	for _, n := range []int{1, 15, 16, 24, 31, 33, 64} {
		key := make([]byte, n)
		ct, err := Encrypt("x", key)
		assert.Error(t, err, "长度 %d 必须被拒绝", n)
		assert.True(t, errors.Is(err, ErrInvalidKeySize), "长度 %d 应返回 ErrInvalidKeySize", n)
		assert.Empty(t, ct)
	}
}

// GCM 的 nonce 必须每次随机, 否则相同明文产生相同密文会泄露信息。
func TestEncrypt_NonceIsRandomPerCall(t *testing.T) {
	key := testKey()
	const plaintext = "同一份明文"
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		ct, err := Encrypt(plaintext, key)
		assert.NoError(t, err)
		assert.False(t, seen[ct], "相同明文加密两次得到相同密文, nonce 未随机化")
		seen[ct] = true

		// 每份密文都必须能独立解回原文
		pt, err := Decrypt(ct, key)
		assert.NoError(t, err)
		assert.Equal(t, plaintext, pt)
	}
}

// 密文被篡改必须认证失败, 绝不返回可疑明文。
func TestDecrypt_TamperIsDetected(t *testing.T) {
	key := testKey()
	ct, err := Encrypt("交付凭证不可篡改", key)
	assert.NoError(t, err)

	raw, err := base64.StdEncoding.DecodeString(ct)
	assert.NoError(t, err)

	// 逐个位置翻转一个 bit, 每次都必须被 GCM 认证拦住
	for i := range raw {
		tampered := make([]byte, len(raw))
		copy(tampered, raw)
		tampered[i] ^= 0x01

		pt, err := Decrypt(base64.StdEncoding.EncodeToString(tampered), key)
		assert.Error(t, err, "第 %d 字节被篡改却解密成功", i)
		assert.Empty(t, pt, "认证失败时不得返回明文")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	ct, err := Encrypt("敏感数据", testKey())
	assert.NoError(t, err)

	wrong, err := ParseKeyHex(strings.Repeat("cd", KeySize))
	assert.NoError(t, err)

	pt, err := Decrypt(ct, wrong)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidCiphertext))
	assert.Empty(t, pt)
}

func TestDecrypt_MalformedInput(t *testing.T) {
	key := testKey()
	cases := []struct{ name, in string }{
		{"空串", ""},
		{"非base64", "!!!not-base64!!!"},
		{"合法base64但过短", base64.StdEncoding.EncodeToString([]byte("short"))},
		{"仅nonce长度无tag", base64.StdEncoding.EncodeToString(make([]byte, 12))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pt, err := Decrypt(tc.in, key)
			assert.Error(t, err)
			assert.Empty(t, pt)
		})
	}
}

func TestParseKeyHex(t *testing.T) {
	t.Run("合法64位hex", func(t *testing.T) {
		key, err := ParseKeyHex(strings.Repeat("ab", KeySize))
		assert.NoError(t, err)
		assert.Len(t, key, KeySize)
	})

	t.Run("首尾空格被容错", func(t *testing.T) {
		key, err := ParseKeyHex("  " + strings.Repeat("ab", KeySize) + "\n")
		assert.NoError(t, err)
		assert.Len(t, key, KeySize)
	})

	t.Run("空串返回未配置错误", func(t *testing.T) {
		for _, s := range []string{"", "   ", "\t\n"} {
			key, err := ParseKeyHex(s)
			assert.Error(t, err)
			assert.True(t, errors.Is(err, ErrKeyNotConfigured))
			assert.Nil(t, key)
		}
	})

	t.Run("非法hex字符", func(t *testing.T) {
		key, err := ParseKeyHex(strings.Repeat("zz", KeySize))
		assert.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidKeySize))
		assert.Nil(t, key)
	})

	t.Run("长度不足或超出", func(t *testing.T) {
		for _, s := range []string{strings.Repeat("ab", 16), strings.Repeat("ab", 31), strings.Repeat("ab", 33)} {
			key, err := ParseKeyHex(s)
			assert.Error(t, err, "hex 长度 %d 必须被拒绝", len(s))
			assert.True(t, errors.Is(err, ErrInvalidKeySize))
			assert.Nil(t, key)
		}
	})

	t.Run("解析结果可直接用于加解密", func(t *testing.T) {
		key, err := ParseKeyHex(strings.Repeat("0f", KeySize))
		assert.NoError(t, err)
		ct, err := Encrypt("端到端", key)
		assert.NoError(t, err)
		pt, err := Decrypt(ct, key)
		assert.NoError(t, err)
		assert.Equal(t, "端到端", pt)
	})
}

func TestRandomHex(t *testing.T) {
	t.Run("长度为2n", func(t *testing.T) {
		for _, n := range []int{1, 8, 16, 24, 32} {
			s, err := RandomHex(n)
			assert.NoError(t, err)
			assert.Len(t, s, 2*n)
		}
	})

	t.Run("输出为小写hex", func(t *testing.T) {
		s, err := RandomHex(32)
		assert.NoError(t, err)
		for i := 0; i < len(s); i++ {
			c := s[i]
			assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'), "非法字符 %q", c)
		}
	})

	t.Run("不重复", func(t *testing.T) {
		seen := map[string]bool{}
		for i := 0; i < 500; i++ {
			s, err := RandomHex(16)
			assert.NoError(t, err)
			assert.False(t, seen[s], "RandomHex 出现重复, 随机性不足")
			seen[s] = true
		}
	})

	t.Run("非正数报错", func(t *testing.T) {
		for _, n := range []int{0, -1, -16} {
			s, err := RandomHex(n)
			assert.Error(t, err)
			assert.Empty(t, s)
		}
	})
}
