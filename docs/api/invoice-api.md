# 发票 Invoice API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 全部需 `Bearer <token>`（admin 端点另需 operator/admin 角色）

**口径**
- 平台统一开票，金额 = 关联订单实付 `total_amount` 合计（分）。
- 一张发票可合并多个订单；一个订单只能被一张 `pending/issued` 发票占用，被驳回后可重新申请。
- 抬头冗余快照进发票，之后修改开票信息不影响历史发票。
- 当前阶段无税控对接：运营线下开票后通过 admin 端点登记 PDF（占位票可缺省税务号码）。
- 状态机：`pending → issued / rejected`；`red_flushed`（红冲）仅预留。

**状态文案**: `pending` 审核中 / `issued` 已开票 / `rejected` 已驳回。

---

## GET /invoices/title · 读取开票信息 ✅

```
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/invoices/title
```

**响应**（未设置时 `data` 为 `null`）
```json
{"code":0,"data":{
  "id":1,"buyer_id":7,"title_type":"enterprise",
  "company_name":"XX 科技有限公司","tax_no":"91110108MA01C8Y35X",
  "bank_name":"招商银行北京分行","bank_account":"110908877665",
  "address":null,"phone":null,
  "created_at":"2026-08-25T10:00:00Z","updated_at":"2026-08-25T10:00:00Z"
}}
```

## PUT /invoices/title · 保存开票信息 ✅

upsert：每买家一份。四项必填；`tax_no` 校验 15/18/20 位大写字母数字（小写输入归一化为大写）。

```
curl -X PUT -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"company_name":"XX 科技有限公司","tax_no":"91110108MA01C8Y35X","bank_name":"招商银行北京分行","bank_account":"110908877665"}' \
  http://localhost:8080/api/v1/invoices/title
```

**响应**: 保存后的完整 title 对象。校验失败 `code=40001` 并附中文原因。

## GET /invoices/billable-orders · 可开票订单 ✅

已付款（`paid/provisioning/active/completed`）且未被其他发票占用的订单，按下单时间倒序。

```
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/invoices/billable-orders
```

**响应**
```json
{"code":0,"data":{"list":[{
  "order_no":"ORD20260801123456ab12","status":"active","quantity":2,
  "total_amount":15600000,"gpu_model":"NVIDIA H100 SXM 80GB",
  "product_type":"card_rental","pricing_mode":"daily","created_at":"2026-08-01T09:00:00Z"
}],"total":1}}
```

## POST /invoices/apply · 申请开票 ✅

服务端在事务内重新校验订单归属、可开票状态并合计金额，不信前端金额。

```
curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"order_nos":["ORD20260801123456ab12"]}' \
  http://localhost:8080/api/v1/invoices/apply
```

**响应**
```json
{"code":0,"data":{"invoice_no":"INV-2026-0001","amount_fen":15600000,"status":"pending"}}
```

| 失败场景 | code | 说明 |
|------|------|------|
| 未填开票信息 | 40001 | "请先完善开票信息" |
| 订单不可开票 | 40001 | 未支付/已退款/已在其他发票中申请 |
| 超过 50 个订单 | 40001 | 单张发票合并上限 |

## GET /invoices · 发票列表 ✅

```
curl -H "Authorization: Bearer $TOKEN" "http://localhost:8080/api/v1/invoices?page=1&page_size=20"
```

**响应**
```json
{"code":0,"data":{"list":[{
  "id":1,"invoice_no":"INV-2026-0001","buyer_id":7,
  "company_name":"XX 科技有限公司","tax_no":"91110108MA01C8Y35X",
  "bank_name":"招商银行北京分行","bank_account":"110908877665",
  "amount_fen":15600000,"invoice_type":"vat_special","status":"issued",
  "tax_invoice_no":"243170000001","reject_reason":null,
  "applied_at":"2026-08-10T09:00:00Z","issued_at":"2026-08-12T09:00:00Z"
}],"total":1,"page":1,"page_size":20}}
```
> 列表不返回 `pdf_blob`；`invoice_type` 当前固定 `vat_special`（增值税专用发票）。

## GET /invoices/:invoice_no/download · 下载发票 PDF ✅

买家归属校验，仅 `issued` 有文件。成功时直接返回 PDF 二进制（`Content-Type: application/pdf`，`Content-Disposition: attachment`），失败为 JSON 错误。

```
curl -H "Authorization: Bearer $TOKEN" -OJ \
  http://localhost:8080/api/v1/invoices/INV-2026-0001/download
```

---

## GET /admin/invoices · 发票审核列表（运营）✅

```
curl -H "Authorization: Bearer $ADMIN" "http://localhost:8080/api/v1/admin/invoices?status=pending&page=1&page_size=20"
```

## POST /admin/invoices/:id/issue · 完成开票（运营）✅

multipart 上传 PDF（≤5MB，校验 `%PDF-` 魔数）+ 可选 `tax_invoice_no`。CAS：`pending → issued`，重复提交安全。

```
curl -X POST -H "Authorization: Bearer $ADMIN" \
  -F "pdf=@/path/to/invoice.pdf" -F "tax_invoice_no=243170000001" \
  http://localhost:8080/api/v1/admin/invoices/1/issue
```

## POST /admin/invoices/:id/reject · 驳回（运营）✅

```
curl -X POST -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"reason":"开票信息有误，请修改后重新申请"}' \
  http://localhost:8080/api/v1/admin/invoices/1/reject
```
