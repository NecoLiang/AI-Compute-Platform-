package blockchain

// BSN 开放联盟链·文昌链 (IRITA v4, Tendermint 0.34) 客户端。
//
// 存证走链原生 irismod.record 模块 (MsgCreateRecord{digest, digest_algo}),
// 无需部署智能合约 —— 这也是 BSN 接入参数表中「合约名称/合约地址」为空的原因。
// 交易在本地用链账户 secp256k1 私钥按 SIGN_MODE_DIRECT 签名, 经 BSN 项目网关
// 的 Tendermint JSON-RPC (/api/{projectId}/rpc) 广播; 验证按 tx hash 查回交易,
// 解码出 MsgCreateRecord 并比对 digest。
//
// 实现说明: 官方 opb-sdk-go/irita-sdk-go 是 2021 年的 cosmos 依赖栈, 与本项目
// Go 1.26 依赖冲突风险大, 且我们只需要「一种消息 + 一个查询」。因此这里手写
// 所需 protobuf wire 编解码(字段号与官方 proto 一一核对, 见各消息注释),
// 只引入纯 Go 的 secp256k1 库。载荷契约不变, 上层 service/worker 无感知。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/ripemd160"
)

// ErrBSNNotConfigured 接入参数不全时的明确错误, 不做任何降级假装上链。
var ErrBSNNotConfigured = fmt.Errorf("BSN 文昌链未接入: 需配置 blockchain.gateway_url + project_id + account_key (BSN 门户「项目管理→下载接入参数」+「链账户」)")

// ErrChainAccountMissing 链账户尚未在链上开户(未绑定/未充能量值)。这是基础设施级
// 状态而非单条存证的失败 —— worker 遇到它应整批跳过, 不烧存证的重试次数。
var ErrChainAccountMissing = fmt.Errorf("链账户在链上不存在")

// BSNConfig 文昌链接入配置。GatewayURL/ProjectID 来自 BSN 门户导出的接入参数表;
// AccountKey 是链账户私钥(64 位 hex, secp256k1), 属敏感信息, 生产从环境变量注入。
type BSNConfig struct {
	GatewayURL  string // 如 https://opbningxia.bsngate.com:18602
	ProjectID   string // BSN 项目 id
	ProjectKey  string // BSN 项目 key; 项目未启用密钥校验时为空
	ChainID     string // 默认 wenchangchain
	AccountKey  string // 链账户 secp256k1 私钥 hex — 敏感, 勿入仓库
	Denom       string // gas 代币面额, 默认 ugas
	GasLimit    uint64 // 单笔交易 gas 上限, 默认 200000
	GasPrice    uint64 // gas 单价(接入参数表 gasprice), 默认 1; 手续费 = GasLimit*GasPrice
	ExplorerURL string // 区块链浏览器 tx 前缀, 供"自行查验"链接
}

const (
	defaultChainID  = "wenchangchain"
	defaultDenom    = "ugas"
	defaultGasLimit = 200000
	defaultGasPrice = 1
	bech32HRP       = "iaa" // IRITA 地址前缀
)

type BSNClient struct {
	cfg     BSNConfig
	http    *http.Client
	priv    *secp256k1.PrivateKey
	address string // 链账户 bech32 地址 iaa1...
}

// NewBSNClient 装配客户端并推导链账户地址。AccountKey 非法时返回错误。
func NewBSNClient(cfg BSNConfig) (*BSNClient, error) {
	if cfg.ChainID == "" {
		cfg.ChainID = defaultChainID
	}
	if cfg.Denom == "" {
		cfg.Denom = defaultDenom
	}
	if cfg.GasLimit == 0 {
		cfg.GasLimit = defaultGasLimit
	}
	if cfg.GasPrice == 0 {
		cfg.GasPrice = defaultGasPrice
	}
	// broadcast_tx_commit 要等交易进块(文昌链出块几秒), 超时给足。
	c := &BSNClient{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
	if cfg.AccountKey != "" {
		raw, err := hex.DecodeString(cfg.AccountKey)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("blockchain.account_key 须为 64 位 hex 的 secp256k1 私钥")
		}
		c.priv = secp256k1.PrivKeyFromBytes(raw)
		c.address = pubKeyToBech32(c.priv.PubKey().SerializeCompressed())
	}
	return c, nil
}

// Configured 网关 + 项目 + 链账户齐备才算接入。未接入时 worker 待命, 存证积累为 pending。
func (b *BSNClient) Configured() bool {
	return b != nil && b.cfg.GatewayURL != "" && b.cfg.ProjectID != "" && b.priv != nil
}

