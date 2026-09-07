# 页面 × 接口清单（前端 AI Coding 对照表）

> 2026-09-07 依据前端 `src/app` 路由与后端 `main` 分支逐页核对生成。
> 通用约定（响应包裹/鉴权/错误码/金额单位）见 [README.md](README.md)。
> ✅=前端已接且字段一致 ｜ 🆕=后端就绪、页面待前端实现 ｜ ⬜=页面存在但接口待接

**全局字段口径（页面渲染前先确认这四条）**
1. 所有金额字段单位是**分**（`unit_price`/`total_amount`/`platform_fee`/`amount`…），页面展示除以 100
2. `duration` 是**计费周期数**不是小时：hourly=小时 daily=天 weekly=周 monthly=月 perpetual=1
3. 时间一律 ISO8601 字符串；可空时间为 `null`
4. 列表接口统一 `page`/`page_size` 入参，`data.list`+`data.total` 出参

---

## 一、公开页面

### `/market` 算力市场列表 ✅
- `GET /products` — 筛选参数：`q` `product_type(card_rental|outright|center|colocation)` `gpu_model` `region` `delivery_mode(bare_metal|container|rack|vm)` `pricing_mode(hourly|daily|weekly|monthly|perpetual)` `available_hours` `price_min/price_max`(分) `card_count_min` `sort` `page` `page_size`
- 商品卡片关键字段：`gpu_model` `card_count` `stock` `unit_price`(分/卡·周期) `pricing_mode` `region` `self_operated` **`health`**(unknown/healthy/degraded/offline —— offline 应显示「暂不可下单」灰态，unknown 不显示徽章)
- 🆕 **智能选型入口**（页面待做）：`POST /market/agent-search`（需登录），入参 `{query: string ≤500字}`；响应字段：
  - `relevant` false 时只渲染 `reject_reason`
  - `analysis_steps[] {title, detail}` — 建议打字机逐步展示
  - `compute_estimate {total_vram_gb(float), per_card_vram_gb, min_cards, compute_class, basis}` — 做成「算力推定卡」，`basis` 含推导公式是核心展示位
  - `matches[] {product(同商品结构), score(0-100), reasons[]}` — 商品卡+匹配理由
  - `note` 无匹配时的提示文案
  - 错误码：42900 限流(10 次/分)、50000 网关异常；详见 [agent-search-api.md](agent-search-api.md)

### `/market/[productId]` 商品详情 ✅
- `GET /products/:id` — 全字段见 compute-api.md；注意 `machine_count`/`total_pflops_approx`/`power_capacity_kw`/`rack_count` 可为 `null`（colocation/center 专属字段）
- `GET /gpu-catalog?q=<gpu_model>` 可选：用型号库补充展示显存/算力/安可徽章

### `/market/[productId]/inquiry` 询单 ✅
- `POST /products/:id/inquiries`

### `/attestations/verify` 存证查验 🆕（导航已有死链，页面待做）
- `GET /blockchain/verify?type=order|delivery|violation&id=<订单号>`（公开，无需登录）
- 响应字段渲染规则：`verified=true` → 绿色⛓「已上链可查验」+ `chain_timestamp` + `verify_url`「去区块链浏览器查验」外链按钮；`chain_status=pending` →「上链中」；`db_hash_match=false` → 红色告警「数据与存证不一致」；`note` 为兜底文案
- `GET /blockchain/attestations/:target_type/:target_id`（公开）：原始存证记录（`data_hash`/`signers`/`chain_tx_id`/`confirmed_at`），详见 [blockchain-api.md](blockchain-api.md)

### `/(portal)/terms|privacy|resource-listing-rules|resource-usage-rules` 协议页 ✅
- 静态正文，无后端接口；版本号 `2026-09-06.1` 随表单提交（见 [legal-consent-api.md](legal-consent-api.md)）

---

## 二、认证与账户

