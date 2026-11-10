# 算力撮合 Compute API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 标注 ✅ 的需 `Bearer <token>`

**供给方准入口径（2026-09-15）**：供给方的全部交易动作（发布/重提商品、交付、被动盘点等）要求**企业认证**（`enterprises.status=verified`）+ 有效供给方资质；个人实名不能替代，未达标返回 40300「供给方必须完成企业认证…」。买家下单仍为个人或企业认证任一。

**供给方名称口径（2026-09-15）**：公开市场接口（`GET /products`、`GET /products/:id`）与买家订单接口返回**脱敏名** `supplier_name`（如「北京***有限公司」，自营为「平台自营」，无企业认证为空串）；管理端 `/admin/products`、`/admin/orders`、`/admin/nodes` 返回**全名**。全名仅供给方本人与运营可见。

---

## GET /products · 商品列表（公开）

```
curl "http://localhost:8080/api/v1/products?gpu_model=H100&region=北京&pricing_mode=hourly&sort=price_asc&page=1&page_size=20"
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| gpu_model | string | — | NVIDIA H100 / 华为昇腾910B 等 |
| region | string | — | 北京/上海/深圳等 |
| pricing_mode | string | — | hourly / daily / weekly / monthly / nightly / perpetual。零租商品匹配**任一已启用价格行**（不限展示主价，停用行不匹配）；非零租商品及无价格行的存量零租商品按顶层 `pricing_mode`（T-093） |
| card_count_min | int | — | 商品规格卡数 `card_count` ≥ N（≥0，非负整数，否则 40001） |
| min_cards | int | — | 能一次租到至少 N 张卡（T-093，≥0，否则 40001）：托管商品 `max_free_per_node ≥ N`（单台节点空闲且可租的卡数，与下单分卡同口径）；人工零租 `stock ≥ N`；买断/算力中心 `card_count ≥ N`（每份所含卡数）；空心机房无卡，传入即不匹配 |
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
    "region":"北京","status":"active","self_operated":false,
    "fulfillment_mode":"manual","availability":"fulltime","night_start_min":null,"night_end_min":null,
    "card_split_options":null,"max_free_per_node":null,
    "prices":[
      {"pricing_mode":"hourly","unit_price":3500,"min_duration":1,"enabled":true},
      {"pricing_mode":"daily","unit_price":72000,"min_duration":1,"enabled":true}
    ]
  }],
  "total":47,"page":1,"page_size":20
}}
```
> `unit_price = 3500` 表示 ¥35.00 / 卡·时。前端展示时除以 100。

**v1.2 灵活计费字段（028）**

- `prices`：零租（`card_rental`）商品的全部计费方式，按周期从短到长排序；公开市场与详情只返回 `enabled=true` 的行，供给方 `/supplier/products` 与运营 `/admin/products` 返回含已停用在内的全部行。非零租商品恒为 `[]`，以顶层 `pricing_mode/unit_price/min_duration` 为准。`unit_price` 单位为 分/卡/周期。
- 顶层 `pricing_mode/unit_price/min_duration` 是**展示主价**：已启用价格中周期最短的一项，供旧前端与排序使用；下单金额以所选方式的价格行为准。
- `card_split_options`（T-092）：托管容器零租商品（`card_rental` + `managed` + `container`）买家可选的单笔卡数，升序 int 数组，如 `[1,2,4,8]`；下单 `quantity` 必须是其中之一（否则 40001），`min_order` 对这类商品不生效。其他商品恒为 `null`。
- `max_free_per_node`（T-092）：托管商品单台节点当前最多可一次租到的卡数（fleet 重算库存时回写；节点无磁盘配额或整机交付时只计全空闲整机）；人工商品为 `null`。`stock` 对托管商品是全部空闲卡合计，不代表单笔可租量，前端判断"能否租 N 卡"应看此字段或调用 availability。
- `fulfillment_mode`：`manual`（供给方人工交付）/ `managed`（平台托管开通）；`availability`：`fulltime` / `night_window`；夜间窗口商品的 `night_start_min/night_end_min` 为 Asia/Shanghai 当日分钟数（0-1439，`end<=start` 表示次日结束），其余为 `null`。

---

## GET /products/:id · 商品详情（公开）

仅返回 `active`（在售）商品，与公开市场列表保持一致。不存在或处于 `draft`（含审核驳回）、`pending`、`sold_out`、`offline`、`frozen` 等非在售状态时，统一返回 HTTP 200、`code: 40400`、`message: "商品不存在"`，不含 `data`，避免暴露商品或供给方信用资料。

供给方仍通过 `/supplier/products` 查看自己的商品，运营通过 `/admin/products` 审核；买家历史订单通过 `/orders/:order_no` 获取商品资料，不受公开可见性限制影响。

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

## GET /products/:id/availability · 可租性与报价 ✅ 登录（T-093）

只读计算：给出"现在按这组参数能否下单"以及与 `POST /orders` **完全相同**的报价（同一纯函数 `QuoteOrder` + 当前交易配置费率）。**不预留**任何库存或卡槽；结果仅代表查询时刻，下单时以订单事务内的分配为准（仍可能 40900）。

