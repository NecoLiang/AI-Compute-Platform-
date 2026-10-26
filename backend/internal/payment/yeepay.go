package payment

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ===== 易宝支付(平台商收付款方案)客户端 =====
//
// 资金路径(不碰资金二清): 买家经易宝标准收银台付款 → 资金进入收单商户(平台商编)的
// 分账在途账户(fundProcessType=DELAY_SETTLE) → 清算完成后平台调用「申请订单分账」把
// 供给方应得部分分到供给方的易宝商编, 剩余(平台服务费)解冻给收单商户。退款同样原路
// 经易宝处理, 已分账订单通过 divideBackDetail 从供给方商编归集回退。

// YeepayConfig 由 main 从 config.yaml / 环境变量(YEEPAY_*)装配。
type YeepayConfig struct {
	AppKey            string // 开放平台应用 appKey, 如 app_1008xxxxxxx
	ParentMerchantNo  string // 发起方(平台商)商编
	MerchantNo        string // 收款商编; 平台商自收时与 ParentMerchantNo 相同
	PrivateKey        string // 商户 RSA2048 私钥(PKCS#8 base64) — 敏感, 只从环境变量注入
	PlatformPublicKey string // 易宝平台公钥, 留空用官方默认值
	ServerRoot        string // 留空用 https://openapi.yeepay.com/yop-center
	NotifyBaseURL     string // 回调地址前缀, 如 https://www.omnisline.com/api/v1
	ReturnBaseURL     string // 支付完成后跳回的前端站点, 如 https://www.omnisline.com
	PayChannels       string // 向买家开放的支付方式, 逗号分隔; 留空为全部(须与易宝已开通产品一致)
}

type YeepayClient struct {
	cfg      YeepayConfig
	channels []string
	yop      *yopClient // nil 表示未接入
}

// NewYeepayClient 全部为空时返回未接入的客户端(所有能力显式报错); 半配置直接报错。
func NewYeepayClient(cfg YeepayConfig) (*YeepayClient, error) {
	required := []string{cfg.AppKey, cfg.ParentMerchantNo, cfg.MerchantNo, cfg.PrivateKey, cfg.NotifyBaseURL, cfg.ReturnBaseURL}
	set := 0
	for _, v := range required {
		if strings.TrimSpace(v) != "" {
			set++
		}
	}
	if set == 0 {
		return &YeepayClient{}, nil
	}
	if set != len(required) {
		return nil, fmt.Errorf("yeepay.app_key/parent_merchant_no/merchant_no/private_key/notify_base_url/return_base_url 必须同时配置")
	}
	for _, raw := range []string{cfg.NotifyBaseURL, cfg.ReturnBaseURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" {
			return nil, fmt.Errorf("yeepay 回调/跳转地址必须是 https 绝对地址: %q", raw)
		}
	}
	channels, err := parsePayChannels(cfg.PayChannels)
	if err != nil {
		return nil, err
	}
	cfg.NotifyBaseURL = strings.TrimRight(cfg.NotifyBaseURL, "/")
	cfg.ReturnBaseURL = strings.TrimRight(cfg.ReturnBaseURL, "/")
	client, err := newYOPClient(cfg.AppKey, cfg.ServerRoot, cfg.PrivateKey, cfg.PlatformPublicKey)
	if err != nil {
		return nil, err
	}
	return &YeepayClient{cfg: cfg, channels: channels, yop: client}, nil
}

func (y *YeepayClient) Configured() bool { return y != nil && y.yop != nil }

// payChannelOrder 买家可选支付方式的展示顺序。
var payChannelOrder = []string{"wechat", "alipay", "bank"}

// parsePayChannels 未开通的产品下单会被易宝拒绝(00012), 所以只开放配置里列出的方式。
func parsePayChannels(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return payChannelOrder, nil
	}
	want := map[string]bool{}
	for _, ch := range strings.Split(raw, ",") {
		ch = strings.ToLower(strings.TrimSpace(ch))
		if limitPayTypes[ch] == "" {
			return nil, fmt.Errorf("yeepay.pay_channels 含未知支付方式 %q(可选 wechat/alipay/bank)", ch)
		}
		want[ch] = true
	}
	out := make([]string, 0, len(want))
	for _, ch := range payChannelOrder {
		if want[ch] {
			out = append(out, ch)
		}
	}
	return out, nil
}

// PayChannels 向买家开放的支付方式; 未接入时为空。
func (y *YeepayClient) PayChannels() []string {
	if !y.Configured() {
		return []string{}
	}
	return y.channels
}

