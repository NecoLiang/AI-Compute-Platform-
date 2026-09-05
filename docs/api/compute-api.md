# 算力撮合 Compute API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 标注 ✅ 的需 `Bearer <token>`

---

## GET /products · 商品列表（公开）

```
curl "http://localhost:8080/api/v1/products?gpu_model=H100&region=北京&pricing_mode=hourly&sort=price_asc&page=1&page_size=20"
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| gpu_model | string | — | NVIDIA H100 / 华为昇腾910B 等 |
| region | string | — | 北京/上海/深圳等 |
| pricing_mode | string | — | hourly / weekly / monthly |
| price_min | int | — | 最低单价(分) |
| price_max | int | — | 最高单价(分) |
| sort | string | — | price_asc / price_desc / created_at_desc |
| page | int | — | 默认 1 |
| page_size | int | — | 默认 20 |

**响应**
```json
{"code":0,"data":{
  "list":[{
    "id":1,"supplier_id":2,"gpu_model":"NVIDIA H100 SXM 80GB","card_count":8,
    "cpu_spec":"2× Intel Xeon 8480+","memory_spec":"2TB DDR5","storage_spec":"30TB NVMe",
    "bandwidth_spec":"10Gbps","delivery_mode":"bare_metal","pricing_mode":"hourly",
    "unit_price":3500,"available_hours":"全天 24h","stock":32,"min_order":1,"min_duration":1,
    "region":"北京","status":"active","self_operated":false
  }],
  "total":47,"page":1,"page_size":20
}}
```
> `unit_price = 3500` 表示 ¥35.00 / 卡·时。前端展示时除以 100。

---

## GET /products/:id · 商品详情（公开）

```
curl http://localhost:8080/api/v1/products/1
```

**响应**
```json
{"code":0,"data":{
  "product":{...},
  "credit":{"supplier_id":2,"fulfill_rate":99.2,"sla_rate":99.8,"violation_count":0}
}}
```

---

## POST /supplier/products · 提交商品审核 ✅ supplier

```
curl -X POST http://localhost:8080/api/v1/supplier/products \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"gpu_model":"NVIDIA H100 SXM 80GB","card_count":64,"cpu_spec":"2× Intel Xeon 8480+","memory_spec":"2TB DDR5","storage_spec":"30TB NVMe","bandwidth_spec":"10Gbps","delivery_mode":"bare_metal","pricing_mode":"hourly","unit_price":3500,"available_hours":"全天 24h","stock":64,"min_order":1,"min_duration":1,"region":"北京","compliance_agreed":true}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| gpu_model | string | ✅ | GPU 型号 |
| card_count | int | ✅ | 可售总卡数 |
| delivery_mode | string | ✅ | bare_metal / container / vm / rack |
| pricing_mode | string | ✅ | hourly / weekly / monthly |
| unit_price | int | ✅ | 单价(分)/卡·时，如 3500=¥35.00 |
| stock | int | ✅ | 可售余量 |
| region | string | ✅ | 地域 |
| compliance_agreed | bool | ✅ | 合规承诺（必须为 true） |

---

## POST /orders · 下单 ✅ buyer

```
curl -X POST http://localhost:8080/api/v1/orders \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"product_id":1,"quantity":8,"duration":720,"compliance_agreed":true}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| product_id | int | ✅ | 商品 ID |
| quantity | int | ✅ | 卡数 |
| duration | int | ✅ | 租期(小时) |
| compliance_agreed | bool | ✅ | 合规承诺 |

**成功**
```json
{"code":0,"data":{
  "order_no":"ORD20260713143000123abc","total_amount":20160000,
  "platform_fee":1008000,"status":"pending_payment",
  "payment_expires_at":"2026-07-13T14:45:00Z"
}}
```
**失败**
```json
{"code":40900,"message":"insufficient stock"}
```

---

## GET /orders · 我的订单 ✅ buyer

```
curl "http://localhost:8080/api/v1/orders?status=active&order_no=20260711&page=1&page_size=20" \
  -H "Authorization: Bearer <token>"
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| status | string | 否 | 订单状态精确筛选 |
| order_no | string | 否 | 订单号包含搜索；可带开头的 `#` |
| page | int | 否 | 默认 1，最大 1000000 |
| page_size | int | 否 | 默认 20，最大 100 |