```
curl -H "Authorization: Bearer <token>" \
  "http://localhost:8080/api/v1/products/12/availability?pricing_mode=hourly&cards=4&duration=2"
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| cards | int | ✅ | 卡数（= 下单 `quantity`）。托管分卡商品必须在 `card_split_options` 内，否则 40001 |
| pricing_mode | string | — | 计费方式，省略取展示主价方式；未开放/已停用 40001 |
| duration | int | ✅² | 周期数，与 `POST /orders` 同校验：须 ≥ 该方式 `min_duration` 且 ≤ 单笔上限，否则 40001 |
| night_start_date | string | — | `YYYY-MM-DD`，仅包夜。非包夜传入、或格式错误 40001 |

² 买断（`perpetual`）可省略，按 1 计。

**限流**：每用户每分钟 30 次（Redis 固定窗口，key `market:availability:rl:{user_id}`），超出 42900「查询过于频繁, 每分钟最多 30 次, 请稍后再试」。Redis 未装配/不可用时返回 50000（不放开限流）。

**响应**（托管商品，可下单）：
```json
{"code":0,"message":"success","data":{
  "available":true,
  "reason":"",
  "quote":{"pricing_mode":"hourly","unit_price":1500,"quantity":4,"duration":2,
           "total_amount":12000,"platform_fee":780,"fee_rate":650},
  "lease_preview":{"start_rule":"on_activation",
                   "start_at":"2026-10-28T15:04:05+08:00","end_at":"2026-10-28T17:04:05+08:00"}
},"request_id":"..."}
```

**响应**（人工商品，库存不足）：
```json
{"code":0,"message":"success","data":{
  "available":false,
  "reason":"库存不足: 当前可租 6",
  "quote":{"pricing_mode":"hourly","unit_price":2300,"quantity":7,"duration":1,
           "total_amount":16100,"platform_fee":1046,"fee_rate":650},
  "lease_preview":{"start_rule":"on_buyer_confirm","start_at":null,"end_at":null}
},"request_id":"..."}
```

**响应**（包夜，S4 T-094；`night_start_date` 省略时取可订的第一晚）：
```json
{"code":0,"message":"success","data":{
  "available":true,
  "reason":"",
  "quote":{"pricing_mode":"nightly","unit_price":9000,"quantity":4,"duration":2,
           "total_amount":72000,"platform_fee":4680,"fee_rate":650},
  "lease_preview":{"start_rule":"night_window",
                   "start_at":"2026-11-03T22:00:00+08:00","end_at":"2026-11-05T06:00:00+08:00"}
},"request_id":"..."}
```
包夜不可订时 `available=false`、`reason`「所选夜晚暂无同机空闲卡(同一组卡须每晚都未被预订)」（报价照给）。

字段说明：
- `quote`：金额单位分。`unit_price` 分/卡/周期；`total_amount = unit_price × quantity × duration`；`platform_fee` 内含于总价，= `total_amount × fee_rate / 10000` 向下取整；`fee_rate` 为基点。与随后 `POST /orders` 的 `total_amount/platform_fee` 相同（前提是期间商品价格与交易配置未变）。`available=false` 时仍给报价。
- `lease_preview.start_rule`：`on_activation`＝托管商品，租期从开通自检通过起算（D-6，不计开通耗时），`start_at` 取查询时刻（Asia/Shanghai，秒级），`end_at` 按计费周期换算，仅为预估；买断 `end_at=null`。`on_buyer_confirm`＝人工商品，租期从买家确认签收起算，签收时刻未知，`start_at/end_at` 均为 `null`。`night_window`＝包夜：`start_at` = 首晚窗口开始（当晚订单为查询时刻），`end_at` = 末晚窗口结束（固定，不随开通耗时顺延）。
- `available=false` 时 `reason` 为可直接展示的中文原因，按以下优先级只给第一条：交易总开关关闭（「交易暂时关闭，请稍后再试」）→ 商品已售罄 → 供应方算力节点已全部离线 → 供给方资质已过期 → 人工：库存不足 → 托管：托管履约未启用 / 托管履约模块未启用 → 托管：「所选卡数暂无整机余量」（没有任何单台节点能凑齐 `cards` 张卡；判定与下单分配的候选规则一致：已验收 + 可调度 + 在线 + 空闲 ≥ cards，无磁盘配额的节点只能整机）。

**错误码**：40001 参数非法（cards 缺失/非正、卡数不在可选集合、计费方式未开放、时长不足、托管买断、面议商品、`night_start_date` 非法：格式错、非包夜传入、早于可订的第一晚、晚于今天起第 30 天、末晚超出今天起 60 天；包夜用于非夜间窗口/非托管/非分卡容器商品）；40400「商品不存在」（不存在或非 `active/sold_out`；`sold_out` 可查询，返回 `available=false`）；42900 限流；50000 内部错误。

---

## POST /supplier/products · 提交商品审核 ✅ supplier

```
curl -X POST http://localhost:8080/api/v1/supplier/products \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"gpu_model":"NVIDIA H100 SXM 80GB","card_count":64,"cpu_spec":"2× Intel Xeon 8480+","memory_spec":"2TB DDR5","storage_spec":"30TB NVMe","bandwidth_spec":"10Gbps","delivery_mode":"bare_metal","pricing_mode":"hourly","unit_price":3500,"available_hours":"全天 24h","stock":64,"min_order":1,"min_duration":1,"region":"北京","compliance_agreed":true,"compliance_version":"2026-09-06.1"}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| gpu_model | string | ✅ | GPU 型号 |
| card_count | int | ✅ | 可售总卡数 |
| delivery_mode | string | ✅ | bare_metal / container / vm / rack |
| fulfillment_mode | string | — | 履约模式：`manual`（缺省，供给方人工交付）/ `managed`（平台托管开通，v1.2，仅零租，见下方「托管商品」）；创建后不可修改 |
| pricing_mode | string | ✅¹ | 零租：hourly / daily / weekly / monthly / nightly；智算中心：monthly；买断：perpetual |
| unit_price | int | ✅¹ | 单价(分)/卡/周期，如 3500=¥35.00 |
| min_duration | int | — | 最短租期（周期数），默认 1 |
| prices | array | — | 仅零租。多计费方式：`[{"pricing_mode":"hourly","unit_price":3500,"min_duration":1,"enabled":true}, ...]`；`min_duration` 省略为 1，`enabled` 省略为 true |
| availability | string | — | fulltime（默认）/ night_window；night_window 仅零租 |
| card_split_options | int[] | — | T-092/T-093，仅托管容器零租（`card_rental` + `managed` + `container`）。每个值须属于平台集合 `{1,2,4,6,8}` 且 ≤ `card_count`；去重升序保存；省略取平台集合中 ≤ `card_count` 的全部值。其他商品传入返回 40001「可选卡数仅适用于平台托管的分卡容器(container)零租商品」 |
| night_start_min / night_end_min | int | — | 仅 night_window；默认 1320/480（22:00-08:00）；30 分钟对齐，窗口长 4-14 小时；`available_hours` 由服务端按窗口生成 |
| stock | int | ✅ | 可售余量；`managed` 商品忽略此值（库存由节点卡槽派生，写入 0） |
| region | string | ✅ | 地域 |
| compliance_agreed | bool | ✅ | 合规承诺（必须为 true） |
| compliance_version | string | ✅ | 当前规范版本：`2026-09-06.1`；发布为上架规范，下单为使用规范 |

¹ 零租传 `prices` 时顶层 `pricing_mode/unit_price/min_duration` 被忽略，并由服务端同步为展示主价；未传 `prices` 时（旧前端）按顶层字段生成一行价格。

**零租计费组合约束**（违反返回 `40001`）：每种方式最多一行、单价 > 0、`1 ≤ min_duration ≤` 该方式单笔上限（小时 87600 / 天 3650 / 周 520 / 月 120 / 晚 31）、至少启用一种、`单价 × max(stock, card_count) × 单笔上限 ≤ 100 亿元`；`nightly`（包夜）只能单独开放，且要求 `availability=night_window` + `fulfillment_mode=managed`；`night_window` 商品只能卖包夜，且只能 `delivery_mode=container`（整机 `bare_metal` 返回 40001「夜间窗口(包夜)商品仅支持 container(分卡容器)交付, 整机(bare_metal)不能包夜出租」）；`fulltime` 商品不能传夜间时段。非零租商品不能传 `prices`、`managed` 或夜间窗口。

**成功响应**（`PUT /supplier/products/:id` 重提同样返回）：
```json
{"code":0,"data":{"id":12,"warnings":[{"code":"daily_above_24_hourly","message":"日价高于 24 小时价"}]}}
```
`warnings` 为价格倒挂提醒，只提醒不阻断，无提醒时为 `[]`。只比较已启用价格：`daily_above_24_hourly`（日价 > 24×时价）、`weekly_above_7_daily`（周价 > 7×日价）、`monthly_above_30_daily`（月价 > 30×日价）、`monthly_above_4_weekly`（未设日价时，月价 > 4×周价）。重提时未再提交的计费方式保留为 `enabled=false`（不删除），存量订单续租据此返回 `40900`。

商品对象（`GET /products`、`GET /products/:id`、`GET /supplier/products`、`/admin/products`）均带 `fulfillment_mode`。`managed` 商品的初始状态是 `draft`（不是 `pending`），见下方「托管商品」。

---

## GET /products/:id/night-calendar · 包夜夜间日历 ✅ 登录（S4 T-094）

