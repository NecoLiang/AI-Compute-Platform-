# 运营管理 Admin API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 需 operator 或 admin 角色

---

## 审核中心

### GET /admin/audits/qualifications · 资质审核列表

默认返回待审核记录；传 `status=all` 返回包含已通过、已驳回在内的完整审核台账。

### GET /admin/audits/qualifications/:id/document · 下载申请附件
### POST /admin/audits/qualifications/:id/approve · 通过
### POST /admin/audits/qualifications/:id/reject · 驳回

`supplier_onboarding` 类型为供给方入驻申请；通过时服务端会在同一事务中授予申请账户 `supplier` 角色并写入审计日志。

```
curl -X POST http://localhost:8080/api/v1/admin/audits/qualifications/1/reject \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"reason":"证照已过期，请更新后重新提交"}'
```

### POST /admin/audits/products/:id/approve · 通过商品审核
### POST /admin/audits/products/:id/reject · 驳回商品审核

---

## 订单管理

### GET /admin/orders · 全平台订单

每条订单新增 `allowed_actions`，只包含当前允许的 `cancelled` / `frozen` 操作；没有可用操作时返回 `[]`。历史 `stock_reserved=NULL` 的订单不开放取消，需先核对真实库存占用。页面以服务端能力为准，提交时服务端仍会在行锁内重新校验。
```
curl "http://localhost:8080/api/v1/admin/orders?status=active&page=1&page_size=20" \
  -H "Authorization: Bearer <token>"
```

### PATCH /admin/orders/:id/status · 订单干预

- 仅接受 `cancelled`（关闭尚未终结的订单）和 `frozen`（冻结待支付、已支付、交付中或使用中的订单）。不接受直接设置支付、签收、完成或退款状态；这些状态必须来自对应业务流程。
- 关闭允许 `pending_payment/paid/provisioning/active/frozen`，不自动退款；退款申请中及终态不允许关闭。关闭只释放该订单已记录的实际库存占用；冻结保留占用、吊销平台访问凭证，并取消待支付续租。状态、库存、凭证与运营审计在同一事务提交；重复同一操作不会重复写审计或释放库存。
- 不支持的目标状态返回 `40001`，已变化或不可处置的状态返回 `40900`，不存在的订单返回 `40400`。历史库存未核对时关闭返回 `40001` 和明确原因；数据库或审计失败返回 `50000` 并回滚。
- 原路退款尚未接入，此接口不能替代资金渠道退款，也不能证明机房侧已撤销实际访问。
```
curl -X PATCH http://localhost:8080/api/v1/admin/orders/ORD001/status \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"status":"frozen"}'
```
- 托管订单（v1.2）：冻结同事务暂停租约（`suspended`）并断开现有 SSH 会话，容器与 GPU 保留；冻结后不走"只能关闭/退款"，可用下面的解冻接口恢复。

### POST /admin/orders/:id/unfreeze · 解冻托管订单（v1.2 S2 T-088）

请求体可省略或 `{"wrong_freeze":true}`；响应 `{"order_no":"...","status":"active|paid|pending_payment|completed","wrong_freeze":false}`。订单回到的状态由租约实况决定，写审计 `unfreeze_order`。完整规则与错误码见 compute-api.md「托管订单冻结与解冻」。人工订单 40001。

---

## 商品管理

### GET /admin/products · 全平台商品
### PATCH /admin/products/:id/offline · 强制下架

---

## 风控工作台

### GET /admin/risk/alerts · 告警列表

```
curl "http://localhost:8080/api/v1/admin/risk/alerts?level=high&page=1&page_size=20" \
  -H "Authorization: Bearer <token>"
```