### `/auth/login` `/auth/register` `/auth/verify` ✅（经 BFF `/api/auth/*`）
- `POST /auth/captcha/verify`（Cap token 换 captcha_token）
- `POST /auth/sms/code` `{phone, purpose(login|register), captcha_token}`
- `POST /auth/sms/login` `{phone, sms_code, remember}`
- `POST /auth/register` `{phone, sms_code, agree_tos:true, terms_version, privacy_version}` ⚠️ 两个 version 必填当前版本，缺失/旧版本返回 40001
- 微信登录 ✅ 后端已合并上线：`GET /auth/wechat/status`→`{enabled}`；`POST /auth/wechat/start`；`POST /auth/wechat/exchange`；`POST /auth/wechat/bind`（详见 [wechat-login-api.md](wechat-login-api.md)；生产尚未配置微信 AppID，status 返回 enabled=false，按钮隐藏是预期行为）

### `/auth/identity` 实名认证 ✅
- `POST /user/kyc/personal` `{real_name, id_card, sensitive_data_agreed:true, privacy_version}`
- `POST /user/kyc/enterprise` FormData（含营业执照文件）+ 同上两个同意字段
- `GET /user/kyc/status`
- `GET /auth/consents`：当前用户同意记录（后端已上线，页面可加「我的授权记录」）

---

## 三、买家控制台 `/console/buyer/*`

### `/checkout` 下单 ✅
- `GET /products/:id`（回显）+ `POST /orders` `{product_id, quantity, duration(周期数), compliance_agreed:true, compliance_version}`
- 下单被拦截的两种业务错误要区分展示：库存不足 / 「供应方算力节点已全部离线」(health=offline)

### `/console/buyer/orders` + `[orderId]` 订单 ✅
- `GET /orders?status&order_no&page&page_size`、`GET /orders/:orderNo`
- 状态枚举：`pending_payment|paid|provisioning|active|completed|cancelled|refunding|refunded|frozen`
- 详情响应含 `actions {can_confirm, can_renew, can_refund, can_view_credential}` —— **按钮显隐直接用这组字段，不要前端自行推断**
- `POST /orders/:id/confirm`（签收）、`POST /orders/:id/renew` ⬜、`POST /orders/:id/refund` ⬜（后端就绪，按钮待接）
- 凭证：`GET /orders/:id/access-credential`（脱敏）、`POST .../reveal`（明文，二次确认后调用）
- 🆕 订单详情可加「存证时间线」：复用 `/blockchain/verify?type=order|delivery&id=<orderNo>`
- 支付：`POST /payment/pay` → 响应含 `pay_url`；`GET /payment/status/:order_no` ⬜（轮询支付结果，待接）

### `/console/buyer/invoices` 发票 ✅
- `GET/PUT /invoices/title`、`GET /invoices/billable-orders`、`POST /invoices/apply`、`GET /invoices?status`、`GET /invoices/:invoice_no/download`(PDF 二进制)

### `/console/buyer/tickets` + `[ticketNo]` 工单 ✅
- `POST /tickets`、`GET /tickets?status&keyword`、`GET /tickets/:ticket_no`、`POST .../messages`、`POST .../close`

### `/console/buyer/messages` 通知 ✅
- `GET /notifications?type(order|ticket|system)`、`POST /notifications/:id/read`、`POST /notifications/read-all`、`DELETE /notifications/:id`；列表响应的 `unread` 是全类型未读数（角标用）

### `/console/buyer/profile` `/console/buyer/billing` ⬜
- `GET/PUT /user/profile`（后端就绪待接）；billing 可复用 `GET /payment/...` 系列

---

## 四、供应方控制台 `/console/supplier/*`

### `/supplier/apply` 入驻申请 ✅
- `GET/POST /supplier-applications`

### `/console/supplier/qualifications` 资质 ✅
- `GET/POST /supplier/qualifications`（FormData 上传证照）

### `products|centers|colocation` 发布与管理 ✅
- `GET /supplier/products`、`GET /supplier/products/summary`、`POST /supplier/products`、`PUT /supplier/products/:id`(驳回重提)
- 发布必带 `compliance_agreed:true, compliance_version`；型号下拉 `GET /gpu-catalog`（字段见 [gpu-catalog-api.md](gpu-catalog-api.md)，`secure_certified=true` 展示「安可认证」徽章）
- 被驳回商品展示 `rejected_reason`