只读：自 `from` 起连续 `days` 晚，每晚**单台节点**最多可订的同机卡数（已按商品可选卡数 / 整机规则取整）。不加锁快照，下单仍以预订唯一键为准（并发下可能 40900）。仅 `availability=night_window` 的托管分卡容器商品可查。

```
curl -H "Authorization: Bearer <token>" \
  "http://localhost:8080/api/v1/products/12/night-calendar?cards=4&from=2026-11-03&days=7"
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| cards | int | — | 计划卡数；> 0 时只统计能一次容纳该卡数的节点，`max_cards` 不足 `cards` 的晚记 0。省略或 0 = 不限 |
| from | string | — | `YYYY-MM-DD`（Asia/Shanghai），省略取可订的第一晚；须在昨天 ~ 今天起 60 天内，否则 40001 |
| days | int | — | 1-31，缺省 14；超出 40001。上限 31 = 今天起第 0~30 天，省略 `from` 查满 31 晚即可覆盖全部可选起始夜晚 |

**夜晚标识**：`night_date` = 窗口**开始**那天的日期。跨零点窗口（如 22:00-06:00）的 11-03 晚 = 11-03 22:00 至 11-04 06:00；11-04 01:00 仍属 11-03 晚。

**响应**
```json
{"code":0,"message":"success","data":[
  {"night_date":"2026-11-03","max_cards":0},
  {"night_date":"2026-11-04","max_cards":4},
  {"night_date":"2026-11-05","max_cards":8}
],"request_id":"..."}
```
- 已截止（当晚窗口开始满 2 小时，Q-36）或超出可订范围（今天起 60 天）的晚 `max_cards=0`。
- **起始夜晚**（`night_start_date`）只能选今天起第 30 天及以前的晚；日历中更晚的晚 `max_cards` 照常返回（可作为多晚订单的后续晚），前端选择起始夜晚时须自行限制到今天起第 30 天。
- 同一订单所有晚使用**同一组卡**（容器跨晚复用、数据不丢），故连续 N 晚能否下单不等于每晚 `max_cards` 的最小值：同机同卡须每晚都空闲。连续多晚以 `GET /products/:id/availability?pricing_mode=nightly&night_start_date=&duration=N&cards=` 为准。

**限流**：与 availability 共用每用户每分钟 30 次（同一 Redis key），超出 42900；Redis 未装配 50000。

**错误码**：40001（参数非法；商品非夜间窗口托管分卡容器）；40400「商品不存在」；42900；50000（托管履约/包夜租约钩子未接入等内部错误）。

---

## POST /orders · 下单 ✅ buyer

```
curl -X POST http://localhost:8080/api/v1/orders \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"product_id":1,"quantity":8,"duration":720,"compliance_agreed":true,"compliance_version":"2026-09-06.1"}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| product_id | int | ✅ | 商品 ID |
| quantity | int | ✅ | 卡数 |
| duration | int | ✅ | 租期，单位为所选计费方式的周期（小时/天/周/月/晚）；买断忽略并按 1 |
| pricing_mode | string | — | 所选计费方式；省略取商品展示主价方式（兼容旧前端）。必须是该商品已启用的价格行，否则 `40001`「该商品未开放xx计费」 |
| night_start_date | string | 包夜必填 | `YYYY-MM-DD`，首晚（窗口开始那天的日期）。`duration` = 晚数（1-31）。校验见「包夜订单（S4）」 |
| compliance_agreed | bool | ✅ | 合规承诺 |
| compliance_version | string | ✅ | 当前规范版本：`2026-09-06.1`；发布为上架规范，下单为使用规范 |

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

金额 = 所选方式的 `unit_price × quantity × duration`，最短/最长租期按该方式校验。订单写入计费方式快照 `orders.pricing_mode`，此后租期换算、续租报价、发票只认快照，不随商品改价或展示主价变化漂移。v1.2 前的存量订单由迁移 028 按迁移时的计费方式回填快照（续租子订单取其续租快照，其余取当时商品方式），与新订单同样只认快照；快照缺失属数据异常，交付、签收、续租报价返回 `50000`「订单缺少计费方式快照…」，不回退商品当前展示方式。托管商品（`fulfillment_mode=managed`）下单的错误码见下方「托管商品」「托管订单」两节（`fleet.enabled=false` 为 40001，租约钩子未接入为 50000）；包夜（`nightly`）见下方「包夜订单（S4）」。

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

`pricing_mode` 为订单的计费方式快照（v1.2 前存量订单由 028 回填），不回退商品当前展示方式；快照缺失（数据异常）时为空串。供给方订单列表、可开票订单列表同理。

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
    "fulfillment_mode":"managed","pricing_mode":"monthly",
    "night_start_date":null,"night_start_min":null,"night_end_min":null,
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

`order.fulfillment_mode`（`managed`/`manual`，旧订单无值按 `manual`）、`order.pricing_mode`（订单计费方式快照，缺失为空串）、`order.night_start_date`（包夜首晚日期，序列化为该日 00:00 的时间串，如 `"2026-11-03T00:00:00+08:00"`）、`order.night_start_min`/`order.night_end_min`（包夜窗口快照，自 00:00 起的分钟数）与订单列表行同名同义；非包夜订单三个 `night_*` 字段为 `null`。包夜订单的 `duration` 为购买晚数，每晚明细见 managed-rental-api.md「GET /orders/:id/nights」。

`product.pricing_mode` 与 `product.min_duration` 取**订单**的计费方式快照及该方式当前的最短租期（快照缺失时 `pricing_mode` 为空串、`can_renew=false`，不回退商品展示方式），用于续租表单；其余 `product` 字段为商品当前资料。`delivery` 和 `supplier.credit` 没有记录时为 `null`。详情不返回 `buyer_id`、`supplier_id`、密文、`access_key` 或凭证明文；凭证仍通过独立的 access-credential 接口查看。当前数据库没有订单事件表和商品快照，因此此接口不伪造区块链时间线，`product` / `supplier` 是当前关联资料。

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
- 订单缺少计费方式快照（数据异常，须平台核对）：`code=50000`，不按商品当前方式换算租期

## GET /orders/:id/renewal-quote · 续租报价 ✅ buyer

`:id` 为原订单号，`duration` 查询参数为正整数计费周期数。仅订单本人且 buyer/KYC 有效可用；子续租订单不能再次续租。交易关闭、商品下架/离线/面议/买断、未知库存占用、冻结/退款或存在未处理续租时拒绝。

```json
{"parent_order_no":"ORD20260908000000abcdef","mode":"extend","quantity":2,"duration":2,"pricing_mode":"hourly","min_duration":1,"max_duration":87600,"unit_price":3000,"total_amount":12000,"platform_fee":780,"fee_rate":650,"lease_end_at":"2026-09-08T12:00:00+08:00","renewed_until":"2026-09-08T14:00:00+08:00"}
```

金额均为分，按**原订单的计费方式快照**（存量订单由 028 回填；快照缺失返回 `50000`，不回退商品当前展示方式）取该方式当前的价格行与平台费率报价，原订单金额不变；`min_duration/max_duration` 也取该方式。供给方已停用该计费方式时返回 `40900`「该计费方式已停用，请重新下单」，不会换算成其他方式；此时详情 `can_renew=false`。托管订单的 extend/restart 口径见下文「托管订单续租（S2 T-088）」；包夜订单返回 `50000`。`extend` 要求当前租期与凭证有效，`renewed_until` 从原结束时间增加周期；商品售罄不影响已有资源续期。`restart` 表示租期已结束，按真实可用库存重新预留，`renewed_until=null`，重新交付和签收后才确定新租期。日/周/月按后端日历计算。