列表保留完整订单字段，并追加商品/供给方摘要，金额单位仍为分：

```json
{"code":0,"message":"success","data":{
  "list":[{
    "id":1,"order_no":"ORD20260711001","buyer_id":42,"product_id":9,
    "quantity":8,"duration":1,"unit_price":2520000,"total_amount":20160000,
    "platform_fee":1008000,"status":"active",
    "payment_expires_at":null,"lease_start_at":"2026-07-11T02:00:00Z",
    "lease_end_at":"2026-08-11T02:00:00Z","compliance_agreed":true,
    "created_at":"2026-07-11T01:30:00Z","updated_at":"2026-07-11T02:00:00Z",
    "gpu_model":"H100","product_type":"card_rental","pricing_mode":"monthly","self_operated":false,
    "supplier_name":"中联数据"
  }],
  "total":1,"page":1,"page_size":20
},"request_id":"req_xxx"}
```

`supplier_name`：自营商品固定返回“平台自营”；非自营仅返回已认证企业名称，没有可用资料时为空串。

---

## GET /orders/:order_no · 买家订单详情 ✅

```
curl http://localhost:8080/api/v1/orders/ORD20260713001 \
  -H "Authorization: Bearer <token>"
```

仅查询当前 JWT 用户自己的订单；订单不存在或不属于当前用户均返回 `code=40400`。路径只接受 `ORD` / `REN` 开头、总长不超过 32 的订单号，不接受数据库自增 ID。

```json
{"code":0,"message":"success","data":{
  "order":{"order_no":"ORD20260713001","status":"active","quantity":8,"duration":1,
    "unit_price":2520000,"total_amount":20160000,"platform_fee":1008000,
    "payment_expires_at":null,"lease_start_at":"2026-07-11T02:00:00Z",
    "lease_end_at":"2026-08-11T02:00:00Z","compliance_agreed":true,
    "created_at":"2026-07-11T01:30:00Z","updated_at":"2026-07-11T02:00:00Z"},
  "product":{"id":1,"product_type":"card_rental","gpu_model":"NVIDIA H100","card_count":8,
    "machine_count":null,"total_pflops_approx":null,"power_capacity_kw":null,"rack_count":null,
    "cpu_spec":"2x Intel Xeon","memory_spec":"1TB","storage_spec":"8TB NVMe",
    "bandwidth_spec":"25Gbps","delivery_mode":"bare_metal","pricing_mode":"monthly",
    "region":"华北","self_operated":false},
  "supplier":{"name":"中联数据","self_operated":false,"credit":null},
  "delivery":{"access_status":"delivered","access_expires_at":"2026-08-11T02:00:00Z",
    "revoked_at":null,"confirmed_by_buyer":true,"buyer_confirmed_at":"2026-07-11T02:00:00Z",
    "created_at":"2026-07-11T01:55:00Z"},
  "actions":{"can_confirm":false,"can_renew":true,"can_refund":true,"can_view_credential":true}
},"request_id":"req_xxx"}
```

`delivery` 和 `supplier.credit` 没有记录时为 `null`。详情不返回 `buyer_id`、`supplier_id`、密文、`access_key` 或凭证明文；凭证仍通过独立的 access-credential 接口查看。当前数据库没有订单事件表和商品快照，因此此接口不伪造区块链时间线，`product` / `supplier` 是当前关联资料。

## POST /dev/fixtures/buyer-orders · 为当前用户生成本地订单（仅 debug）

无需请求体。接口仅在 `server.mode != release` 时注册，使用 JWT 的 `user_id` 幂等写入 4 条开发订单；生产环境没有该路由。

```json
{"code":0,"message":"success","data":{"orders":[
  {"order_no":"ORDDEV000000000801","status":"pending_payment"},
  {"order_no":"ORDDEV000000000802","status":"paid"},
  {"order_no":"ORDDEV000000000803","status":"active"},
  {"order_no":"ORDDEV000000000804","status":"completed"}
],"count":4},"request_id":"req_xxx"}
```

---

## POST /orders/:id/confirm · 确认签收 ✅ buyer

路径参数必须传 `order_no`，无需请求体。仅订单本人可以确认 `provisioning` 且访问凭证状态为 `generated` 的订单；成功后订单转为 `active`，凭证转为 `delivered`。