// Address 链账户地址, 供启动日志与 BSN 门户绑定/充能量值用。
func (b *BSNClient) Address() string {
	if b == nil {
		return ""
	}
	return b.address
}

// PubKeyHex 链账户压缩公钥(hex), BSN 门户绑定非托管链账户时使用。公钥非敏感信息。
func (b *BSNClient) PubKeyHex() string {
	if b == nil || b.priv == nil {
		return ""
	}
	return hex.EncodeToString(b.priv.PubKey().SerializeCompressed())
}

// TxURL 拼区块链浏览器链接。未配置浏览器地址时返回空串。
func (b *BSNClient) TxURL(txID string) string {
	if b == nil || b.cfg.ExplorerURL == "" || txID == "" {
		return ""
	}
	return strings.TrimSuffix(b.cfg.ExplorerURL, "/") + "/" + txID
}

// CheckAccount 只读自检: 查询链账户在链上的状态, 供联调工具用。
func (b *BSNClient) CheckAccount(ctx context.Context) (string, error) {
	if !b.Configured() {
		return "", ErrBSNNotConfigured
	}
	accNum, seq, err := b.queryAccount(ctx, b.address)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("account_number=%d sequence=%d", accNum, seq), nil
}

// UploadHash 把 hash 写入 irismod.record 存证模块, 返回交易 hash (REQ-H-022)。
func (b *BSNClient) UploadHash(ctx context.Context, hash string) (string, error) {
	if !b.Configured() {
		return "", ErrBSNNotConfigured
	}
	accNum, seq, err := b.queryAccount(ctx, b.address)
	if err != nil {
		return "", err
	}
	txRaw := b.buildSignedCreateRecord(hash, accNum, seq)

	var res struct {
		CheckTx   struct{ Code int; Log string } `json:"check_tx"`
		DeliverTx struct{ Code int; Log string } `json:"deliver_tx"`
		Hash      string                         `json:"hash"`
		Height    string                         `json:"height"`
	}
	if err := b.rpc(ctx, "broadcast_tx_commit", map[string]any{"tx": base64.StdEncoding.EncodeToString(txRaw)}, &res); err != nil {
		return "", err
	}
	if res.CheckTx.Code != 0 {
		return "", fmt.Errorf("交易未通过 CheckTx (code=%d): %s", res.CheckTx.Code, res.CheckTx.Log)
	}
	if res.DeliverTx.Code != 0 {
		return "", fmt.Errorf("交易上链失败 (code=%d): %s", res.DeliverTx.Code, res.DeliverTx.Log)
	}
	return res.Hash, nil
}

// VerifyHash 按交易 hash 查回链上交易, 解码 MsgCreateRecord 并比对 digest。
// 返回 (链上是否存在且 digest 一致, 交易 hash)。
func (b *BSNClient) VerifyHash(ctx context.Context, txID, hash string) (bool, string, error) {
	if !b.Configured() {
		return false, "", ErrBSNNotConfigured
	}
	txBytes, err := hex.DecodeString(strings.TrimPrefix(txID, "0x"))
	if err != nil {
		return false, "", fmt.Errorf("非法交易 hash: %s", txID)
	}
	var res struct {
		Hash     string `json:"hash"`
		TxResult struct{ Code int; Log string } `json:"tx_result"`
		Tx       string `json:"tx"`
	}
	if err := b.rpc(ctx, "tx", map[string]any{"hash": base64.StdEncoding.EncodeToString(txBytes), "prove": false}, &res); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, txID, nil
		}
		return false, txID, err
	}
	if res.TxResult.Code != 0 {
		return false, txID, nil
	}
	raw, err := base64.StdEncoding.DecodeString(res.Tx)
	if err != nil {
		return false, txID, err
	}
	digest, ok := extractRecordDigest(raw)
	return ok && digest == hash, txID, nil
}

// ===== 账户查询 =====