## POST /orders/:id/renew · 创建续租订单 ✅ buyer

```json
{"duration":2,"expected_pricing_mode":"hourly","request_id":"cdd36d72-79ab-4a0a-9d2d-56a25f89c877","compliance_agreed":true,"compliance_version":"2026-09-06.1","expected_lease_end_at":"2026-09-08T12:00:00+08:00","expected_renewed_until":"2026-09-08T14:00:00+08:00","expected_total_amount":12000,"expected_platform_fee":780}
```

- 同一次提交重试保持 UUID `request_id` 和报价字段不变；同一原订单、同一请求返回同一子订单。修改参数复用请求号返回 `40900`。
- 服务器锁定原订单并重新报价；计费方式、租期或金额变化返回 `40900`，前端刷新报价后重新显式同意。每个原订单最多一笔待支付续租。
- 成功返回 `{order_no,total_amount,platform_fee}`，订单号以 `REN` 开头。合规同意记录与订单在同一事务落库，`reference` 是子订单号。
- 创建不延长租期。到期前续租不重复扣库存，支付期限为 15 分钟与原租期结束时间的较早者。到期后续租只预留尚未持有的卡；取消、超时只释放这笔实际占用。
- 验签成功且金额、支付记录、订单状态与时限匹配后，在同一事务记录支付、分账金额及履约变更。重复回调不重复延长/划转库存，运营不能通过改子订单状态绕过支付。
- 到期前支付更新原订单结束时间与凭证有效期；到期后支付将预留库存划归原订单、原订单转为 `paid` 并吊销旧凭证，供给方在原订单重新交付，买家重新签收。子订单 `completed` 表示续租款项已应用，实际资源交付仍看原订单。
- 续租子订单（`REN`）同样写入计费方式快照。
- 原订单价格/数量/原购买周期不改写。重新交付使用子订单的周期与计费方式快照；原交付存证保留，新交付关联本次续租订单。

原订单详情及供给方订单列表新增可空 `current_lease`（`order_no,duration,pricing_mode`），指向最近一次已付款且未退款的到期后续租。重新交付按此周期快照执行；原订单的金额和购买周期保持历史值。此时退款须从本次续租子订单申请，原订单 `can_refund=false`，服务端也拒绝绕过。

订单详情新增 `order.parent_order_no`、`pending_renewal_order_no`、`product.min_duration` 及可空 `renewal`（`parent_order_no,mode,pricing_mode,lease_end_at,renewed_until,applied_at,confirmed_at`）。前端仅在 `actions.can_renew=true` 时提供新续租，待支付子订单通过 `pending_renewal_order_no` 恢复处理。

## POST /orders/:id/cancel · 取消待支付订单 ✅ buyer

仅订单本人可取消 `pending_payment`，重复取消成功且不重复归还库存。已支付订单不可取消。原订单冻结、关闭或申请退款时，待支付续租在同一事务取消，后续回调不会重新开通。

## POST /orders/:id/refund · 申请退款 ✅ buyer

普通订单沿现有退款流程。续租只允许尚未使用、且未被后续租期覆盖的最后一段续期；到期后重新交付仅在重新签收前可申请。申请转 `refunding`，运营确认完成时回退未使用的租期/预留，重复处理不重复归还库存。若处理时已开始使用或租期已变化，拒绝自动回退，须平台核对；已使用部分的按比例退款与真实渠道到账仍属于后端 Issue #12。

---

## POST /orders/:id/deliver · 回填凭证 ✅ supplier
```json
{"ip_address":"10.0.1.128","ssh_port":22,"username":"root","credential_note":"密码已私信"}
```
交付信息与访问凭证均使用 AES-256-GCM 加密存储；未配置 `security.credential_key` 时明确失败，不降级为明文。凭证到期时间按订单计费方式快照与购买周期换算；快照缺失时返回 `50000`，不交付。

---

## GET /supplier-applications · 我的供给方入驻申请 ✅ authenticated
## POST /supplier-applications · 提交供给方入驻申请 ✅ authenticated + KYC

供给方认证独立于机房登记。必填企业名称、统一社会信用代码、法定代表人及证件号、营业执照、业务联系人、开户银行、账户名称和银行账号；不再要求或接收机房地址、IDC 确认、供配电和制冷说明。历史申请中的机房字段保留可读，不影响审核。

发布入口按模块区分：`/console/supplier/products/new` 仅零租与买断，`/console/supplier/centers/new` 仅成熟算力中心，`/console/supplier/colocation/new` 仅空心机房。共用服务端商品接口与类型校验；编辑重提保留原商品类型。运营商品管理的待审核条目链接至 `/admin/reviews?tab=products`，复用通过／驳回流程。


POST 使用 `multipart/form-data`，`business_license` 必须为 PDF/JPG/PNG 且不超过 5MB；完整字段和文件内容写入 MySQL。审核通过后，服务端在同一事务中将申请置为 `verified`、授予 `supplier` 角色并写入审计日志。用户不能通过通用角色接口绕过审核。

---

## GET /supplier/products · 我的商品 ✅ supplier
## GET /supplier/orders · 供给方订单 ✅ supplier

每行为订单对象（与买家订单同字段）+ `gpu_model`、`product_type`、`pricing_mode`（订单快照）、可空 `current_lease`（见下文「到期后续租」），以及：

- `fulfillment_mode`：`manual` / `managed`（下单时商品履约模式快照）。
- `settle_ready_at`（v1.2，可空时间戳）：仅供给方视角输出。`managed` 订单在租期结束（清除完成、停机补偿结算落地）后写入，非空才进入分账；为 `null` 时前端展示「平台托管 · 租期结束后分账」。`manual` 订单恒为 `null`。买家订单接口不输出该字段。

## GET /supplier/qualifications · 我的资质 ✅ supplier
## POST /supplier/qualifications · 提交资质 ✅ supplier


提交使用 JSON（不是 FormData）：

```json
{"qual_type":"idc_license","cert_name":"IDC 经营许可证","cert_number":"示例编号","cert_url":"https://example.com/license.pdf","expires_at":"2027-09-30"}
```

- `expires_at` 可省略或为 `null`；提供时须为 `YYYY-MM-DD`，不能早于数据库当天日期。历史空值保留，不推测有效期。空字符串兼容为未提供。
- GET 保留 `expires_at` 时间戳，新增 `superseded`（已有较新有效同类型已审核证照，前端展示“已更新”）及可空整数 `expires_in_days`（数据库日历日期差）。审核状态仍为 `pending / verified / rejected / expired`；前端在 `verified` 且剩余 0–30 天时展示“即将到期”，负数展示“已过期”。到期当天仍有效。
- 启动时及每 5 分钟扫描已审核资质。30 天内补发一次预警；过期后一次性更新为 `expired` 并冻结该供给方 `active / sold_out` 商品，不改动已有订单。提交商品、审核上架、新下单和续租报价/创建也检查过期资质，避免等待扫描时继续交易。
- 更新通过提交同一 `qual_type` 的新证照并经审核完成；较新的有效已审核证照替代同类型旧证照。待审核、驳回或其他类型证照不能解除原类型过期限制。过期申请不能通过审核。旧商品不自动解冻；新证照审核通过后可重新发布商品。
- 通知发给资质 `user_id`，类型为 `system`，链接 `/console/supplier/qualifications#qualification-{id}`，沿用通知查询/已读/删除接口。真实短信和邮件另见后端 Issue #16。
- 状态、商品冻结、通知和 `audit_logs` 在同一事务提交；锁定资质后按 `target_type=supplier_qualification`、资质 ID、事件 `qualification_expiring / qualification_expired` 与有效期去重。`after_value` 保存有效期日期，系统操作者为空。修改后的有效期重新判断，删除通知不重发，失败回滚并在下一轮重试。
- 运营可通过 `GET /admin/audit-logs` 查询事件证据；旧证照已被有效新证照替代时，只记录到期状态，不再提醒或冻结。无需新增数据库迁移。

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