### `/console/supplier/orders` 订单交付 ✅
- `GET /supplier/orders?status`、`POST /orders/:id/deliver`(回填凭证)
- 🆕 交付面板可加「调度建议」：`GET /supplier/schedule-advice?order_no=` → `{need_cards, summary, nodes[]{node_name, status, available_cards/total_cards, score, verdict(recommended|alternative|unavailable), reasons[]}}` —— recommended 高亮、reasons 直接展示

### 节点管理（页面待建，建议 `/console/supplier/nodes`）🆕
- `POST /supplier/nodes` `{product_id, node_name, total_cards}` → 响应 `node_key` **仅此一次展示**，页面必须提供复制按钮+丢失提示
- `GET /supplier/nodes`：列表渲染 `status`(online 绿/degraded 黄/offline 灰) + `last_heartbeat_at` + `available_cards/total_cards` + `gpu_util_pct`
- `DELETE /supplier/nodes/:id`
- 页面附节点侧接入说明（30s 心跳 curl 示例见 [scheduler-api.md](scheduler-api.md)）

### `/console/supplier/settlements` 结算 ✅
- `GET /supplier/settlements?status`、`GET /supplier/settlements/summary`

### `/console/supplier/inventory` 盘点 ✅
- `GET /supplier/resource-syncs?product_id`、`POST /supplier/resource-syncs/passive`

### `/console/supplier/messages` ✅ 同买家通知四件套

---

## 五、运营后台 `/admin/*`（BFF 通用透传 `/api/admin/[...path]`）

| 页面 | 接口 | 状态 |
|---|---|---|
| `/admin/reviews` | `GET /admin/audits/qualifications[?status=all]`、`POST .../:id/approve\|reject`、`GET .../:id/document`(文件)、`POST /admin/audits/products/:id/approve\|reject` | ✅ |
| `/admin/products` | `GET /admin/products?status`、`PATCH /admin/products/:id/offline` | ✅ |
| `/admin/orders` | `GET /admin/orders`、`PATCH /admin/orders/:id/status`（改 frozen 会自动吊销凭证+违规上链存证） | ✅ |
| `/admin/finance` | `GET /admin/payment/list`、`GET /admin/invoices?status`、`POST /admin/invoices/:id/issue`(FormData: pdf+tax_invoice_no)、`POST /admin/invoices/:id/reject`；`GET /admin/payment/reconcile` ⬜ | ✅ |
| `/admin/tickets` | `GET /admin/tickets`、`POST /admin/tickets/:id/claim\|resolve\|close`；`GET /admin/tickets/:id` + `POST .../messages` ⬜（详情/回复待接） | ✅ |
| `/admin/crm` | `GET /admin/leads`、`POST /admin/leads/:id/assign` | ✅ |
| `/admin/risk` | `GET /admin/risk/alerts`、`POST .../:id/freeze\|dismiss` | ✅ |
| `/admin/users` | `GET /admin/users`、`PATCH /admin/users/:id/freeze` | ✅ |
| `/admin/audit` | `GET /admin/audit-logs` | ✅ |
| `/admin/settings` | `GET/PUT /admin/config`；`GET/POST/PUT /admin/gpu-catalog` ⬜（型号库维护界面待接，字段见 gpu-catalog-api.md） | ✅ |
| `/admin/cms` | `GET/POST /admin/cms/notices` | ✅ |
| 节点总览（建议并入 `/admin/settings` 或新页）🆕 | `GET /admin/nodes?status`、`GET /admin/schedule-advice?order_no=`、`POST /admin/blockchain/requeue-failed`(存证死信补推) | 🆕 |

---

## 六、暂无后端支撑的页面（前端占位，勿接假数据上生产）

- `/console/funder`、`/admin/tokens`（Token 工厂板块，后端未排期）
- `/console/vendor`：后端已有设备市场接口（`/equipments*`、`/vendor/equipments*`、`/vendor/leads`、`/leads/:id/quote|close`、`/commissions`），前端整块未接 ⬜
- `/console/supplier/analytics`：无专用统计接口，可先用 settlements/summary + orders 拼

---

**维护约定**：本表与 docs/api/*.md 均以**后端仓库为唯一权威源**，接口变更与文档在同一 commit 更新；前端仓库的 docs/api 为同步副本，不要单独修改。
