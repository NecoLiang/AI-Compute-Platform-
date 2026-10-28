package accessgw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte(strings.Repeat("s", 32))

func TestSignVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	nonce, err := NewNonce()
	if err != nil || !ValidNonce(nonce) {
		t.Fatalf("nonce %q %v", nonce, err)
	}
	body := []byte(`{"login":"L"}`)
	sig := Sign(testSecret, "POST", PathAuth, now.Unix(), nonce, body)
	ts := strconv.FormatInt(now.Unix(), 10)
	if err := Verify(testSecret, "POST", PathAuth, ts, nonce, sig, body, now.Add(59*time.Second)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	cases := []struct {
		name                        string
		method, uri, ts, nonce, sig string
		body                        []byte
		now                         time.Time
		want                        error
	}{
		{"skew future", "POST", PathAuth, ts, nonce, sig, body, now.Add(-61 * time.Second), ErrBadTimestamp},
		{"skew past", "POST", PathAuth, ts, nonce, sig, body, now.Add(61 * time.Second), ErrBadTimestamp},
		{"no ts", "POST", PathAuth, "", nonce, sig, body, now, ErrBadTimestamp},
		{"bad nonce", "POST", PathAuth, ts, "XYZ", sig, body, now, ErrBadNonce},
		{"upper nonce", "POST", PathAuth, ts, strings.ToUpper(nonce), sig, body, now, ErrBadNonce},
		{"tampered body", "POST", PathAuth, ts, nonce, sig, []byte(`{"login":"M"}`), now, ErrBadSignature},
		{"tampered uri", "POST", PathSessions, ts, nonce, sig, body, now, ErrBadSignature},
		{"tampered method", "GET", PathAuth, ts, nonce, sig, body, now, ErrBadSignature},
		{"other secret", "POST", PathAuth, ts, nonce, Sign([]byte(strings.Repeat("x", 32)), "POST", PathAuth, now.Unix(), nonce, body), body, now, ErrBadSignature},
	}
	for _, c := range cases {
		if err := Verify(testSecret, c.method, c.uri, c.ts, c.nonce, c.sig, c.body, c.now); err != c.want {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
	// 查询串受签名保护。
	q := Sign(testSecret, "GET", PathRevocations+"?cursor=1-2", now.Unix(), nonce, nil)
	if Verify(testSecret, "GET", PathRevocations+"?cursor=1-3", ts, nonce, q, nil, now) != ErrBadSignature {
		t.Error("query string must be covered by the signature")
	}
}

func TestNewClientValidation(t *testing.T) {
	for _, u := range []string{"", "ftp://h:1", "http://h:1/api", "h:1"} {
		if _, err := NewClient(u, testSecret, 0); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if _, err := NewClient("http://h:1", []byte("short"), 0); err == nil {
		t.Error("short secret accepted")
	}
}

// 客户端签名在服务端可验证, 且 query 参与签名; 非 0 业务码/非 200 都是错误。
func TestClientSignsRequests(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotURI = r.URL.RequestURI()
		err := Verify(testSecret, r.Method, r.URL.RequestURI(), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderNonce),
			r.Header.Get(HeaderSignature), body, time.Now())
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 40100, "message": err.Error()})
			return
		}
		switch r.URL.Path {
		case PathRevocations:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": RevocationsResponse{Logins: []string{"a"}, Cursor: "9-9"}})
		case PathProbeResult:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": AuthResponse{Allow: true, CacheTTLS: 3}})
		}
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, testSecret, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if r, err := c.Auth(ctx, AuthRequest{Login: "x", Method: MethodPassword, Password: "p", RemoteIP: "1.2.3.4"}); err != nil || !r.Allow {
		t.Fatalf("auth %v %v", r, err)
	}
	r, err := c.Revocations(ctx, "1 2&x")
	if err != nil || r.Cursor != "9-9" {
		t.Fatalf("revocations %v %v", r, err)
	}
	if gotURI != PathRevocations+"?cursor=1+2%26x" {
		t.Errorf("uri %q", gotURI)
	}
	if err := c.ReportProbe(ctx, ProbeResult{Token: "t"}); err == nil {
		t.Error("HTTP 502 must be an error")
	}
	bad, _ := NewClient(srv.URL, []byte(strings.Repeat("x", 32)), time.Second)
	if _, err := bad.Auth(ctx, AuthRequest{}); err == nil || !strings.Contains(err.Error(), "40100") {
		t.Errorf("wrong secret: %v", err)
	}
}
