package db

import "testing"

// DSN 缺 loc 会造成 8 小时时间偏移(订单一创建就被判超时), 因此这段修正逻辑必须有测试兜住。
func TestNormalizeDSN(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantLoc   string
		wantParse string
		wantFixed bool
	}{
		{
			name:      "缺 loc 与 parseTime 时补齐",
			in:        "root:pw@tcp(127.0.0.1:3306)/tokenfactory",
			wantLoc:   "Asia/Shanghai",
			wantParse: "true",
			wantFixed: true,
		},
		{
			name:      "有 parseTime 但缺 loc",
			in:        "root:pw@tcp(127.0.0.1:3306)/tokenfactory?parseTime=true&charset=utf8mb4",
			wantLoc:   "Asia/Shanghai",
			wantParse: "true",
			wantFixed: true,
		},
		{
			name:      "已完整时不改动",
			in:        "root:pw@tcp(127.0.0.1:3306)/tokenfactory?parseTime=true&loc=Asia%2FShanghai",
			wantLoc:   "Asia/Shanghai",
			wantParse: "true",
			wantFixed: false,
		},
		{
			name:      "显式指定其他时区时不覆盖",
			in:        "root:pw@tcp(127.0.0.1:3306)/tokenfactory?parseTime=true&loc=UTC",
			wantLoc:   "UTC",
			wantParse: "true",
			wantFixed: false,
		},
		{
			name:      "parseTime=false 会被强制改为 true",
			in:        "root:pw@tcp(127.0.0.1:3306)/tokenfactory?parseTime=false&loc=UTC",
			wantLoc:   "UTC",
			wantParse: "true",
			wantFixed: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, warns := normalizeDSN(c.in)
			q := queryOf(t, got)

			if q["loc"] != c.wantLoc {
				t.Errorf("loc = %q, 期望 %q (DSN: %s)", q["loc"], c.wantLoc, got)
			}
			if q["parseTime"] != c.wantParse {
				t.Errorf("parseTime = %q, 期望 %q", q["parseTime"], c.wantParse)
			}
			if fixed := len(warns) > 0; fixed != c.wantFixed {
				t.Errorf("是否发生修正 = %v, 期望 %v (warns=%v)", fixed, c.wantFixed, warns)
			}
		})
	}
}

// 用户名/密码中的特殊字符与 tcp(...) 里的冒号不应把 DSN 切坏。
func TestNormalizeDSN_PreservesCredentials(t *testing.T) {
	in := "root:p@ss/w0rd!@tcp(10.0.0.1:3306)/db1"
	got, _ := normalizeDSN(in)

	if want := "root:p@ss/w0rd!@tcp(10.0.0.1:3306)/db1?"; len(got) < len(want) || got[:len(want)] != want {
		t.Errorf("凭据或库名被破坏: %s", got)
	}
	if q := queryOf(t, got); q["loc"] != "Asia/Shanghai" {
		t.Errorf("loc 未补齐: %s", got)
	}
}

// 形态异常的 DSN 原样返回, 交给驱动去报错, 不在这里吞掉。
func TestNormalizeDSN_MalformedPassthrough(t *testing.T) {
	in := "这不是一个合法DSN"
	if got, warns := normalizeDSN(in); got != in || len(warns) != 0 {
		t.Errorf("异常 DSN 应原样返回, got=%q warns=%v", got, warns)
	}
}

func queryOf(t *testing.T, dsn string) map[string]string {
	t.Helper()
	out := map[string]string{}
	i := -1
	for k := len(dsn) - 1; k >= 0; k-- {
		if dsn[k] == '?' { i = k; break }
	}
	if i < 0 { return out }
	for _, kv := range splitAll(dsn[i+1:], '&') {
		for p := 0; p < len(kv); p++ {
			if kv[p] == '=' {
				out[kv[:p]] = unescape(kv[p+1:])
				break
			}
		}
	}
	return out
}

func splitAll(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep { out = append(out, s[start:i]); start = i + 1 }
	}
	return append(out, s[start:])
}

func unescape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			out = append(out, hexVal(s[i+1])<<4|hexVal(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

func hexVal(b byte) byte {
	switch {
	case b >= '0' && b <= '9': return b - '0'
	case b >= 'a' && b <= 'f': return b - 'a' + 10
	case b >= 'A' && b <= 'F': return b - 'A' + 10
	}
	return 0
}
