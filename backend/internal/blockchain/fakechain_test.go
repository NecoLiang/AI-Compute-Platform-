package blockchain

// fakeChain 供测试用的最小 Tendermint JSON-RPC 网关:
// - abci_query(/cosmos.auth.v1beta1.Query/Account): 返回固定 account_number, 自增 sequence
// - broadcast_tx_commit: 原样保存 TxRaw 字节并返回其 sha256 作为交易 hash
// - tx: 按 hash 返回保存的 TxRaw —— 使 VerifyHash 解码的正是客户端自己编码的字节
// fail 置 true 时对广播返回 500, 模拟网关/链故障。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeChain struct {
	srv      *httptest.Server
	fail     atomic.Bool
	mu       sync.Mutex
	txs      map[string][]byte // txHash(hex 大写) -> TxRaw
	sequence uint64
	lastPath string
}

func newFakeChain(t *testing.T) *fakeChain {
	t.Helper()
	f := &fakeChain{txs: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChain) URL() string { return f.srv.URL }

func (f *fakeChain) handle(w http.ResponseWriter, r *http.Request) {
	f.lastPath = r.URL.Path
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	write := func(result any) {
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}
	switch req.Method {
	case "abci_query":
		// QueryAccountResponse{ account = Any{ type_url, BaseAccount{addr=1, acc_num=3, seq=4} } }
		f.mu.Lock()
		seq := f.sequence
		f.sequence++
		f.mu.Unlock()
		base := append(pbString(1, "iaa1fake"), append(pbVarint(3, 7), pbVarint(4, seq)...)...)
		anyMsg := append(pbString(1, "/cosmos.auth.v1beta1.BaseAccount"), pbBytes(2, base)...)
		write(map[string]any{"response": map[string]any{
			"code": 0, "value": base64.StdEncoding.EncodeToString(pbBytes(1, anyMsg)),
		}})
	case "broadcast_tx_commit":
		if f.fail.Load() {
			w.WriteHeader(500)
			w.Write([]byte("gateway down"))
			return
		}
		txB64, _ := req.Params["tx"].(string)
		raw, _ := base64.StdEncoding.DecodeString(txB64)
		sum := sha256.Sum256(raw)
		hash := strings.ToUpper(hex.EncodeToString(sum[:]))
		f.mu.Lock()
		f.txs[hash] = raw
		f.mu.Unlock()
		write(map[string]any{
			"check_tx": map[string]any{"code": 0}, "deliver_tx": map[string]any{"code": 0},
			"hash": hash, "height": "1",
		})
	case "tx":
		hashB64, _ := req.Params["hash"].(string)
		hb, _ := base64.StdEncoding.DecodeString(hashB64)
		hash := strings.ToUpper(hex.EncodeToString(hb))
		f.mu.Lock()
		raw, ok := f.txs[hash]
		f.mu.Unlock()
		if !ok {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32603, "message": "Internal error", "data": "tx (" + hash + ") not found"}})
			return
		}
		write(map[string]any{
			"hash": hash, "tx_result": map[string]any{"code": 0},
			"tx": base64.StdEncoding.EncodeToString(raw),
		})
	default:
		w.WriteHeader(404)
	}
}
