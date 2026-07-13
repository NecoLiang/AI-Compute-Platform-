# 居间金融 Intermediary API

**Base**: `http://localhost:8080/api/v1`

---

## POST /leads · 创建线索（公开）

```
curl -X POST http://localhost:8080/api/v1/leads \
  -H "Content-Type: application/json" \
  -d '{"type":"equipment","contact_name":"李四","contact_phone":"13900001111","contact_email":"lisi@example.com","description":"需要 20台 H100 服务器","amount_range":"5000000","term":"12个月"}'
```

| 参数 | 类型 | 说明 |
|------|------|------|
| type | string | equipment / construction |
| contact_name | string | 联系人 |
| contact_phone | string | 电话 |
| contact_email | string | 邮箱(可选) |
| description | string | 需求描述 |
| amount_range | string | 预算范围(可选) |
| term | string | 期限(可选) |

---

## POST /finance/lease/contact · 融资租赁留资（公开）

同 `/leads`，type 自动设为 `finance_lease`。

---

## GET /vendor/leads · 厂商线索 ✅ vendor

## POST /leads/:id/quote · 报价 ✅ vendor

## POST /leads/:id/close · 成交登记 ✅ vendor

```
curl -X POST http://localhost:8080/api/v1/leads/1/close \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"deal_amount":480000000,"commission_rate":3.0}'
```

| 参数 | 类型 | 说明 |
|------|------|------|
| deal_amount | int | 成交金额(分) |
| commission_rate | float | 佣金率(%)，如 3.0 = 3% |

---

## GET /commissions · 佣金台账 ✅ vendor

---

## 线索状态

| 状态 | 含义 |
|------|------|
| new | 新建待分配 |
| assigned | 已分配 |
| following | 跟进中 |
| quoted | 已报价 |
| closed | 已成交 |
| cancelled | 已关闭 |