## 协议版本要求（2026-09-06）

商品发布 `POST /supplier/products`、驳回重提 `PUT /supplier/products/:id` 和下单 `POST /orders` 必须同时传入 `compliance_agreed=true` 与 `compliance_version="2026-09-06.1"`。发布/重提对应《算力资源上架规范》，下单对应《算力资源使用规范》。缺失、旧版本或未同意返回 `40001`。同意记录与商品或订单、库存变更原子提交；失败不留下新同意记录，记录失败也不保留业务变更。

续租必须携带当次显式同意与当前版本，并为子订单追加同意记录。面议询价不伪造协议同意。参见 [协议页面与同意契约](legal-consent-api.md)。

## 2026-09-08 第一批交易控制契约

- `GET /trading-config` 无需登录，返回 `{trading_enabled:boolean,fee_rate:number}`（基点），禁止缓存。读取失败/配置不完整时 HTTP 503、code 50000。
- `POST /orders` 在订单事务内锁定当前交易配置；关闭返回 40900。费率为整数基点，费用向下取整到分，内含于总价；已创建订单使用保存的 `platform_fee`，支付分账不重新读取费率。
- `GET /products`、`GET /products/:id` 的商品对象均包含 `health=unknown|healthy|degraded|offline`。offline 不可下单；unknown 不显示健康徽章，其他状态不改变既有准入规则。
- 库存按订单实际 `stock_reserved` 归还并清零，与终态流转同事务；零占用订单不增加库存。历史 NULL 占用必须先核对，释放失败保持原状态，禁止猜测回填。该内部字段不对外返回。
- 增量迁移、历史核对与应用回退见 [发布说明](trade-controls-release.md)。

## 托管商品（S1，`fulfillment_mode=managed`）

v1.2 S1（T-076~T-081）。`fulfillment_mode` 缺省或 `manual` 的商品所有接口行为不变。设计见 docs/25 §25.4、§25.12；节点接入接口见 [managed-rental-api.md](managed-rental-api.md)。

**发布 `POST /supplier/products`**
- 仅零租（`product_type=card_rental`）且 `delivery_mode` 为 `container`（分卡容器）或 `bare_metal`（整机）可选 `managed`；否则 40001「平台托管仅支持零租商品」/「平台托管仅支持 container(分卡容器)或 bare_metal(整机)交付」；`fulfillment_mode` 取其他值 40001「履约模式非法…」。
- `stock` 被忽略并写 0；**创建即为 `draft`**（人工商品为 `pending`），不进审核队列、不在市场展示。供给方随后在该商品下添加 ssh 托管节点并完成探活（managed-rental-api.md），再提交审核。
- `fleet.enabled=false` 时仍可创建 `managed` 草稿（便于提前准备），但不能提交审核与下单（见下）。

**提交审核 `PUT /supplier/products/:id`**（与驳回后重提同一接口、同一完整请求体；仅 `draft` / `offline` 可提交，成功 → `pending`）
| 情形 | code | message |
|---|---|---|
| `fleet.enabled=false` | 40001 | 托管履约未启用(fleet.enabled=false): 托管商品暂不可提交审核或下单 |
| 该商品下没有 ssh 托管节点 | 40001 | 托管商品提交审核前须至少接入一个 ssh 托管节点并通过探活 |
| 没有一台「`verified` + 最近一次通过的完整探活 + 每张卡型号与商品 `gpu_model` 一致（P5 归一比对）」的节点 | 40001 | 托管商品暂不可提交审核: 节点 X: 核验状态 unverified; 节点 Y: GPU0 实测 … 与商品型号 … 不一致; …（逐节点列出原因） |
| 请求体 `fulfillment_mode` 与创建时不同 | 40001 | 履约模式创建后不可修改, 如需切换请新建商品 |
| 商品不在 `draft` / `offline`（如已 `pending` / `active`） | 40900 | product review conflict |
| 修改了 `availability` / `night_start_min` / `night_end_min` / `delivery_mode`，而该商品仍有未结束的租约（state 不在 purged/failed/cancelled）或昨夜及以后的包夜预订 | 40900 | 该商品仍有未结束的租约或包夜预订, 不能修改可用时段、夜间窗口或交付方式; 请待全部租约结束后再修改, 或新建商品 |

只改价格、卡数选项等其他字段不受上表最后一行限制。这一行保证同一张卡不会同时出现「按时/天/周/月在租」与「包夜预订」：节点只挂一个商品，`night_window` 商品只能包夜下单，全天商品不能包夜下单，所以两种占用在结构上互斥；唯一能打破互斥的是在用期间修改售卖时段，现在已被拒绝。

**审核通过 `POST /admin/audits/products/:id/approve`**：`managed` 商品按当前节点状态再校验一次（审核期间节点可能已探活失败），不满足返回上表同样的 40001；通过后 `pending → active` 并立即按空闲卡槽重算库存（无空闲卡则 `sold_out`，有卡后自动回到 `active`）。

**其他**
- `managed` 商品库存由节点卡槽派生（docs/25 §25.6），人工盘点（`POST /supplier/resource-syncs/passive`、`POST /admin/resource-syncs/active`）返回 40001「托管商品库存由平台按卡槽自动计算」。
- `draft` 商品对公开接口不可见：`GET /products/:id` 40400，`POST /orders` 40900「product not available」。

## 托管订单（S2，`fulfillment_mode=managed`）

v1.2 S2（T-082~T-084，T-092 下单分配子集）。只作用于 `products.fulfillment_mode='managed'` 的商品与订单；人工订单（`manual`）的所有接口行为不变。设计见 docs/25 §25.5、docs/26 §26.5；租约与连接信息接口见 [managed-rental-api.md](managed-rental-api.md)「买家连接」。

**下单 `POST /orders`**（请求/响应结构不变；买家准入、协议版本、商品 `active`、供给方资质、节点健康等通用校验在前，与人工订单相同）
- `fleet.enabled=false`：40001「托管履约未启用(fleet.enabled=false): 托管商品暂不可提交审核或下单」。
- fleet 已启用但租约钩子未接入：50000「托管履约模块未启用: 租约开通(docs/25 S2)尚未接入, 托管商品暂不可下单」。
- 买断计费（`pricing_mode=perpetual`）的托管商品：40001「托管商品不支持买断计费」。
- 不扣 `products.stock`（托管库存由卡槽派生），订单 `stock_reserved=0`；同一事务内建租约（`reserved`）并在**同一台节点**上预留 `quantity` 张卡槽（docs/26 §26.5.2），随后重算派生库存。事务使用 READ COMMITTED。
- 没有一台节点能凑齐所需卡数：40900「所选卡数暂无整机余量」（总空闲卡数可能大于所需，但分散在不同节点）。
- 整机交付（`delivery_mode=bare_metal`）的托管商品（T-089 起可下单）：`card_count` 须等于某台节点的全部卡数，只分配全部卡空闲的节点；否则同上 40900「所选卡数暂无整机余量」。整机无宽限期、到期即清除，节点转待重装，见 managed-rental-api.md「整机（bare_metal）租约」。

