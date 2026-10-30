package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tokenfactory/pkg/errcode"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// 买家连接与运营检索(T-086)集成测试: MySQL(TEST_MYSQL_DSN), 缺失即跳过。

func authLine(t *testing.T, pub any, comment string) string {
	t.Helper()
	k, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	if comment != "" {
		line += " " + comment
	}
	return line
}

func wireLine(t *testing.T, v any) string {
	t.Helper()
	b := ssh.Marshal(v)
	k, err := ssh.ParsePublicKey(b)
	require.NoError(t, err)
	return k.Type() + " " + base64.StdEncoding.EncodeToString(b)
}

func TestParseBuyerKey(t *testing.T) {
	ed := newSigner(t)
	edLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ed.PublicKey())))
	ec, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	weak, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	strong, err := rsa.GenerateKey(rand.Reader, 3072)
	require.NoError(t, err)

	pk, err := parseBuyerKey("  " + edLine + " alice@laptop \n")
	require.NoError(t, err)
	assert.Equal(t, edLine, pk.canonical, "comment and whitespace are dropped")
	assert.Equal(t, fpOf(ed), pk.fingerprint)
	assert.Equal(t, "alice@laptop", pk.comment)
	for _, line := range []string{authLine(t, &ec.PublicKey, ""), authLine(t, &strong.PublicKey, "x")} {
		_, err := parseBuyerKey(line)
		require.NoError(t, err, line[:20])
	}

	// DSA / 安全密钥 / 证书 / 弱 RSA / 带选项 / 多把 / 垃圾: 40001。
	dsa := wireLine(t, struct {
		Name       string
		P, Q, G, Y *big.Int
	}{ssh.KeyAlgoDSA, new(big.Int).SetBit(big.NewInt(1), 1023, 1), new(big.Int).SetBit(big.NewInt(1), 159, 1), big.NewInt(4), big.NewInt(8)})
	sk := wireLine(t, struct {
		Name string
		Key  []byte
		App  string
	}{ssh.KeyAlgoSKED25519, make([]byte, 32), "ssh:"})
	ca := newSigner(t)
	cert := &ssh.Certificate{Key: ed.PublicKey(), CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	require.NoError(t, cert.SignCert(rand.Reader, ca))
	certLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert)))
	for name, line := range map[string]string{
		"dsa": dsa, "sk": sk, "cert": certLine, "rsa2048": authLine(t, &weak.PublicKey, ""),
		"options": `from="10.0.0.1" ` + edLine, "two": edLine + "\n" + authLine(t, &ec.PublicKey, ""),
		"garbage": "ssh-ed25519 AAAA", "empty": "  ",
	} {
		_, err := parseBuyerKey(line)
		assert.Equal(t, errcode.ParamInvalid, codeOf(err), name)
	}

	n, err := keyName("", strings.Repeat("长", 80))
	require.NoError(t, err)
	assert.Equal(t, 64, len([]rune(n)))
	n, err = keyName(" ", "")
	require.NoError(t, err)
	assert.Equal(t, defaultKeyName, n)
	_, err = keyName(strings.Repeat("a", 65), "")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
}

