# 消息 Notification API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 全部需 `Bearer <token>`（买家归属）

**口径**
- 通知由业务事件**同步产生**（暂无异步队列与运营后台）：
  - `order` 订单动态：交付提醒签收、退款完成、订单取消、超时未支付自动取消
  - `ticket` 工单消息：运营回复、受理/完结/关闭
  - `system` 系统通知：发票开具、发票驳回
- 已读 = `read_at` 非空；删除为软删（`deleted_at`），保留审计痕迹。
- 列表响应的 `unread` 是**全部类型**的未读数（不受 `type` 筛选影响），供 Tab 角标。

---

## GET /notifications · 通知列表 ✅

```
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/notifications?type=ticket&page=1&page_size=20"
```

**响应**
```json
{"code":0,"data":{
  "list":[{"id":3,"user_id":2,"type":"ticket","title":"工单有新回复",
    "content":"您的工单 WO-20260826-001「实例无法连接」收到平台运营回复。",
    "link":"/console/buyer/tickets/WO-20260826-001",
    "read_at":null,"created_at":"2026-08-26T10:47:00Z"}],
  "total":1,"unread":3,"page":1,"page_size":20
}}
```

## POST /notifications/:id/read · 标记已读 ✅

CAS 幂等；不存在/非本人/已读过 → 40400。

## POST /notifications/read-all · 全部已读 ✅

**响应**: `{"code":0,"data":{"marked":3}}`

## DELETE /notifications/:id · 删除（软删）✅

不存在/非本人 → 40400。