**状态流转**
| 订单状态 | 托管口径 |
|---|---|
| `pending_payment` | 卡槽已预留（租约 `reserved`）；超时/取消释放卡槽与租约（租约 → `cancelled`） |
| `paid` | 收款确认同一事务内租约 → `pending` 并入队开通任务 |
| `provisioning` | 开通任务开始执行（无需供给方回填凭证，**不走** `POST /orders/:id/deliver` 与 `POST /orders/:id/confirm`） |
| `active` | 容器创建且经访问网关系统自检通过。`lease_start_at` = 自检通过时刻，`lease_end_at` 按计费周期换算；交付记录直接为 `delivered`（无签收环节）并写 `selftest_passed_at` |
| `cancelled` | 开通彻底失败（换节点后仍失败）：`lease_start_at` 保持 `null`，吊销凭证，由支付侧自动全额原路退款（见 payment-api.md「托管订单（S2）」） |

- 交付信息（`GET /orders/:id/access-credential`）：`ip_address` 为网关地址（`accessgw.public_host`）、`ssh_port` 为网关端口、`username` 为租约号（登录名）；**不含节点 IP/端口**。访问凭证 `access_value` 即网关密码登录口令，沿用 reveal 接口查看。推荐改用 SSH 公钥（`POST /user/ssh-keys`）。
- 订单进入 `cancelled/refunded/completed` 时同一事务内释放卡槽并迁移租约（`releaseHoldTx`，已释放幂等）；人工订单仍按 `stock_reserved` 归还库存。
- 释放只发生一次（T-093）：关单定时任务、买家取消、运营关单（`PATCH /admin/orders/:id/status` → `cancelled`）、退款完成都先锁订单行再做带前置状态的守卫 UPDATE，只有真正迁移了状态的那一路释放卡槽；其余返回 40900（订单已不在可迁移状态）或空操作，库存按卡槽真实空闲重算，不做加减。运营关单改为同样的守卫迁移（原为无条件写状态）。托管订单误入人工 `stock_reserved` 归还路径返回 50000，不会改 `products.stock`。
- **库存上报**（T-093）：托管商品的库存由平台按卡槽派生，节点 agent / 供给方对托管商品的资源同步（被动上报与主动同步）返回 40001「托管商品库存由平台按卡槽自动计算」，不写库存与快照。

### 托管订单续租（S2 T-088）✅

托管订单沿用 `GET /orders/:id/renewal-quote` 与 `POST /orders/:id/renew`，**请求、响应结构与人工订单完全相同**（见上文两节；报价按原订单计费方式快照）。不扣 `products.stock`，余量按卡槽判定；租约侧动作与订单在同一事务内完成。`mode` 由原订单 `lease_end_at` 是否已过决定：

| `mode` | 条件 | 报价 / 下单校验 | 支付落账 |
|---|---|---|---|
| `extend` | 原订单 `active`、`lease_end_at` 未到，且租约 `active/parked`、`access_until` 未到 | 租约不满足 → 40900 `ErrRenewalConflict` | 原订单 `lease_end_at` 与交付 `access_expires_at` 改为 `renewed_until`；租约 `access_until=renewed_until`（同事务刷新撤销游标，网关按新时刻放行）。凭证、会话、容器不变 |
| `restart` | 原订单 `lease_end_at` 已过（正常为 `completed`；到期扫描尚未处理的 `active` 订单因租约仍在租返回 40900） | 租约 `grace`：只能回**原节点**（数据卷在原节点），原节点须仍可调度且能凑齐卡数（本租约仍持有的卡 → 容器原卡 → 同机其他空闲卡），下单时同事务预留；租约 `purged`：按商品全部节点重新分配（同新下单规则），下单时预留；租约 `purging`：40900 | 原订单 → `paid`（`lease_start_at/lease_end_at` 清空，旧凭证吊销）；租约 → `pending` 并自动入队开通：宽限期内为原节点恢复（原卡齐全直接 `docker start`，换卡则提交镜像后重建，数据卷沿用），宽限期后为全新环境。开通自检通过后原订单 → `active`，租期按**本次续租子订单**的周期与快照从自检通过时刻重新起算，登录名（租约号）不变 |

`renewed_until`：`extend` 为原结束时间加续租周期；`restart` 为 `null`。宽限期内 `restart` 报价/下单成功不代表保留期延长：未支付的续租单在原 `grace_until` 之后会暂缓清除，直到续租单超时关单。

**错误**（在通用续租错误之外）
| 场景 | code | message |
|---|---|---|
| extend：租约已不在租或 `access_until` 已过（含冻结中） | 40900 | 续租条件已变化或已有待支付续租订单，请刷新订单 |
| restart：宽限期内原节点不可调度或凑不齐卡数 | 40900 | 原节点暂无足够空闲卡或暂不可调度, 宽限期内只能在原节点恢复(数据卷在原节点); 可稍后再试或重新下单 |
| restart：宽限期后没有单台节点能凑齐卡数 | 40900 | 所选卡数暂无整机余量 |
| restart：租约清除中（`purging`） | 40900 | 租约数据清除中, 请稍后再续租(清除完成后按全新环境开通) |
| 包夜（`nightly`）订单（报价与下单均在其他校验之前拒绝） | 50000 | 包夜订单暂不支持续租，如需继续使用请在包夜日历中选择日期重新下单 |
| 租约续租钩子未接入（服务端装配缺失） | 50000 | 托管订单续租尚未接入(docs/25 §25.7.2, T-088), 暂不可续租 |

**支付与失败善后**
- 收款时再次校验（extend：租约仍在租未到期；restart：预留卡槽仍完整）；不满足时不落账，款项按「订单已关闭或已失效, 款项原路退回」排队原路退款。
- 未支付的 restart 续租单关闭/取消：退回下单时预留的卡槽（宽限期内预留的卡回到 `draining` 并确保容器停止，停止后释放；宽限期后直接释放）。
- 续租退款（续租尚未开始使用）：extend 把租约 `access_until` 回退为原 `lease_end_at`；restart 开通完成前退款，宽限期内恢复的回到 `grace`（数据保留到原 `grace_until`），宽限期后的全新开通直接清除。续租已开始使用后退款 40900「租约 … 已…, 续租已开始使用, 请联系平台处理退款」。
- restart 重新开通彻底失败：续租子订单 → `cancelled` 并自动全额原路退款，原订单回到 `completed`（恢复续租前租期）；宽限期内恢复的租约回到 `grace`，宽限期后的全新开通清除租约。买家收到「续租重新开通失败, 续租款将全额退回」。

### 托管订单冻结与解冻（S2 T-088）✅