func TestBuyerKeysLifecycle(t *testing.T) {
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s)) // 租约进入 active
	const buyer = int64(21)
	ed := newSigner(t)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ed.PublicKey()))) + " alice@laptop"

	k, err := e.s.AddBuyerKey(buyer, "", line, "198.51.100.1")
	require.NoError(t, err)
	assert.Equal(t, "alice@laptop", k.Name, "name defaults to the key comment")
	assert.Equal(t, "ssh-ed25519", k.KeyType)
	assert.Equal(t, fpOf(ed), k.Fingerprint)
	assert.NotContains(t, k.PublicKey, "alice")
	_, err = e.s.AddBuyerKey(buyer, "dup", line, "")
	assert.Equal(t, errcode.Conflict, codeOf(err))
	_, err = e.s.AddBuyerKey(buyer, strings.Repeat("名", 65), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))), "")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))

	// 同一把公钥可以被不同买家各自添加。
	e.db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (99,'13900000099','x')`)
	_, err = e.s.AddBuyerKey(99, "", line, "")
	require.NoError(t, err)

	// 上限 10 把。
	for i := 1; i < maxBuyerKeys; i++ {
		_, err := e.s.AddBuyerKey(buyer, fmt.Sprintf("k%d", i), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))), "")
		require.NoError(t, err)
	}
	_, err = e.s.AddBuyerKey(buyer, "", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))), "")
	assert.Equal(t, errcode.Conflict, codeOf(err))
	list, err := e.s.ListBuyerKeys(buyer)
	require.NoError(t, err)
	assert.Len(t, list, maxBuyerKeys)

	// 删除: 他人的公钥 40400; 本人删除后刷新在租租约 access_changed_at 并写审计。
	assert.Equal(t, errcode.NotFound, codeOf(e.s.DeleteBuyerKey(99, k.ID, "")))
	e.db.MustExec(`UPDATE leases SET access_changed_at=NOW() - INTERVAL 1 HOUR WHERE id=?`, e.lease.ID)
	require.NoError(t, e.s.DeleteBuyerKey(buyer, k.ID, "198.51.100.1"))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM leases WHERE id=? AND access_changed_at > NOW() - INTERVAL 1 MINUTE`, e.lease.ID))
	assert.Equal(t, errcode.NotFound, codeOf(e.s.DeleteBuyerKey(buyer, k.ID, "")), "already deleted")
	// 删钥只拦新登录, 不断开已建立的会话(不入队 kill_sessions, 租约仍 active; 待业务负责人确认的默认口径)。
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='kill_sessions'`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM leases WHERE id=? AND state='active'`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM audit_logs WHERE action='buyer_ssh_key.revoke' AND target_id=?`, k.ID))
	assert.Equal(t, maxBuyerKeys+1, countRows(t, e.db, `SELECT COUNT(*) FROM audit_logs WHERE action='buyer_ssh_key.add'`))
	list, err = e.s.ListBuyerKeys(buyer)
	require.NoError(t, err)
	assert.Len(t, list, maxBuyerKeys-1)

	// 重新添加已删除的同一把: 复用原行, 恢复为有效。
	k2, err := e.s.AddBuyerKey(buyer, "again", line, "")
	require.NoError(t, err)
	assert.Equal(t, k.ID, k2.ID)
	assert.Equal(t, "again", k2.Name)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM buyer_ssh_keys WHERE user_id=? AND fingerprint=? AND revoked_at IS NULL`, buyer, fpOf(ed)))
}