**响应**
```json
{"code":0,"message":"success","data":{"list":[{
  "id":7,"level":"high","alert_type":"mining_freeze",
  "target_type":"order","target_id":32,
  "rule_detail":"{\"lease_no\":\"l-4g7bq4xc\",\"order_no\":\"ORD20261101103000123abc\",\"node_id\":7,\"hits\":[{\"kind\":\"stratum\",\"detail\":\"pid 4303 kworker: stratum+ssl://1.2.3.4:443\",\"freeze\":true}]}",
  "status":"processing","created_at":"2026-11-01T10:30:00+08:00"
}],"total":1,"page":1,"page_size":20},"request_id":"..."}
```

- 查询参数只有 `level`（`high/medium/low`）与分页；按 `created_at` 倒序。`rule_detail` 是字符串（挖矿告警为 JSON 文本），无值时为空串；列表不返回 `resolution/operator_id`。
- 托管租约挖矿特征扫描（v1.2 S2 T-088，`fleet.enabled=true`）写入两类告警，`target_type` 均为 `order`（详见 managed-rental-api.md「租约生命周期 · 挖矿特征扫描」）：
  - `mining_freeze`（`level=high`）：命中 stratum 参数或矿池地址外连，订单**已自动冻结**，告警直接为 `processing`（`resolution=auto_frozen`），同事务写系统审计 `mining_auto_freeze`。
  - `mining_suspect`（`level=medium`）：仅命中矿工进程名或矿池常用端口，`pending` 待人工核查（可冻结或标记误报）；同一订单有未结挖矿告警或 24h 内已写过时不重复写。

### POST /admin/risk/alerts/:id/freeze · 冻结处置
### POST /admin/risk/alerts/:id/dismiss · 标记误报

冻结接口无请求体，按告警保存的 `target_type/target_id` 解析对象：`order` 冻结订单并吊销凭证；`user/account` 冻结账户并使已有会话失效。不能冻结当前操作人。对象/告警不存在返回 `40400`，无效类型返回 `40001`，状态冲突返回 `40900`。

托管订单（`fulfillment_mode=managed`）冻结时同事务把在租租约转 `suspended` 并断开现有 SSH 会话（容器保留），解冻用 `POST /admin/orders/:id/unfreeze`（compute-api.md「托管订单冻结与解冻」）。对已是 `processing` 的告警（例如 `mining_freeze`）调用 `freeze` 会对目标再执行一次冻结后转 `resolved`：订单仍为 `frozen` 时幂等，订单已解冻则会被重新冻结，因此应先结案再解冻。

目标冻结、审计与告警 `processing` 在同一 MySQL 事务提交；账户会话失效处理成功后才变为 `resolved`。会话失效处理失败返回 `50000`，保持 `processing`，允许重试冻结；数据库账户状态也会阻止已有令牌访问。重复冻结不会重复写冻结审计。`dismiss` 只允许 pending，重复 dismissed 幂等，不允许忽略 processing/resolved。


---

## 托管节点（v1.2，`fleet.enabled=true`）