// NotifyURL 各类异步通知的接收地址(公开路由, 见 Handler.RegisterCallbackRoutes)。
func (y *YeepayClient) NotifyURL(kind string) string {
	return y.cfg.NotifyBaseURL + "/payment/yeepay/notify/" + kind
}

// ===== 领域类型: 金额一律为分(int64), 与易宝交互时才换算成元 =====

type PaymentOrder struct {
	OrderNo   string
	Amount    int64
	Channel   string // wechat/alipay/bank
	GoodsName string
	ExpiresAt time.Time
}

type Checkout struct {
	PayURL        string
	UniqueOrderNo string
}

// PaymentResult 来自支付结果通知或订单查询; Status 为易宝原值
// PROCESSING/SUCCESS/TIME_OUT/FAIL/CLOSE。
type PaymentResult struct {
	OrderNo       string
	UniqueOrderNo string
	Status        string
	Amount        int64
	PaidAt        *time.Time
	Cleared       bool
	ClearedAt     *time.Time
}

type DivideItem struct {
	LedgerNo string // 分账接收方商编
	Amount   int64
	Desc     string
}

type DivideOrder struct {
	OrderNo   string
	RequestID string
	Items     []DivideItem
}

type DivideDetail struct {
	DetailNo string
	LedgerNo string
	Amount   int64
}

// DivideResult Status 为 PROCESSING/SUCCESS/FAIL。
type DivideResult struct {
	Status  string
	Details []DivideDetail
}

type DivideBackItem struct {
	LedgerNo       string
	DivideDetailNo string
	Amount         int64
}

type RefundOrder struct {
	OrderNo    string
	RequestID  string
	Amount     int64
	Reason     string
	DivideBack []DivideBackItem
}

// RefundResult Status 为 PROCESSING/SUCCESS/FAILED/CANCEL/SUSPEND。
type RefundResult struct {
	OrderNo        string
	RequestID      string
	UniqueRefundNo string
	Status         string
	Amount         int64
	FailReason     string
}

type NetInResult struct {
	RequestNo         string
	ApplicationStatus string // WAIT_SIGN/REVIEWING/REVIEW_BACK/AUTHENTICITY_VERIFYING/AGREEMENT_SIGNING/BUSINESS_OPENING/COMPLETED
	MerchantNo        string
	AuditOpinion      string
	NetInURL          string
}

// YeepayBizError 易宝受理了请求但业务失败(result.code 非成功码)。
type YeepayBizError struct {
	Op      string
	Code    string
	Message string
}

func (e *YeepayBizError) Error() string {
	return fmt.Sprintf("易宝%s失败: %s %s", e.Op, e.Code, e.Message)
}

func bizCode(err error) string {
	var biz *YeepayBizError
	if errors.As(err, &biz) {
		return biz.Code
	}
	return ""
}

// ===== 接口 =====

// limitPayTypes 买家选择的支付方式 → 收银台展示的支付产品。
var limitPayTypes = map[string]string{
	"wechat": "WECHAT",
	"alipay": "ALIPAY",
	"bank":   "EBANK,YJZF",
}

func (y *YeepayClient) CreatePayment(o PaymentOrder) (*Checkout, error) {
	if !y.Configured() {
		return nil, ErrYeepayNotConfigured
	}
	params := map[string]string{
		"parentMerchantNo": y.cfg.ParentMerchantNo,
		"merchantNo":       y.cfg.MerchantNo,
		"orderId":          o.OrderNo,
		"orderAmount":      formatYuan(o.Amount),
		"goodsName":        truncateRunes(o.GoodsName, 80),
		"fundProcessType":  "DELAY_SETTLE",
		"notifyUrl":        y.NotifyURL("pay"),
		"csNotifyUrl":      y.NotifyURL("clear"),
		"returnUrl":        y.cfg.ReturnBaseURL + "/console/buyer/orders/" + o.OrderNo,
		"memo":             o.OrderNo,
	}
	if !o.ExpiresAt.IsZero() {
		params["expiredTime"] = o.ExpiresAt.In(shanghai).Format("2006-01-02 15:04:05")
	}
	if pt := limitPayTypes[o.Channel]; pt != "" {
		params["limitPayType"] = pt
	}
	var out struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		UniqueOrderNo string `json:"uniqueOrderNo"`
		CashierURL    string `json:"cashierUrl"`
	}
	if err := y.yop.postForm("/rest/v1.0/cashier/unified/order", params, &out); err != nil {
		return nil, err
	}
	if out.Code != "00000" {
		return nil, &YeepayBizError{Op: "下单", Code: out.Code, Message: out.Message}
	}
	return &Checkout{PayURL: out.CashierURL, UniqueOrderNo: out.UniqueOrderNo}, nil
}

