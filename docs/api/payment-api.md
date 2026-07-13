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
> 前端拿到 `pay_url` 后跳转易宝收银台 / 展示二维码

---

## POST /payment/callback · 支付回调（易宝调用，无需鉴权）

易宝支付完成后异步回调本接口。平台验签后更新订单状态→触发分账→通知供给方。

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
