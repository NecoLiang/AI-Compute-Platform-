# 居间金融 Intermediary API

**Base**: `http://localhost:8080/api/v1`

**口径（2026-09-07 安全修复后）**
- 留资接口有校验：`type` 白名单 `equipment/construction/finance_lease`（`compute` 只能由商品询价内部产生）、联系人 1-64 字必填、电话格式必填、描述 ≤2000 字；不符返回 40001 + 可展示的中文提示。
- vendor 三个接口全部**按归属执行**：只能看到/操作分配给自己的线索，越权返回 40300。

---

## POST /leads · 创建线索（公开）

```
curl -X POST http://localhost:8080/api/v1/leads \
  -H "Content-Type: application/json" \
  -d '{"type":"equipment","contact_name":"李四","contact_phone":"13900001111","contact_email":"lisi@example.com","description":"需要 20台 H100 服务器","amount_range":"5000000","term":"12个月"}'
```

| 参数 | 类型 | 说明 |
|------|------|------|
| type | string | equipment / construction / finance_lease（白名单外拒绝） |
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

只返回**分配给当前账号**的线索（`assignee_id` 过滤）。

## POST /leads/:id/quote · 报价 ✅ vendor

仅限被分配人，且线索处于 `assigned/following` 状态；否则 40300「线索不存在、未分配给当前账号或状态不允许该操作」。

## POST /leads/:id/close · 成交登记 ✅ vendor

仅限被分配人，线索须处于 `assigned/following/quoted`；`deal_amount` 必须为正（分），`commission_rate` 须在 (0,100]（%），否则 40001。

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
