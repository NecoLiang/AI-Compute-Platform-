# 支付分账 Payment API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 除 callback 外均需 `Bearer <token>`

> ⚠️ **易宝支付未接入**。`POST /payment/pay` 和 `POST /payment/callback` 当前返回错误。沙箱环境需联系易宝商务开通。

---

## POST /payment/pay · 发起支付 ✅ buyer

```
curl -X POST http://localhost:8080/api/v1/payment/pay \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"order_no":"ORD20260713001","channel":"wechat"}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| order_no | string | ✅ | 下单返回的订单号 |
| channel | string | ✅ | wechat / alipay / bank |

**当前返回**（易宝未接入）
```json
{"code":50000,"message":"易宝支付未接入: 需配置 merchant_no + RSA私钥 + API地址"}
```

**接入后预期返回**
```json
{"code":0,"data":{"pay_url":"https://yeepay.com/...","tx_id":"YP..."}}
```
> 前端拿到 HTTPS `pay_url` 后跳转收银台；当前未实现二维码展示。

---

## POST /payment/callback · 支付回调（易宝调用，无需鉴权）

该路由预留给支付渠道异步通知，目前一律拒绝未能验签的回调。业务层已实现验签成功后更新订单与发起分账；真实渠道报文适配及供给方支付通知尚未接入。

---

## GET /payment/status/:order_no · 支付状态 ✅ buyer

```
curl http://localhost:8080/api/v1/payment/status/ORD20260713001 \
  -H "Authorization: Bearer <token>"
```

---

## POST /payment/supplier/onboard · 供给方进件 ✅ supplier

## GET /payment/supplier/onboard/status · 进件状态 ✅ supplier

## GET /payment/settlements · 分账记录 ✅ supplier

```
curl "http://localhost:8080/api/v1/payment/settlements?order_no=ORD20260713001" \
  -H "Authorization: Bearer <token>"
```

---

## 金额说明

所有金额单位为**分(fen)**，`int64` 类型：

| 展示值 | 传输值 |
|--------|:------:|
| ¥35.00 | 3500 |
| ¥201,600.00 | 20160000 |
| ¥10,080.00 (5%佣金) | 1008000 |

> 前端展示：金额 / 100，保留两位小数。禁止在前端用浮点数做金额运算。

---

## 分账规则

```
supplierAmount = totalAmount - platformFee
platformFee = totalAmount × feeRate / 10000  (feeRate默认500=5%)
```


## 支付与业务订单衔接（2026-09-05）

`POST /payment/pay` 从登录会话取得买家身份，并验证角色、认证、订单归属、`pending_payment` 状态与 15 分钟有效期。只接收订单号及渠道，不接受客户端金额；金额读取订单快照。重复请求复用已保存的 `pay_url` 和 `tx_id`，不重复创建付款。支付状态查询限定订单本人，未发起时返回空数组。

回调先通过外部渠道验签，再验证交易号对应的订单号、金额与成功状态。在同一事务内更新 payment 为 `paid`、业务订单为 `paid` 并生成两条分账记录，供给方 ID 来自商品，佣金取自订单快照。重复回调不会重复写分账或把交付中订单退回待交付。渠道接受分账后仅记 `processing`，不冒充资金已到账；失败回调可重试既有分账。

支付超时或订单取消后到达的成功回调不会恢复订单或重新消耗已释放库存，需由支付渠道对账处理。真实渠道及晚到款项对账仍需接入易宝。

**当前真实渠道仍未接入**：默认 `YeepayClient` 拒绝创建支付和回调验签。数据库集成测试仅在外部支付边界注入测试替身，不是沙箱扣款或真实支付证明。需要可用商户、渠道接口、验签资料后才能验收真实支付及到账。

数据库增量：`017_payment_checkout_url`；`pay_url` 仅随发起支付响应返回，不加入通用支付记录 JSON。

本地回归：在独立 MySQL 实例设置 `TEST_MYSQL_DSN`，运行 `go test ./internal/compute -run TestTrade -count=1 -v`，覆盖准入、驳回重提、询价进入 CRM、支付归属与金额、回调到交付签收、幂等及超时返库存。测试自动创建并删除独立数据库，不改业务库。

## 真实渠道接入缺口（2026-09-05 复核）

这不是仅填写环境变量即可启用的能力。`NewYeepayClient()` 当前返回空客户端，`CreatePayment`、`VerifyCallback`、`CreateSplit` 均为拒绝执行的占位实现。

- 接入前需确认已签约的聚合支付、合单、分账产品及沙箱权限，取得对应 API 文档、平台服务商与二级商户标识、应用标识及本地密钥配置位置。
- `CallbackReq` 当前只有交易号、订单号、状态、金额，是业务测试使用的简化结构，不能作为易宝报文格式或签名协议。真实接入须按渠道协议处理原始报文、签名或加密字段，完成验证后再进入业务状态流转，并使用渠道规定的回执。
- 渠道适配必须覆盖真实下单返回、重复请求、异步通知重试、晚到款项对账、分账结果查询；当前 `processing` 只表示提交已受理，不证明到账。
- 验收证据须包含沙箱支付流水、合法与非法回调、订单状态、交付签收和渠道分账结果。现有 MySQL 测试仅验证内部业务链路。