**冻结**沿用 `PATCH /admin/orders/:id/status`（`status:"frozen"`）与 `POST /admin/risk/alerts/:id/freeze`（admin-api.md），挖矿特征扫描命中 stratum / 矿池地址时自动走同一路径。托管订单在冻结事务内额外：租约 `active/parked → suspended`、入队断开现有 SSH 会话、网关拒绝新登录；**容器不停、GPU 不释放**（保留核查现场）。冻结期间租期到期：停容器、释放 GPU、保留容器与数据卷，租约保持 `suspended`，不进宽限计时。详见 managed-rental-api.md「租约生命周期 · 冻结」。人工订单的冻结口径不变（冻结后只能关闭/退款）。

#### POST /admin/orders/:id/unfreeze · 运营解冻托管订单 ✅ operator/admin

`:id` 为订单 id 或订单号。请求体可省略；省略等价于 `{"wrong_freeze":false}`。

```json
{"wrong_freeze":true}
```

```json
{"code":0,"message":"success","data":{"order_no":"ORD20261101103000123abc","status":"active","wrong_freeze":true},"request_id":"..."}
```

订单回到哪个状态由租约实况决定（不是简单恢复冻结前状态）：

| 解冻时租约状态 | 订单 → `status` | 其他效果 |
|---|---|---|
| `reserved` | `pending_payment` | 按原支付期限继续付款 |
| `pending` / `provisioning` | `paid` | 租约回到 `pending` 并重新入队开通（已有待领取的开通任务时不重复） |
| `active/parked/suspended` 且 `access_until` 未到 | `active` | 租约 `suspended → active`；冻结时吊销的访问凭证恢复（凭证值不变，有效期仍为原值） |
| `active/parked/suspended` 且 `access_until` 已过 | `completed` | 租约 → `grace`，**宽限期自解冻时刻起算**，可 restart 续租 |
| `grace` / `purging` / `purged` | `completed` | 租约侧收尾照常 |

- 任何分支都刷新撤销游标（网关丢弃冻结期间的缓存结果）；写审计 `action=unfreeze_order`、`target_type=order`、`before_value="frozen"`、`after_value={"status":"<目标状态>","wrong_freeze":<bool>}`；买家站内信「订单已解冻」。
- `wrong_freeze=true`：核查结论为误冻结。若冻结时租约在租（有 `suspended_at`），提交后把 `[suspended_at, 解冻时刻)` 交给停机认定计入平台责任停机（T-105 接入点 `LeaseHooks.WrongFreeze`；未接入时只记服务日志，不影响解冻结果）。

**错误**
| 场景 | code | message |
|---|---|---|
| `:id` 既不是正整数也不是合法订单号 / 请求体不是合法 JSON | 40001 | 订单编号无效 / 绑定错误 |
| 续租子订单 | 40001 | 续租单不支持单独解冻, 请处理其原订单 |
| 人工订单 | 40001 | 仅托管订单支持解冻; 人工订单冻结后请关闭或退款 |
| 订单不存在 | 40400 | order not found / 订单不存在 |
| 订单不是 `frozen` | 40900 | 订单当前状态为 X, 不是冻结状态 |
| 并发下订单状态已变化 | 40900 | 订单状态已变化, 请刷新后重试 |
| 开通任务仍在执行 | 40900 | 该订单的开通任务仍在执行(将因订单冻结自行放弃), 请稍后再解冻 |
| 租约 `failed/cancelled` | 40900 | 租约 X 已failed, 订单无法恢复, 请关闭订单或退款 |
| 无运营身份 | 40300 | 无权操作订单 |
| 托管租约钩子未接入（`fleet.enabled=false` 时即如此） | 50000 | 托管履约模块未启用: 租约开通(docs/25 S2)尚未接入, 托管商品暂不可下单 |

### 托管订单停机与补偿（S2 T-105）✅

停机的认定规则、原因与责任方见 managed-rental-api.md「停机认定与补偿」；运营检索见同文件「运营接口 · GET /admin/lease-outages」。两个接口都只对托管订单有效，`:id` 为**原订单号**（续租子订单的停机也挂在原订单的租约下）。归属判定顺序与 `GET /orders/:id/access` 相同：40400 订单不存在 → 40300 非本人订单 → 40400 人工交付订单 → 40400 订单尚无租约。

#### GET /orders/:id/outages · 停机记录与补偿 ✅ buyer（本人）

```json
{"code":0,"message":"success","data":{
  "order_no":"ORD20261101103000123abc","lease_no":"l-4g7bq4xc","lease_state":"active",
  "access_until":"2026-11-01T12:10:00+08:00",
  "remedy":"extend","refund_requested_at":null,
  "can_change_remedy":true,"refund_eligible":false,"refund_available":false,
  "term_minutes":120,"term_outage_minutes":10,"longest_outage_minutes":10,
  "refund_ratio":0.1,"refund_single_hours":2,
  "extended_minutes":10,"extended_nights":null,"pending_refund_amount":0,
  "items":[{"id":31,"cause":"node_offline","liable":"supplier",
    "started_at":"2026-11-01T10:20:00+08:00","ended_at":"2026-11-01T10:30:00+08:00","ongoing":false,
    "minutes":10,"counted":true,"remedy":"extend","remedy_applied_at":"2026-11-01T10:30:02+08:00","remedy_amount":null}]},
 "request_id":"..."}
```

| 字段 | 说明 |
|---|---|
| `remedy` | 当前补偿方式 `extend`（默认，顺延）/ `refund`（按比例退费） |
| `refund_requested_at` | 买家申请过退费的时间（退费通道未就绪时只记录意向，补偿仍按顺延执行） |
| `can_change_remedy` | 租约在租（`active/parked/suspended`）且 `access_until` 未到 |
| `refund_eligible` | 达到退费门槛（Q-47）：**当前所属订单**（原订单或已生效的续租单）期内停机累计 ≥ 租期 × `refund_ratio`，或单次停机 ≥ `refund_single_hours` 小时；进行中的停机按已持续时长计入 |
| `refund_available` | 退费通道已就绪（T-106 退费实现已接入且 `fleet.outage.yeepay_partial_refund_confirmed=true`）。开关默认 `false`；易宝能力已于 2026-11-10 确认，启用 fleet 并在沙箱验证部分退款后由运维置 `true` |
| `term_minutes` / `term_outage_minutes` / `longest_outage_minutes` | 门槛计算口径：当前所属订单租期分钟数、期内计入停机分钟、期内最长一段 |
| `extended_minutes` | 已以顺延方式补偿的停机总分钟数（时/日/周/月订单即租期后移的分钟数；包夜订单是被补偿的停机分钟，实际补偿为补加整晚，见 `extended_nights`） |
| `extended_nights` | 包夜订单已补加的晚数（排在原购买最后一晚之后的夜晚，含已过去的）；非包夜订单为 `null` |
| `pending_refund_amount` | 待退费金额合计（分），`remedy=refund` 且尚未退款成功的停机段（含已提交易宝处理中） |
| `items[]` | 按开始时间倒序。`ongoing=true` 为进行中（`ended_at=null`，`minutes` 为已持续分钟）；`counted=false` 为不足 `fleet.outage.min_minutes`（默认 5）或与其他段完全重叠，不补偿；`counted=null` 为尚未结算 |
| `items[].remedy` | `extend` 已顺延（`remedy_applied_at` 为顺延时刻）/ `refund` 待退费（`remedy_amount` 为金额，分；`remedy_applied_at` 为空表示尚未退款） |

`items[]` 不返回认定原文与责任金额（运营接口可见）。