func TestOrderAccessAndSessions(t *testing.T) {
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s))
	const buyer = int64(21)
	no := e.order.OrderNo

	a, err := e.s.OrderAccess(buyer, no)
	require.NoError(t, err)
	assert.Equal(t, "gw.test", a.GatewayHost)
	assert.Equal(t, 2222, a.GatewayPort)
	assert.Equal(t, e.lease.LeaseNo, a.Login)
	assert.Equal(t, LeaseActive, a.LeaseState)
	require.NotNil(t, a.AccessUntil)
	assert.True(t, a.AccessUntil.After(time.Now()))
	assert.Equal(t, []string{"publickey", "password"}, a.AuthMethods)

	e.db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (99,'13900000099','x')`)
	_, err = e.s.OrderAccess(99, no)
	assert.Equal(t, errcode.Forbidden, codeOf(err))
	_, err = e.s.OrderAccess(buyer, "NOPE-not-exist")
	assert.Equal(t, errcode.NotFound, codeOf(err))
	e.db.MustExec(`UPDATE orders SET fulfillment_mode='manual' WHERE id=?`, e.order.ID)
	_, err = e.s.OrderAccess(buyer, no)
	assert.Equal(t, errcode.NotFound, codeOf(err))
	_, _, _, _, err = e.s.OrderSessions(buyer, no, 1, 20)
	assert.Equal(t, errcode.NotFound, codeOf(err))
	e.db.MustExec(`UPDATE orders SET fulfillment_mode='managed' WHERE id=?`, e.order.ID)

	// 会话: key_name 关联买家登记的公钥(含已删除), 按开始时间倒序, 分页。
	ed := newSigner(t)
	k, err := e.s.AddBuyerKey(buyer, "work", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ed.PublicKey()))), "")
	require.NoError(t, err)
	require.NoError(t, e.s.DeleteBuyerKey(buyer, k.ID, ""))
	e.db.MustExec(`INSERT INTO access_sessions (lease_id, gw_session_id, remote_ip, auth_method, key_fingerprint, started_at, ended_at, end_reason)
		VALUES (?, 'a', '198.51.100.1', 'publickey', ?, NOW() - INTERVAL 2 HOUR, NOW() - INTERVAL 1 HOUR, 'closed'),
		       (?, 'b', '2001:db8::1', 'password', NULL, NOW() - INTERVAL 10 MINUTE, NULL, NULL),
		       (?, 'c', '198.51.100.1', 'publickey', 'SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG', NOW() - INTERVAL 3 HOUR, NULL, NULL)`,
		e.lease.ID, fpOf(ed), e.lease.ID, e.lease.ID)
	list, total, page, size, err := e.s.OrderSessions(buyer, no, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, 1, page)
	assert.Equal(t, 2, size)
	require.Len(t, list, 2)
	assert.Equal(t, "2001:db8::1", list[0].RemoteIP)
	assert.Nil(t, list[0].KeyName)
	assert.Equal(t, e.lease.LeaseNo, list[1].LeaseNo)
	require.NotNil(t, list[1].KeyName)
	assert.Equal(t, "work", *list[1].KeyName)
	assert.Zero(t, list[1].BuyerID, "buyer view does not echo buyer_id")
	list, _, _, _, err = e.s.OrderSessions(buyer, no, 2, 2)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Nil(t, list[0].KeyName, "fingerprint not registered by this buyer")
	_, _, _, _, err = e.s.OrderSessions(99, no, 1, 20)
	assert.Equal(t, errcode.Forbidden, codeOf(err))

	// 运营: 会话按租约号 / IP(规范化) 检索, 带 buyer_id。
	list, total, _, _, err = e.s.AdminSessions(e.lease.LeaseNo, "", 1, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, buyer, list[0].BuyerID)
	list, total, _, _, err = e.s.AdminSessions("", "2001:0db8:0:0::1", 1, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, "b", func() string {
		var id string
		require.NoError(t, e.db.Get(&id, `SELECT gw_session_id FROM access_sessions WHERE id=?`, list[0].ID))
		return id
	}())
	_, total, _, _, err = e.s.AdminSessions(e.lease.LeaseNo, "198.51.100.1", 1, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	list, total, _, _, err = e.s.AdminSessions("l-zzzzzzzz", "", 1, 20)
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.NotNil(t, list)
	_, _, _, _, err = e.s.AdminSessions("L-bad", "", 1, 20)
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	_, _, _, _, err = e.s.AdminSessions("", "1.2.3", 1, 20)
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))

	// 运营: 租约检索。
	leases, total, _, _, err := e.s.AdminLeases(LeaseActive, e.node, buyer, 1, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, e.lease.LeaseNo, leases[0].LeaseNo)
	leases, total, _, _, err = e.s.AdminLeases(LeaseGrace, 0, 0, 1, 20)
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.NotNil(t, leases)
	_, _, _, _, err = e.s.AdminLeases("bogus", 0, 0, 1, 20)
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
}

// 路由与响应信封: 买家路由走 user_id, 运营路由查询参数校验。
func TestAccessHandlers(t *testing.T) {
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", int64(21)) })
	g := r.Group("/api/v1")
	h := NewHandler(e.s)
	h.RegisterBuyerRoutes(g)
	h.RegisterAdminRoutes(g)
	call := func(method, path, body string) (int, json.RawMessage) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var env struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
		return env.Code, env.Data
	}

	ed := newSigner(t)
	body, _ := json.Marshal(map[string]string{"name": "laptop", "public_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ed.PublicKey())))})
	code, data := call("POST", "/api/v1/user/ssh-keys", string(body))
	require.Equal(t, 0, code, string(data))
	var k BuyerKey
	require.NoError(t, json.Unmarshal(data, &k))
	assert.Equal(t, "laptop", k.Name)
	code, _ = call("POST", "/api/v1/user/ssh-keys", `{"name":"x"}`)
	assert.Equal(t, errcode.ParamInvalid, code)
	code, data = call("GET", "/api/v1/user/ssh-keys", "")
	require.Equal(t, 0, code)
	assert.Contains(t, string(data), k.Fingerprint)

	code, data = call("GET", "/api/v1/orders/"+e.order.OrderNo+"/access", "")
	require.Equal(t, 0, code)
	assert.Contains(t, string(data), `"login":"`+e.lease.LeaseNo+`"`)
	code, data = call("GET", "/api/v1/orders/"+e.order.OrderNo+"/sessions?page=1&page_size=10", "")
	require.Equal(t, 0, code)
	assert.JSONEq(t, `{"list":[],"total":0,"page":1,"page_size":10}`, string(data))

	code, _ = call("DELETE", fmt.Sprintf("/api/v1/user/ssh-keys/%d", k.ID), "")
	assert.Equal(t, 0, code)
	code, _ = call("DELETE", fmt.Sprintf("/api/v1/user/ssh-keys/%d", k.ID), "")
	assert.Equal(t, errcode.NotFound, code)
	code, _ = call("DELETE", "/api/v1/user/ssh-keys/abc", "")
	assert.Equal(t, errcode.ParamInvalid, code)

	code, data = call("GET", "/api/v1/admin/leases?state=active", "")
	require.Equal(t, 0, code)
	assert.Contains(t, string(data), e.lease.LeaseNo)
	assert.NotContains(t, string(data), "port_claim")
	code, _ = call("GET", "/api/v1/admin/leases?node_id=x", "")
	assert.Equal(t, errcode.ParamInvalid, code)
	code, _ = call("GET", "/api/v1/admin/access-sessions?ip=bad", "")
	assert.Equal(t, errcode.ParamInvalid, code)
	code, data = call("GET", "/api/v1/admin/access-sessions?lease_no="+e.lease.LeaseNo, "")
	require.Equal(t, 0, code)
	assert.Contains(t, string(data), `"total":0`)
}