完整契约见 [managed-rental-api.md](managed-rental-api.md#运营接口)。租约列表 `GET /admin/leases`、访问会话 `GET /admin/access-sessions` 等运营接口只在该文档中定义。

### GET /admin/nodes · 节点总览
托管节点附带 `managed`（最近探活结论 / 卡槽统计 / 指纹确认状态）。

### POST /admin/nodes/:id/suspend · 停用节点
`{"reason":"..."}`（可选，≤200 字）。停止新分配、释放无租约卡槽、作废排队任务、重算库存、通知供给方、写审计；存量租约不受影响。40400 非托管节点；40900 已停用。

### POST /admin/nodes/:id/resume · 恢复节点
回到 `unverified` 并自动入队完整探活，探活通过才回到 `verified`。响应 `{"verify_status":"unverified","task_id":...}`；40900 不在停用状态。

### GET /admin/node-tasks?state=&node_id=&page=&page_size= · 节点任务
`state` ∈ `pending/running/succeeded/failed/dead`。分页信封 `{list,total,page,page_size}`。

### POST /admin/node-tasks/:id/retry · 重试死信
仅 `dead` 可重试（否则 40900），次数清零回 `pending`，写审计 `node_task.retry`。

```
curl -X POST http://localhost:8080/api/v1/admin/nodes/7/suspend \
  -H "Authorization: Bearer <token>" -H "Content-Type: application/json" \
  -d '{"reason":"多次探活失败, 待供给方排查"}'
```

### GET /admin/lease-outages · 停机检索（S2 T-105）
`?lease_no=&order_no=&cause=&supplier_id=&open=1&refund_pending=1&page=&page_size=`；含认定原文 `detail` 与责任金额 `liability_amount`（分）。契约见 [managed-rental-api.md](managed-rental-api.md#运营接口)。

### POST /admin/supplier-breach-refunds · 供给方违约未使用部分退款（S2 T-106）
docs/25 §25.7.6「供给方约束」的运营发起路径（买家自助申请未实现）。

```json
{"order_no":"ORD20261101103000123abc","reason":"供给方节点连续离线 3 小时"}
```

- `order_no` 为托管**原订单号**（续租单号返回 40001「订单无租约(续租单请使用原订单号)」）；`reason` 必填。
- 门槛：当前所属订单（原订单或已生效的续租单）期内**供给方责任**停机累计 ≥ 租期 × `fleet.outage.refund_ratio`，或单次 ≥ `refund_single_hours` 小时（与 Q-47 退费门槛同参数，平台责任停机不计；进行中的停机按已持续时长计）。
- 执行：进行中的停机先在当前时刻结束并按租约补偿方式结算；然后同一事务内把访问期限（租约 `access_until`、订单 `lease_end_at`、交付 `access_expires_at`）截断到当前时刻，登记 `payment_refunds(kind='supplier_breach', source_id=租约 ID)`，金额 = `floor(所属订单实付 × 剩余访问秒数 ÷ 所属订单租期秒数)`，封顶为该支付尚未登记部分退款的余额；写审计 `lease.supplier_breach_refund`。提交后立即向易宝发起退款，提交失败由定时任务继续推进。之后租约照常进入宽限 → 清除 → 租期末分账，违约退款全额计入供给方责任。
- 一个租约至多一次（`UNIQUE(kind, source_id)`）。

返回：`{"order_no","refund_order_no","lease_no","refund_id","amount","access_until"}`（`refund_order_no` 为退款所在订单，续租后为当前续租单）。

| 场景 | code |
|---|---|
| `order_no` / `reason` 缺失；订单无租约 | 40001 |
| 订单不存在或不是托管订单 | 40400 |
| 租约不在租期内；未达违约门槛；存在待处理或已预付的后续续租；没有可退的未使用金额；已发起过违约退款 | 40900 |
| 部分退款通道未打开（`fleet.outage.yeepay_partial_refund_confirmed=false` 或未接入） | 50000 |

### GET /admin/payment/partial-refunds · 部分退款列表（S2 T-106）
> 本接口属 payment 模块，**不受 `fleet.enabled` 影响、始终注册**（放在本节仅因与停机补偿/违约退款相关）。

`?status=pending|processing|success|failed&order_no=&page=&page_size=`（`page_size` ≤ 100），按 id 倒序，分页信封 `{list,total,page,page_size}`。`list[]`：

```json
{"id":7,"payment_id":12,"order_no":"ORD…","refund_request_id":"P…","amount":7800,"kind":"outage_compensation",
 "source_id":31,"reason":"算力订单停机补偿退费","status":"success","yeepay_refund_no":"…","last_error":null,
 "created_at":"…","updated_at":"…"}
```

`kind`/`source_id`/状态机见 payment-api.md「租期末结算与部分退款」。`status` 非法返回 40001。`failed` 行会在 1 小时后自动换新请求号重提，无需人工操作；长期 `failed` 需人工核对易宝侧。

### 供给方信用（S2 T-105）

沿用 `credit_scores`（每个托管供给方一行）。计分口径与停调度规则见 [managed-rental-api.md](managed-rental-api.md#供给方信用与停调度)；后台每 10min 重算，供给方责任停机结算后即时重算。阈值 `fleet.credit.stop_below`（默认 60）。

#### GET /admin/supplier-credits?stopped=1&page=&page_size= · 信用列表
读已落库结果（不逐个重算），按 `score` 升序；`stopped=1` 只看已停调度。分页信封 `{list,total,page,page_size}`，`list[]` 为 `CreditScore`：

```json
{"supplier_id":20,"score":85,"outage_minutes_30d":0,"outage_count_30d":0,"probe_failures_30d":3,"defaults_30d":0,
 "dispatch_stopped":true,"override_mode":"auto","override_by":null,"override_reason":null,"override_at":null,
 "computed_at":"2026-11-01T10:30:00+08:00"}
```

#### GET /admin/supplier-credits/:id · 信用详情
`:id` 为供给方用户 id。先即时重算再返回 `CreditScore` + `stop_below` + `nodes[]`：
`{"id","node_name","dispatch_hold","dispatch_hold_reason","dispatch_hold_at","schedulable","outage_minutes_30d"}`。
40001 `:id` 非正整数；40400「该供给方没有平台托管节点」。

#### PUT /admin/supplier-credits/:id/override · 运营覆盖
```json
{"mode":"allow","reason":"已核实为探活镜像问题"}
```
`mode`：`auto` 按分数自动；`allow` 强制放行（含单节点停机超限；之后再发生计入的供给方责任停机即自动回到 `auto`）；`stop` 强制停调度（该供给方全部托管节点 `dispatch_hold=1`，已在租的不受影响）。`allow/stop` 必须填 `reason`（≤254 字）。立即重算并同步节点，返回同详情结构；写审计 `supplier.credit_override`（`target_type=supplier`，before/after 含 `override_mode`）。

| 场景 | code |
|---|---|
| `mode` 缺失或不是 auto/allow/stop；allow/stop 未填理由；`:id` 非法 | 40001 |
| 供给方没有托管节点 | 40400 |
| 并发下信用记录已变化 | 40900 |

---

## 用户管理

### GET /admin/users · 用户列表
### PATCH /admin/users/:id/freeze · 冻结用户

---

## 审计与配置

### GET /admin/audit-logs · 审计日志

```
curl "http://localhost:8080/api/v1/admin/audit-logs?page=1&page_size=20" \
  -H "Authorization: Bearer <token>"
```

分页返回 `{id,operator_id,action,target_type,target_id,before_value,after_value,ip,created_at}`，按时间倒序，暂无过滤参数。系统动作（自动冻结、清除回执）库内 `operator_id` 为空，接口中显示为 `0`。v1.2 托管相关 `action`：

| action | target_type | 说明 |
|---|---|---|
| `mining_auto_freeze` | `order` | 挖矿特征自动冻结；`before_value`=冻结前订单状态，`after_value`=`"frozen; alert=<告警id>; lease=<lease_no>; <kind>: <detail>; ..."` |
| `unfreeze_order` | `order` | 运营解冻托管订单；`before_value="frozen"`，`after_value={"status":"<目标状态>","wrong_freeze":<bool>}` |
| `lease.purged` | `lease` | 租约清除回执（JSON，结构见 managed-rental-api.md「清除与清除回执」） |
| `lease.cleanup` | `lease` | 换节点 / 开通失败后旧节点残留清理回执（同上结构） |

### GET /admin/config · 系统配置
```json
{"code":0,"data":{"trading_enabled":true,"fee_rate":500}}
```

### PUT /admin/config · 更新配置（合规开关）

配置写入 `system_config`，服务重启后保持，并同步记录审计日志。GET/PUT 均允许 operator/admin。

- `trading_enabled=false` 阻止新订单创建，返回 `40900`；已有订单支付、交付、退款继续按原流程处理。
- `fee_rate` 是 0–10000 的整数基点，PUT 的 value 使用字符串，例如 `{"key":"fee_rate","value":"650"}` 表示 6.5%。订单创建事务读取并锁定配置，使用整数分计算费用；既有订单费用及结算不随配置变化。
- 买家预览使用公开的 `GET /trading-config`，返回同样两个字段，`Cache-Control: no-store`。读取失败、字段缺失或值非法时返回 HTTP 503 / code 50000；前端禁止提交，不能回退为 5% 或开放交易。

```
curl -X PUT http://localhost:8080/api/v1/admin/config \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"key":"trading_enabled","value":"false"}'
```

---

## 内容管理

### GET /admin/cms/notices · 已发布公告
### POST /admin/cms/notices · 发布公告

公告与审计日志在同一事务中写入；发布成功后可由 GET 接口立即回读。
```json
{"content":"<p>系统维护通知</p>"}
```

---

## 模型目录（v1.2 一键部署）

完整字段、校验规则与状态机见 [model-deploy-api.md](model-deploy-api.md#模型目录运营operatoradmin)。

### GET /admin/model-catalog?status= · 目录列表
### GET /admin/model-catalog/:id · 条目详情（含真机验证记录）
### POST /admin/model-catalog · 新建条目（一律 draft）
### PUT /admin/model-catalog/:id · 整体更新（仅 draft，否则 40900）
### POST /admin/model-catalog/:id/verifications · 录入真机验证记录
### POST /admin/model-catalog/:id/activate · 上线（无 pass 验证记录 40001）
### POST /admin/model-catalog/:id/retire · 退役（不影响运行中部署）

`active` 条目不可编辑，修改须新建条目（`revision` 变化即为新模型）；`retired` 为终态。GPU 型号库 `PUT /admin/gpu-catalog/:id` 同期新增 `compute_capability` 字段，见 [gpu-catalog-api.md](gpu-catalog-api.md)。

## 模型部署（v1.2 一键部署，`fleet.enabled=true` 时注册）

### GET /admin/deployments?state=&health=&page=&page_size= · 全局查看部署

operator/admin。`state` ∈ `pending/checking/pulling/starting/running/restarting/stopping/stopped/failed`，`health` ∈ `unknown/healthy/degraded/unhealthy`，均可选，非法取值 40001；`page_size` 1–100（缺省 20），按 id 新→旧。

```json
{"code":0,"message":"success","data":{"list":[{"deploy_no":"d-wynew4in","order_no":"TF20261031223030ABCD",
 "model":{"id":3,"model_key":"qwen2.5-7b-instruct","display_name":"Qwen2.5 7B Instruct"},
 "state":"running","health":"healthy","health_reason":null,"health_reason_text":null,"tp":2,"dp":1,
 "max_model_len":32768,"gpu_mem_util":0.9,"gpu_uuids":["GPU-6f1c…","GPU-9a02…"],"canary_enabled":true,
 "engine_cpus":63,"engine_memory_gb":508,"access":{"base_url":"http://omnis-engine:8000/v1","served_model":"qwen2.5-7b-instruct"},
 "cache":null,"restart_count":0,"last_restart_at":null,"next_restart_at":null,"fail_reason":null,"stop_reason":null,
 "started_at":"2026-10-31T20:41:05+08:00","stopped_at":null,"created_at":"2026-10-31T20:30:00+08:00",
 "updated_at":"2026-10-31T20:41:05+08:00","buyer_id":21,"node_id":1}],"total":1,"page":1,"page_size":20},"request_id":"…"}
```

字段含义同买家 `DeploymentView`（[model-deploy-api.md](model-deploy-api.md#部署对象-deploymentview)），运营列表额外带 `buyer_id`、`node_id`，不含 `events`。运营暂无代停/代重启接口（买家自助；租约冻结/到期/清除时随租约自动停止）。

---

## 告警级别

| level | 含义 |
|-------|------|
| high | 🔴 高危（如疑似挖矿） |
| medium | 🟠 中等 |
| low | 🟡 低 |