#### PUT /orders/:id/outage-remedy · 选择补偿方式 ✅ buyer（本人）

```json
{"remedy":"refund"}
```

成功返回与 `GET /orders/:id/outages` 相同的结构。只影响**之后结束**的停机段，已结算的段不改。

| 场景 | code | message |
|---|---|---|
| 缺少 `remedy` 或不是 `extend/refund` | 40001 | remedy 必填(extend 或 refund) / remedy 只能是 extend 或 refund |
| 租期已结束（租约不在租或 `access_until` 已过） | 40900 | 租期已结束, 不能再修改补偿方式; 未补偿的停机将按比例退费 |
| `refund` 未达门槛 | 40900 | 未达到退费条件(本期累计停机 ≥ 租期的 10% 或单次停机 ≥ 2 小时), 只能顺延补偿 |
| `refund` 达门槛但退费通道未就绪（开关为 `false` 时） | 50000 | 易宝部分退款能力未确认，暂只支持顺延补偿（同时记录 `refund_requested_at`，补偿方式保持 `extend`） |
| 并发下租约状态已变化 | 40900 | 租约状态已变化, 请刷新后重试 |

- `extend` 随时可选（租期内），并清除 `refund_requested_at`。
- 顺延的效果：租约 `access_until`、订单 `lease_end_at`、交付 `access_expires_at` 同一事务后移停机时长（刷新撤销游标，网关按新时刻放行）；已下单未支付的 `extend` 续租单的起点与 `renewed_until` 一并后移（支付时仍能对上原订单结束时间）。
- 租期结束后才结束的停机段（含冻结中到期）无法顺延，统一转为待退费（`remedy=refund`），由 T-106 退费流程处理。

**退费的真实执行（S2 T-106）**：开关 `fleet.outage.yeepay_partial_refund_confirmed=true` 时（且 `fleet.enabled=true`、易宝已配置，否则服务启动失败），`remedy=refund` 的停机段结束即按 `remedy_amount` 从**该停机所属订单**（续租后为续租单）的支付经易宝原路部分退款：资金在待分账账户中，退款直接退回买家，不涉及分账归还。退款成功（同步结果 / 查询 / 异步通知）时写 `remedy_applied_at`；失败 1 小时后自动换新请求号重试；同一停机段至多一笔退款（幂等）。`remedy_amount=0`（停机极短向下取整为 0）直接视为已补偿。结算任务每分钟兜底提交未登记的待退费停机段。退款记录与分账扣减口径见 payment-api.md「租期末结算与部分退款」。
- 开关为 `false`（默认）时：选 `refund` 仍返回 `50000` 并只记录意向；租期结束后的待退费停机段不会退款，**该订单整条续租链不会进入分账**（资金留在易宝待分账账户），结算任务按订单发运营告警，需人工处理或打开开关。
- 先前只记录了 `refund_requested_at`（补偿按顺延执行）的买家，开关打开后**不会自动改为退费**，需买家重新选择 `refund`（已顺延的段不改）。
- 租期末结算：租约进入清除（`purging/purged`）且所有停机段补偿落地、部分退款全部成功后，根订单写 `settle_ready_at` 并开始分账；此后续租报价 / 下单返回 `40900`（「续租条件已变化…」，需重新下单）。宽限期内仍可 restart 续租。

---

## 包夜订单（S4，`pricing_mode=nightly`）✅

v1.2 S4（T-094/T-095），设计见 docs/26 §26.6、docs/25 §25.7.6。只适用于 `availability=night_window` + `fulfillment_mode=managed` + `delivery_mode=container` 的商品（Q-44）；其他商品选包夜 40001「包夜仅适用于平台托管的夜间窗口分卡容器商品」。一切以 `fleet.enabled=true` 为前提，关闭时与其他托管订单同样 40001。

**下单 `POST /orders`**（`pricing_mode=nightly`、`night_start_date`、`duration`=晚数、`quantity`=卡数）
- 金额 = `unit_price`（分/卡/晚）× 卡数 × 晚数；当晚订单（窗口已开始但未满 2 小时，Q-36）按整晚计价。
- `night_start_date` 校验（40001）：格式 `YYYY-MM-DD`；不早于可订的第一晚（当晚窗口开始满 2 小时即截止）；不晚于今天起第 30 天；末晚不超出今天起第 60 天。
- 同一事务内建租约（`reserved`）并在**同一台节点**为每晚写入同一组 k 张卡的预订（`night_bookings` 唯一键 `(slot_id, night_date)`）。预订不改卡槽状态：白天卡仍归供给方。并发抢同一组卡只有一单成功，其余 40900「所选夜晚暂无同机空闲卡, 请调整日期或卡数」。
- 订单快照写 `night_start_date` 与夜间窗口（`night_start_min/night_end_min`），供给方事后改窗口不影响已下订单；`lease_start_at` = max(首晚开通自检通过, 首晚窗口开始)，`lease_end_at` = 末晚窗口结束（固定）。

**每晚开停机（fleet 每分钟任务）**
- 首晚：支付后到达 窗口开始−10min（当晚订单为支付时刻）认领本晚卡槽并开通（同常规托管开通流水线）。到 应开通时刻+10min 仍认领不到卡（上一租约未释放等）→ 订单 `cancelled`，支付侧自动全额原路退款，买家收到「包夜订单开通失败」。首晚开通晚于应开通时刻 ≥ `fleet.outage.min_minutes` 记 `provision_delay` 停机（平台责任）。
- 窗口结束：租约 `active → parked`（不可登录），停止容器（数据保留）并把卡还给供给方；一键部署同时停止（`stop_reason=nightly`，`auto_resume` 保留）。窗口结束前 30 / 10 分钟站内信提醒，并尝试向容器内终端广播（best-effort）。
- 下一晚 窗口开始−10min：检查宿主机本晚卡上没有其他 GPU 计算进程 → 原容器 `docker start` → 网关自检 → `parked → active`，`auto_resume` 的部署自动恢复为 `pending` 重新部署。
- 开窗失败：宿主机进程占用本晚卡且到 应开通时刻+10min 仍未释放 → 本晚失败，记 `night_unpark_failed` 整晚停机（供给方责任），审计 `lease.night_unpark_failed`，通知供给方「包夜节点违约: 窗口内 GPU 被占用」与买家「包夜本晚未能开通」。其他失败（节点连不上、启动/自检失败）本晚内每分钟重试，窗口结束仍未开通则整晚按失败原因记停机（`node_offline/container_down/gateway_down`）；开窗成功但晚于应开通时刻 ≥ `min_minutes` 的延迟段同样记停机。
- 最后一晚窗口结束 = `access_until`，按常规到期进入宽限期（`fleet.grace.nightly`）。

**登录**：网关只在当晚窗口内放行（`active` 且 当前时刻 ∈ [窗口开始, min(窗口结束, access_until))），会话在窗口结束时断开。

**停机补偿**：默认顺延 = **补加一晚**（同一组卡、同窗口，接在末晚之后，`access_until` 后移 24h）；同一晚内多段停机只补一晚；补加的那晚卡已被别人预订则改为退费（`remedy=refund`，退费通道见上节）。责任金额按 实付 × 有效秒数 ÷（晚数 × 窗口时长）计算。

**续租**：包夜订单续租仍返回 50000（未开放），message「包夜订单暂不支持续租，如需继续使用请在包夜日历中选择日期重新下单」，前端原样展示。

