# 工单 Ticket API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 全部需 `Bearer <token>`（admin 端点另需 operator/admin 角色）

**口径**
- 买家对本人订单发起工单（REQ-A-044），运营介入处理（REQ-D-035）。
- 状态机：`pending`（待处理）→ `processing`（处理中，运营受理）→ `resolved`（已完结）→ `closed`（已关闭，终态）。
- 沟通记录只追加不修改；买家在 `pending/processing` 可回复，运营需先受理才能回复。
- 工单号 `WO-YYYYMMDD-NNN` 按日递增。

**类型**: `refund_dispute` 退款纠纷 / `resource_fault` 资源故障 / `unavailable` 资源不可用 / `appeal` 申诉 / `other` 其他。

---

## POST /tickets · 创建工单（买家）✅

```
curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"order_no":"ORD20260801123456ab12","type":"resource_fault","title":"实例无法连接","content":"从今早开始 SSH 无法连接实例，重启无效。"}' \
  http://localhost:8080/api/v1/tickets
```

**响应**
```json
{"code":0,"data":{"ticket_no":"WO-20260826-001","status":"pending"}}
```

| 失败场景 | code | 说明 |
|------|------|------|
| 订单不属于买家 | 40300 | 归属校验 |
| 类型/标题/描述不合法 | 40001 | 附中文原因 |

## GET /tickets · 工单列表（买家）✅

```
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/tickets?status=pending&keyword=WO-2026&page=1&page_size=20"
```

## GET /tickets/:ticket_no · 工单详情（买家）✅

**响应**
```json
{"code":0,"data":{
  "ticket":{"id":1,"ticket_no":"WO-20260826-001","buyer_id":2,"order_no":"ORD...",
    "type":"resource_fault","title":"实例无法连接","content":"...","status":"processing",
    "resolved_at":null,"closed_at":null,"created_at":"...","updated_at":"..."},
  "messages":[{"id":1,"ticket_id":1,"sender_type":"buyer","sender_id":2,"content":"...","created_at":"..."}]
}}
```

## POST /tickets/:ticket_no/messages · 买家追加沟通 ✅

仅 `pending/processing` 可回复；`resolved/closed` 返回 40001。

## POST /tickets/:ticket_no/close · 买家关闭工单 ✅

---

## GET /admin/tickets · 工单列表（运营）✅

## GET /admin/tickets/:id · 工单详情（运营）✅

## POST /admin/tickets/:id/claim · 受理（运营）✅

`pending → processing`，CAS 幂等。

## POST /admin/tickets/:id/messages · 运营回复（运营）✅

需先受理（`processing`），否则返回 40001「请先受理工单再回复」。

## POST /admin/tickets/:id/resolve · 完结（运营）✅

`processing → resolved`，写 `resolved_at`。

## POST /admin/tickets/:id/close · 关闭（运营）✅

任意非终态 → `closed`，写 `closed_at`。