// queryAccount 经 abci_query 走 gRPC 路由查账户的 account_number 与 sequence。
func (b *BSNClient) queryAccount(ctx context.Context, addr string) (accNum, seq uint64, err error) {
	// cosmos.auth.v1beta1.QueryAccountRequest{ address = 1 }
	reqData := pbString(1, addr)
	var res struct {
		Response struct {
			Code  int    `json:"code"`
			Log   string `json:"log"`
			Value string `json:"value"`
		} `json:"response"`
	}
	err = b.rpc(ctx, "abci_query", map[string]any{
		"path": "/cosmos.auth.v1beta1.Query/Account",
		"data": hex.EncodeToString(reqData),
	}, &res)
	if err != nil {
		return 0, 0, err
	}
	if res.Response.Code != 0 {
		if strings.Contains(res.Response.Log, "not found") {
			return 0, 0, fmt.Errorf("%w: %s 需在 BSN 门户「链账户管理」绑定该地址并分配能量值(gas)后方可发交易", ErrChainAccountMissing, addr)
		}
		return 0, 0, fmt.Errorf("查询链账户失败 (code=%d): %s", res.Response.Code, res.Response.Log)
	}
	value, err := base64.StdEncoding.DecodeString(res.Response.Value)
	if err != nil {
		return 0, 0, err
	}
	// QueryAccountResponse{ account Any = 1 } → Any{ type_url=1, value=2 } → BaseAccount{ address=1, pub_key=2, account_number=3, sequence=4 }
	anyMsg, ok := pbField(value, 1)
	if !ok {
		return 0, 0, fmt.Errorf("账户响应缺少 account 字段")
	}
	baseAcc, ok := pbField(anyMsg, 2)
	if !ok {
		return 0, 0, fmt.Errorf("账户响应缺少 Any.value")
	}
	accNum, _ = pbVarintField(baseAcc, 3)
	seq, _ = pbVarintField(baseAcc, 4)
	return accNum, seq, nil
}

// ===== 交易构造与签名 (SIGN_MODE_DIRECT) =====
//
// 字段号与官方 proto 核对:
//   irismod.record.MsgCreateRecord{ contents=1(repeated Content), creator=2 }
//   irismod.record.Content{ digest=1, digest_algo=2, uri=3, meta=4 }
//   cosmos.tx.v1beta1.TxBody{ messages=1(Any), memo=2 }
//   cosmos.tx.v1beta1.AuthInfo{ signer_infos=1, fee=2 }
//   SignerInfo{ public_key=1(Any), mode_info=2, sequence=3 }; ModeInfo.Single{ mode=1 }; SIGN_MODE_DIRECT=1
//   Fee{ amount=1(Coin), gas_limit=2 }; Coin{ denom=1, amount=2 }
//   SignDoc{ body_bytes=1, auth_info_bytes=2, chain_id=3, account_number=4 }
//   TxRaw{ body_bytes=1, auth_info_bytes=2, signatures=3 }
//   cosmos.crypto.secp256k1.PubKey{ key=1 }

func (b *BSNClient) buildSignedCreateRecord(hash string, accNum, seq uint64) []byte {
	content := append(pbString(1, hash), pbString(2, "SHA256")...)
	msg := append(pbBytes(1, content), pbString(2, b.address)...)
	msgAny := append(pbString(1, "/irismod.record.MsgCreateRecord"), pbBytes(2, msg)...)
	body := pbBytes(1, msgAny)

	pubAny := append(pbString(1, "/cosmos.crypto.secp256k1.PubKey"),
		pbBytes(2, pbBytes(1, b.priv.PubKey().SerializeCompressed()))...)
	modeInfo := pbBytes(1, pbVarint(1, 1)) // Single{mode: SIGN_MODE_DIRECT}
	signerInfo := append(append(pbBytes(1, pubAny), pbBytes(2, modeInfo)...), pbVarint(3, seq)...)
	feeAmount := strconv.FormatUint(b.cfg.GasLimit*b.cfg.GasPrice, 10)
	coin := append(pbString(1, b.cfg.Denom), pbString(2, feeAmount)...)
	fee := append(pbBytes(1, coin), pbVarint(2, b.cfg.GasLimit)...)
	authInfo := append(pbBytes(1, signerInfo), pbBytes(2, fee)...)

	signDoc := append(append(append(pbBytes(1, body), pbBytes(2, authInfo)...),
		pbString(3, b.cfg.ChainID)...), pbVarint(4, accNum)...)
	digest := sha256.Sum256(signDoc)
	// SignCompact 输出 [恢复位|R 32|S 32] 且强制 low-S; Cosmos 签名取 R||S 共 64 字节。
	sig := secpecdsa.SignCompact(b.priv, digest[:], true)[1:]

	return append(append(pbBytes(1, body), pbBytes(2, authInfo)...), pbBytes(3, sig)...)
}

// extractRecordDigest 从 TxRaw 里解出第一条 MsgCreateRecord 的 digest。
func extractRecordDigest(txRaw []byte) (string, bool) {
	body, ok := pbField(txRaw, 1)
	if !ok {
		return "", false
	}
	msgAny, ok := pbField(body, 1)
	if !ok {
		return "", false
	}
	typeURL, _ := pbField(msgAny, 1)
	if string(typeURL) != "/irismod.record.MsgCreateRecord" {
		return "", false
	}
	msg, ok := pbField(msgAny, 2)
	if !ok {
		return "", false
	}
	content, ok := pbField(msg, 1)
	if !ok {
		return "", false
	}
	digest, ok := pbField(content, 1)
	return string(digest), ok
}

