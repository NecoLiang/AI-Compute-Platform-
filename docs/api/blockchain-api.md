# 区块链存证 Blockchain API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 两个查询接口均为**公开**（第三方独立查验是存证的意义所在）；补推接口需 operator/admin

> 历史验收记录（2026-08-30）记载 BSN 文昌链接入；不代表当前环境已配置或可用。
> 当前环境的链路状态以运行配置和实时查验为准。

**口径**

- **链上只有 hash，没有业务明文**。订单号、买卖双方、金额等原始数据在平台库；上链的是
  载荷的 SHA-256。因此"查链上凭证"＝平台重算 hash 与链上比对（verify 接口做的事），
  第三方可拿 `verify_url` 到浏览器独立核对同一笔交易。
- 存证事件由业务动作**自动异步**产生，三类 `target_type`：
  - `order` 下单成功（REQ-H-010）
  - `delivery` 买家确认签收（REQ-H-011）
  - `violation` 订单冻结/风控处置（REQ-H-014，运营不可跳过）
- 异步上链（REQ-H-020）：业务毫秒级返回，worker 每 30s 批量推链；成功后从 `pending` 变 `confirmed` 并回写交易 hash。链路未配置或故障时不能保证确认时间。
- `verified:true` 的唯一条件：**业务库重算 hash 一致 && 链上存在该 hash**。
  未上链、数据被改动、链路故障一律如实返回 `false`（绝不虚报），前端按状态区分展示。

---

## GET /blockchain/verify · 验证链上存证（公开）

```
curl "http://localhost:8080/api/v1/blockchain/verify?type=order&id=ORD20260830xxxxxx"
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| type | string | ✅ | order / delivery / violation |
| id | string | ✅ | 订单号（order/delivery/violation 均以订单号为主键） |

**验证通过**（响应结构示例）
```json
{"code":0,"data":{
  "verified":true,
  "tx_id":"1716251C3392716BA51046F6ECB66F146A17136BB2F19CDFF6B5A8AD500C857C",
  "chain_status":"confirmed",
  "data_hash":"0x5285bf837fac2a793cbbfd1ad8bfb01da5f2ee0f8dc0aa5db08ff338b052815e",
  "db_hash_match":true,
  "chain_timestamp":"2026-08-30T02:40:28Z",
  "verify_url":"https://wenchangexplorer.bsnbase.com/#/txs/1716251C..."
}}
```

**其他状态**（均 `verified:false`，靠 `note`/`chain_status` 区分）

| 场景 | 特征字段 |
|---|---|
| 无存证记录 | `note:"无存证记录"` |
| 已记录、尚未上链 | `chain_status:"pending"`，note 提示尚未上链 |
| 业务数据在存证后被改动 | `db_hash_match:false`，note 明确提示 —— **这是防篡改检出，应醒目告警** |
| 链路故障（网关不可达等） | note 提示链上查询失败，稍后重试 |

**前端展示建议**（docs/14 REQ-H-031）
- 订单详情页存证时间线：每类事件一行，`confirmed` 显示 ⛓ 图标 + 链上时间戳；
  `pending` 显示"上链中"；只在 `verified:true` 时文案用"已上链可查验"
- ⛓ 图标点开 panel：展示 `data_hash`、`tx_id`，附「去区块链浏览器查验」按钮 → `verify_url` 新窗口打开

---

## GET /blockchain/attestations/:target_type/:target_id · 原始存证记录（公开）

```
curl http://localhost:8080/api/v1/blockchain/attestations/order/ORD20260830xxxxxx
```

返回平台库的存证行：`data_hash`、`signers`（平台见证签名，Ed25519）、`chain_tx_id`、
`chain_status`、`attempts`、`last_error`、`created_at`、`confirmed_at`。无记录返回 `code:0`，`data` 可能省略或为 `null`。

---

## POST /admin/blockchain/requeue-failed · 死信补推（operator/admin）

链路长时间故障（重试 ×3 耗尽进死信）恢复后调用，把 `failed` 全部重置回 `pending` 待 worker 补推。

**响应**: `{"code":0,"data":{"requeued":2}}`


## 错误与查验边界（2026-09-14）

- 三个接口都必须同时检查 HTTP 状态和 JSON `code`。数据库查询/更新错误使用现有错误信封：HTTP 200、`code:50000`、`message`，不返回成功 `data`；不能当作无记录或零条补推。
- 未注册原始业务数据 source 时，`verified:false` 且省略 `db_hash_match`。当前路由只注册 `order`/`delivery`；`violation` 可以读取记录，但在补齐可重建业务数据前不会通过完整查验。
- 业务数据读取失败或链路不可用均为 `verified:false`，通过 `note` 说明未完成阶段。`confirmed` 仅是存储的上链状态，不等于实时查验通过。
- 前端仅在 `verified:true`、`db_hash_match:true`、`chain_status:confirmed` 且摘要和交易编号完整时显示查验通过。平台见证签名不等于买卖双方电子签署。
- 补推影响全平台所有 `failed` 记录；重复调用只更新当时仍为 `failed` 的记录，零条是合法结果。UI 要求全局范围确认并在请求期间阻止重复点击；返回只代表重新排队，不代表已上链。
- 前端入口：`/attestations/verify`（公开）、买家订单详情的存证时间线、`/admin/attestations`（operator/admin）。先发布后端错误契约，再开放前端入口。本次验证使用独立本地数据库和既有 fake-chain；未调用生产链。