func (y *YeepayClient) QueryPayment(orderNo string) (*PaymentResult, error) {
	if !y.Configured() {
		return nil, ErrYeepayNotConfigured
	}
	var out struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		OrderID       string `json:"orderId"`
		UniqueOrderNo string `json:"uniqueOrderNo"`
		Status        string `json:"status"`
		OrderAmount   yuan   `json:"orderAmount"`
		PaySuccess    string `json:"paySuccessDate"`
		ClearStatus   string `json:"clearStatus"`
		CsSuccessDate string `json:"csSuccessDate"`
	}
	err := y.yop.get("/rest/v1.0/trade/order/query", map[string]string{
		"parentMerchantNo": y.cfg.ParentMerchantNo, "merchantNo": y.cfg.MerchantNo, "orderId": orderNo,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.Code != "OPR00000" {
		return nil, &YeepayBizError{Op: "订单查询", Code: out.Code, Message: out.Message}
	}
	return &PaymentResult{
		OrderNo: out.OrderID, UniqueOrderNo: out.UniqueOrderNo, Status: out.Status, Amount: int64(out.OrderAmount),
		PaidAt: parseYeepayTime(out.PaySuccess), Cleared: out.ClearStatus == "SUCCESS", ClearedAt: parseYeepayTime(out.CsSuccessDate),
	}, nil
}

func (y *YeepayClient) Divide(o DivideOrder) (*DivideResult, error) {
	if !y.Configured() {
		return nil, ErrYeepayNotConfigured
	}
	type item struct {
		LedgerNo         string      `json:"ledgerNo"`
		Amount           json.Number `json:"amount"`
		DivideDetailDesc string      `json:"divideDetailDesc,omitempty"`
		LedgerType       string      `json:"ledgerType"`
	}
	items := make([]item, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, item{LedgerNo: it.LedgerNo, Amount: json.Number(formatYuan(it.Amount)), DivideDetailDesc: truncateRunes(it.Desc, 40), LedgerType: "MERCHANT2MERCHANT"})
	}
	detail, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	var out divideResponse
	err = y.yop.postForm("/rest/v1.0/divide/apply", map[string]string{
		"parentMerchantNo": y.cfg.ParentMerchantNo, "merchantNo": y.cfg.MerchantNo, "orderId": o.OrderNo,
		"divideRequestId": o.RequestID, "divideDetail": string(detail),
		// 平台服务费即分账剩余部分, 随本次分账解冻给收单商户(平台商编)。
		"isUnfreezeResidualAmount": "TRUE",
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.result("分账")
}

func (y *YeepayClient) QueryDivide(orderNo, requestID string) (*DivideResult, error) {
	if !y.Configured() {
		return nil, ErrYeepayNotConfigured
	}
	var out divideResponse
	err := y.yop.get("/rest/v1.0/divide/query", map[string]string{
		"parentMerchantNo": y.cfg.ParentMerchantNo, "merchantNo": y.cfg.MerchantNo, "orderId": orderNo, "divideRequestId": requestID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.result("分账查询")
}

type divideResponse struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	Status       string `json:"status"`
	DivideDetail string `json:"divideDetail"`
}

func (r divideResponse) result(op string) (*DivideResult, error) {
	if r.Code != "OPR00000" {
		return nil, &YeepayBizError{Op: op, Code: r.Code, Message: r.Message}
	}
	result := &DivideResult{Status: r.Status}
	if strings.TrimSpace(r.DivideDetail) != "" {
		var details []struct {
			DivideDetailNo string `json:"divideDetailNo"`
			LedgerNo       string `json:"ledgerNo"`
			Amount         yuan   `json:"amount"`
		}
		if err := json.Unmarshal([]byte(r.DivideDetail), &details); err != nil {
			return nil, fmt.Errorf("易宝分账明细解析失败: %w", err)
		}
		for _, d := range details {
			result.Details = append(result.Details, DivideDetail{DetailNo: d.DivideDetailNo, LedgerNo: d.LedgerNo, Amount: int64(d.Amount)})
		}
	}
	return result, nil
}

func (y *YeepayClient) Refund(o RefundOrder) (*RefundResult, error) {
	if !y.Configured() {
		return nil, ErrYeepayNotConfigured
	}
	params := map[string]string{
		"parentMerchantNo": y.cfg.ParentMerchantNo, "merchantNo": y.cfg.MerchantNo, "orderId": o.OrderNo,
		"refundRequestId": o.RequestID, "refundAmount": formatYuan(o.Amount),
		"description": truncateRunes(o.Reason, 80), "notifyUrl": y.NotifyURL("refund"),
	}
	if len(o.DivideBack) > 0 {
		type back struct {
			LedgerNo       string      `json:"ledgerNo"`
			Amount         json.Number `json:"amount"`
			DivideDetailNo string      `json:"divideDetailNo"`
		}
		backs := make([]back, 0, len(o.DivideBack))
		for _, b := range o.DivideBack {
			backs = append(backs, back{LedgerNo: b.LedgerNo, Amount: json.Number(formatYuan(b.Amount)), DivideDetailNo: b.DivideDetailNo})
		}
		raw, err := json.Marshal(backs)
		if err != nil {
			return nil, err
		}
		params["divideBackDetail"] = string(raw)
	}
	var out refundResponse
	if err := y.yop.postForm("/rest/v1.0/trade/refund", params, &out); err != nil {
		return nil, err
	}
	return out.result("退款")
}

func (y *YeepayClient) QueryRefund(orderNo, requestID string) (*RefundResult, error) {
	if !y.Configured() {
		return nil, ErrYeepayNotConfigured
	}
	var out refundResponse
	err := y.yop.get("/rest/v1.0/trade/refund/query", map[string]string{
		"parentMerchantNo": y.cfg.ParentMerchantNo, "merchantNo": y.cfg.MerchantNo, "orderId": orderNo, "refundRequestId": requestID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.result("退款查询")
}

type refundResponse struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	OrderID        string `json:"orderId"`
	RefundRequest  string `json:"refundRequestId"`
	UniqueRefundNo string `json:"uniqueRefundNo"`
	Status         string `json:"status"`
	RefundAmount   yuan   `json:"refundAmount"`
	FailReason     string `json:"failReason"`
}

func (r refundResponse) result(op string) (*RefundResult, error) {
	if r.Code != "OPR00000" {
		return nil, &YeepayBizError{Op: op, Code: r.Code, Message: r.Message}
	}
	return &RefundResult{OrderNo: r.OrderID, RequestID: r.RefundRequest, UniqueRefundNo: r.UniqueRefundNo, Status: r.Status, Amount: int64(r.RefundAmount), FailReason: r.FailReason}, nil
}

// NetInURL 获取供给方 H5 入网(开通易宝商编)链接; requestNo 幂等。
func (y *YeepayClient) NetInURL(requestNo string) (string, error) {
	if !y.Configured() {
		return "", ErrYeepayNotConfigured
	}
	var out struct {
		Code     string `json:"code"`
		Msg      string `json:"msg"`
		NetInURL string `json:"netInUrl"`
	}
	err := y.yop.postJSON("/rest/v1.0/mer/netin/h5/index", map[string]string{
		"requestNo": requestNo, "notifyUrl": y.NotifyURL("netin"), "parentMerchantNo": y.cfg.ParentMerchantNo,
	}, &out)
	if err != nil {
		return "", err
	}
	if out.Code != "MAS000000" {
		return "", &YeepayBizError{Op: "获取入网链接", Code: out.Code, Message: out.Msg}
	}
	return out.NetInURL, nil
}

func (y *YeepayClient) QueryNetIn(requestNo string) (*NetInResult, error) {
	if !y.Configured() {
		return nil, ErrYeepayNotConfigured
	}
	var out netInPayload
	if err := y.yop.postJSON("/rest/v1.0/mer/netin/h5/query", map[string]string{"requestNo": requestNo}, &out); err != nil {
		return nil, err
	}
	if out.Code != "MAS000000" {
		return nil, &YeepayBizError{Op: "入网进度查询", Code: out.Code, Message: out.Msg}
	}
	return out.result(), nil
}

type netInPayload struct {
	Code              string `json:"code"`
	Msg               string `json:"msg"`
	RequestNo         string `json:"requestNo"`
	ApplicationStatus string `json:"applicationStatus"`
	MerchantNo        string `json:"merchantNo"`
	AuditOpinion      string `json:"auditOpinion"`
	NetInURL          string `json:"netInUrl"`
}

func (p netInPayload) result() *NetInResult {
	return &NetInResult{RequestNo: p.RequestNo, ApplicationStatus: p.ApplicationStatus, MerchantNo: p.MerchantNo, AuditOpinion: p.AuditOpinion, NetInURL: p.NetInURL}
}

// DecryptNotify 解密并验签异步通知; 未接入时一律拒绝。
func (y *YeepayClient) DecryptNotify(raw string) ([]byte, error) {
	if !y.Configured() {
		return nil, ErrYeepayCallbackUnverifiable
	}
	return y.yop.decryptNotify(raw)
}

// ===== 通知报文 =====

func parsePayNotify(content []byte) (*PaymentResult, error) {
	var n struct {
		OrderID        string `json:"orderId"`
		UniqueOrderNo  string `json:"uniqueOrderNo"`
		Status         string `json:"status"`
		OrderAmount    yuan   `json:"orderAmount"`
		PaySuccessDate string `json:"paySuccessDate"`
	}
	if err := json.Unmarshal(content, &n); err != nil {
		return nil, fmt.Errorf("支付通知解析失败: %w", err)
	}
	return &PaymentResult{OrderNo: n.OrderID, UniqueOrderNo: n.UniqueOrderNo, Status: n.Status, Amount: int64(n.OrderAmount), PaidAt: parseYeepayTime(n.PaySuccessDate)}, nil
}

func parseClearNotify(content []byte) (*PaymentResult, error) {
	var n struct {
		OrderID       string `json:"orderId"`
		UniqueOrderNo string `json:"uniqueOrderNo"`
		Status        string `json:"status"`
		OrderAmount   yuan   `json:"orderAmount"`
		CsSuccessDate string `json:"csSuccessDate"`
	}
	if err := json.Unmarshal(content, &n); err != nil {
		return nil, fmt.Errorf("清算通知解析失败: %w", err)
	}
	return &PaymentResult{OrderNo: n.OrderID, UniqueOrderNo: n.UniqueOrderNo, Status: "SUCCESS", Amount: int64(n.OrderAmount), Cleared: n.Status == "SUCCESS", ClearedAt: parseYeepayTime(n.CsSuccessDate)}, nil
}

func parseRefundNotify(content []byte) (*RefundResult, error) {
	var n struct {
		OrderID         string `json:"orderId"`
		RefundRequestID string `json:"refundRequestId"`
		UniqueRefundNo  string `json:"uniqueRefundNo"`
		Status          string `json:"status"`
		RefundAmount    yuan   `json:"refundAmount"`
		ErrorMessage    string `json:"errorMessage"`
	}
	if err := json.Unmarshal(content, &n); err != nil {
		return nil, fmt.Errorf("退款通知解析失败: %w", err)
	}
	return &RefundResult{OrderNo: n.OrderID, RequestID: n.RefundRequestID, UniqueRefundNo: n.UniqueRefundNo, Status: n.Status, Amount: int64(n.RefundAmount), FailReason: n.ErrorMessage}, nil
}

func parseNetInNotify(content []byte) (*NetInResult, error) {
	var n netInPayload
	if err := json.Unmarshal(content, &n); err != nil {
		return nil, fmt.Errorf("入网通知解析失败: %w", err)
	}
	return n.result(), nil
}

// ===== 金额与时间 =====

// yuan 以分存储的易宝金额; 兼容 JSON 数字与字符串("138.00" / 138.5)。
type yuan int64

func (y *yuan) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*y = 0
		return nil
	}
	fen, err := parseYuan(s)
	if err != nil {
		return err
	}
	*y = yuan(fen)
	return nil
}

// parseYuan 把元金额精确换算为分, 不经过浮点。
func parseYuan(s string) (int64, error) {
	intPart, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if intPart == "" || len(frac) > 2 && strings.TrimRight(frac[2:], "0") != "" {
		return 0, fmt.Errorf("金额格式无效: %q", s)
	}
	if len(frac) > 2 {
		frac = frac[:2]
	}
	frac += strings.Repeat("0", 2-len(frac))
	yuanPart, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil || yuanPart < 0 {
		return 0, fmt.Errorf("金额格式无效: %q", s)
	}
	fen, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("金额格式无效: %q", s)
	}
	return yuanPart*100 + fen, nil
}

func formatYuan(fen int64) string {
	return fmt.Sprintf("%d.%02d", fen/100, fen%100)
}

var shanghai = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}()

func parseYeepayTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04:05.0", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, shanghai); err == nil {
			return &t
		}
	}
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n])
	}
	return string(r)
}

var ErrYeepayNotConfigured = fmt.Errorf("易宝支付未接入: 需配置 yeepay.app_key/商编/私钥/回调地址")

// ErrYeepayCallbackUnverifiable 支付回调无法验签时返回。回调接口无鉴权,
// 放行未验签的回调等于允许任何人伪造支付成功并触发分账, 因此一律拒绝。
var ErrYeepayCallbackUnverifiable = fmt.Errorf("支付回调验签失败或易宝未接入, 已拒绝")