// ===== Tendermint JSON-RPC =====

func (b *BSNClient) rpc(ctx context.Context, method string, params map[string]any, out any) error {
	reqBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	u := strings.TrimSuffix(b.cfg.GatewayURL, "/") + "/api/" + b.cfg.ProjectID + "/rpc"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.cfg.ProjectKey != "" {
		// 项目启用 key 校验后必须携带, 头名为 x-api-key (BSN 手册 7.3.1「网关地址规则」)。
		req.Header.Set("x-api-key", b.cfg.ProjectKey)
	}
	res, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("BSN 网关返回 %d: %s", res.StatusCode, truncate(string(raw), 300))
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("解析 RPC 响应失败: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("RPC %s 失败: %s %s", method, envelope.Error.Message, envelope.Error.Data)
	}
	return json.Unmarshal(envelope.Result, out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ===== protobuf wire 编解码(仅本文件所需的最小子集) =====

func varint(v uint64) []byte {
	buf := make([]byte, binary.MaxVarintLen64)
	return buf[:binary.PutUvarint(buf, v)]
}

// pbBytes 编码 length-delimited 字段 (wire type 2): 嵌套消息 / bytes。
func pbBytes(field int, b []byte) []byte {
	out := varint(uint64(field)<<3 | 2)
	out = append(out, varint(uint64(len(b)))...)
	return append(out, b...)
}

func pbString(field int, s string) []byte { return pbBytes(field, []byte(s)) }

// pbVarint 编码 varint 字段 (wire type 0)。
func pbVarint(field int, v uint64) []byte {
	return append(varint(uint64(field)<<3|0), varint(v)...)
}

// pbField 取消息中第一个指定编号的 length-delimited 字段。
func pbField(msg []byte, field int) ([]byte, bool) {
	for i := 0; i < len(msg); {
		tag, n := binary.Uvarint(msg[i:])
		if n <= 0 {
			return nil, false
		}
		i += n
		f, wt := int(tag>>3), int(tag&7)
		switch wt {
		case 0:
			_, n := binary.Uvarint(msg[i:])
			if n <= 0 {
				return nil, false
			}
			i += n
		case 2:
			l, n := binary.Uvarint(msg[i:])
			if n <= 0 || i+n+int(l) > len(msg) {
				return nil, false
			}
			i += n
			if f == field {
				return msg[i : i+int(l)], true
			}
			i += int(l)
		case 5:
			i += 4
		case 1:
			i += 8
		default:
			return nil, false
		}
	}
	return nil, false
}

// pbVarintField 取消息中第一个指定编号的 varint 字段。
func pbVarintField(msg []byte, field int) (uint64, bool) {
	for i := 0; i < len(msg); {
		tag, n := binary.Uvarint(msg[i:])
		if n <= 0 {
			return 0, false
		}
		i += n
		f, wt := int(tag>>3), int(tag&7)
		switch wt {
		case 0:
			v, n := binary.Uvarint(msg[i:])
			if n <= 0 {
				return 0, false
			}
			i += n
			if f == field {
				return v, true
			}
		case 2:
			l, n := binary.Uvarint(msg[i:])
			if n <= 0 || i+n+int(l) > len(msg) {
				return 0, false
			}
			i += n + int(l)
		case 5:
			i += 4
		case 1:
			i += 8
		default:
			return 0, false
		}
	}
	return 0, false
}

// ===== 地址推导: bech32(iaa, ripemd160(sha256(压缩公钥))) =====

func pubKeyToBech32(compressedPub []byte) string {
	sh := sha256.Sum256(compressedPub)
	r := ripemd160.New()
	r.Write(sh[:])
	return bech32Encode(bech32HRP, r.Sum(nil))
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for _, c := range hrp {
		out = append(out, byte(c)>>5)
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, byte(c)&31)
	}
	return out
}

// convertTo5Bit 8bit → 5bit 分组(带填充)。
func convertTo5Bit(data []byte) []byte {
	var out []byte
	acc, bits := 0, 0
	for _, b := range data {
		acc = acc<<8 | int(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, byte(acc>>bits)&31)
		}
	}
	if bits > 0 {
		out = append(out, byte(acc<<(5-bits))&31)
	}
	return out
}

func bech32Encode(hrp string, data []byte) string {
	d5 := convertTo5Bit(data)
	values := append(bech32HRPExpand(hrp), d5...)
	polymod := bech32Polymod(append(values, 0, 0, 0, 0, 0, 0)) ^ 1
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, v := range d5 {
		sb.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		sb.WriteByte(bech32Charset[(polymod>>uint(5*(5-i)))&31])
	}
	return sb.String()
}