```json
{"code":0,"message":"success","request_id":"req_xxx"}
```

- 非订单本人：`code=40300`
- 状态不允许、尚未生成凭证或重复签收：`code=40900`
- 订单不存在：`code=40400`

## POST /orders/:id/renew · 续费 ✅ buyer
```json
{"duration": 720}
```

## POST /orders/:id/refund · 申请退款 ✅ buyer

---

## POST /orders/:id/deliver · 回填凭证 ✅ supplier
```json
{"ip_address":"10.0.1.128","ssh_port":22,"username":"root","credential_note":"密码已私信"}
```
交付信息与访问凭证均使用 AES-256-GCM 加密存储；未配置 `security.credential_key` 时明确失败，不降级为明文。

---

## GET /supplier-applications · 我的供给方入驻申请 ✅ authenticated
## POST /supplier-applications · 提交供给方入驻申请 ✅ authenticated + KYC

POST 使用 `multipart/form-data`，`business_license` 必须为 PDF/JPG/PNG 且不超过 5MB；完整字段和文件内容写入 MySQL。审核通过后，服务端在同一事务中将申请置为 `verified`、授予 `supplier` 角色并写入审计日志。用户不能通过通用角色接口绕过审核。

---

## GET /supplier/products · 我的商品 ✅ supplier
## GET /supplier/orders · 供给方订单 ✅ supplier
## GET /supplier/qualifications · 我的资质 ✅ supplier
## POST /supplier/qualifications · 提交资质 ✅ supplier

---

## 订单状态说明

| 状态 | 含义 | 下一步 |
|------|------|--------|
| pending_payment | 待支付 | 15分钟内支付否则自动取消 |
| paid | 已支付待开通 | 等待供给方开通 |
| provisioning | 开通中 | 等待买家确认签收 |
| active | 履约中 | 可使用算力 |
| completed | 已完成 | 到期自动完成 |
| cancelled | 已取消 | — |
| refunding | 退款中 | — |
| refunded | 已退款 | — |
| frozen | 已冻结 | 风控冻结 |


## 交易流程修复（2026-09-05）

- 发布、重提：有效 `supplier` 角色，个人或企业认证为 `verified`，至少一项有效供给方资质。缺少准入返回 `40300`；合规承诺必须为 `true`。
- 下单、续租和面议询价：有效 `buyer` 角色且个人或企业认证为 `verified`。下单明确检查 `compliance_agreed=true`；数量、周期、金额和库存由服务端计算校验。
- `POST /supplier/products` 返回 `{id}`，初始状态 `pending`。旧文档的 `POST /products` 不是真实发布路由。
- `POST /admin/audits/products/:id/reject` 必须提供 `{"reason":"具体修改要求"}`（1–256 字），仅 `pending → draft`。供给方 `/supplier/products` 和 `/supplier/products/summary` 返回 `rejected_reason`。
- `PUT /supplier/products/:id` 使用与发布相同的完整请求体，校验归属且仅允许 `draft`。成功后 `pending`，清空旧原因，重新进入审核队列。修改已上架商品或重复审批返回 `40900`。
- `POST /admin/audits/products/:id/approve` 仅允许 `pending → active`。市场列表仅展示 `active`。
- `POST /products/:id/inquiries` 请求 `{contact_name, contact_phone, message}`。姓名 1–64 字，电话 6–20 字符，需求 5–2000 字。仅 `active` 且面议商品接受询价。返回 `{id}` 为 CRM 线索编号，`type=compute`，包含商品、供给方、买家编号和需求，运营通过 `GET /admin/leads` 读取；CRM“由我跟进”复用 `POST /admin/leads/:id/assign`（`assignee_id` 为当前管理员），成功后显示负责人及 `assigned` 状态。
- `PATCH /admin/orders/:id/status` 支持数字订单 ID 和业务订单号，先解析真实订单再执行状态流转；不存在的订单返回 `40400`，重复取消不重复释放库存。
- 页面入口：商品列表“修改并重提”、商品详情“申请报价”、订单详情“前往支付”和“确认签收”。支付渠道可用性见 payment-api.md。

数据库增量：`015_product_review_reason`、`016_compute_inquiry_leads`；先迁移再启用对应接口。
