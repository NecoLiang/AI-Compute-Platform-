# 节点探活与调度 Scheduler API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 供应方接口需 supplier 角色；admin 接口需 operator/admin；心跳接口凭 `X-Node-Key`

**口径**（设计见 `docs/23`）
- 节点状态：`online`（心跳新鲜且有余量）/ `degraded`（心跳新鲜但无余量或负载≥95%）/ `offline`（心跳超时 90s）。
- 商品健康度 `products.health`：`healthy`/`degraded`/`offline`/`unknown`（未接入探活）。
  **offline 的商品下单被拦截**，恢复心跳立即解除；健康变化站内信通知供应方。
- `node_key` 明文仅注册时返回一次，平台只存哈希，丢失需删除节点重新注册。
- **托管节点（v1.2，`mgmt_mode="ssh"`）**：平台经 SSH 主动探活，不发心跳、不签发 `node_key`。接入、探活、卡槽、停用/恢复的完整契约见 [managed-rental-api.md](managed-rental-api.md)。仅 `fleet.enabled=true` 时可用。
  心跳超时清扫（90s → offline）只处理 `mgmt_mode="push"` 节点；ssh 节点的 online/offline 由 fleet 轻量探活判定。

---

## POST /supplier/nodes · 注册节点 ✅

```
curl -X POST http://localhost:8080/api/v1/supplier/nodes \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"product_id":1,"node_name":"gpu-node-1","total_cards":8}'
```

**响应**：`{"code":0,"data":{"node":{...},"node_key":"nk-a1b2...","notice":"node_key 仅本次展示..."}}`

- 请求体可带 `mgmt_mode`：缺省或 `"push"` 为上面的心跳节点；`"ssh"` 为托管节点（额外字段 `disk_per_card_gb`、`lease_port_start`、`lease_port_end`），响应只有 `{"node":{...}}`、不含 `node_key`，详见 [managed-rental-api.md](managed-rental-api.md#post-suppliernodes--添加托管节点-)。
- 托管商品（`fulfillment_mode=managed`）只能添加 ssh 节点，传 push 返回 40001；`fleet.enabled=false` 时传 `"ssh"` 返回 40001「托管履约未启用」。

## GET /supplier/nodes · 我的节点列表 ✅
## DELETE /supplier/nodes/:id · 删除节点 ✅

托管节点在列表中带 `mgmt_mode`、`verify_status`、`schedulable` 等字段，不含任何凭证。删除托管节点由 fleet 处理：有进行中租约的卡槽时 40900；否则同事务删除卡槽、作废任务并重算库存。`fleet.enabled=false` 时删除托管节点返回 50000（托管模块未启用，无法安全清理卡槽）。

## POST /node/heartbeat · 节点心跳（机器侧，30s 一次）✅

```
curl -X POST http://localhost:8080/api/v1/node/heartbeat \
  -H "X-Node-Key: nk-a1b2..." -H "Content-Type: application/json" \
  -d '{"node_id":1,"available_cards":8,"gpu_util_pct":35,"vram_util_pct":40}'
```

- `available_cards` 必填（0 表示无可调度容量 → degraded）；utilization 选填。
- 节点不存在或密钥错误统一返回 40001「节点或密钥不正确」（不区分两种情况，防节点枚举）；`available_cards` 越界 → 40001。

## GET /supplier/schedule-advice?order_no= · 调度建议（供应方交付用）✅
## GET /admin/schedule-advice?order_no= · 调度建议（运营视角）✅

**响应**
```json
{"code":0,"data":{
  "order_no":"ORD...","product_id":1,"need_cards":4,
  "summary":"推荐调度到节点「node-b」(得分 92): 满足 4 卡需求且综合健康度/容量适配/负载最优",
  "nodes":[
    {"node_id":2,"node_name":"node-b","status":"online","available_cards":4,"total_cards":8,
     "score":92,"verdict":"recommended",
     "reasons":["节点在线, 心跳正常","容量适配: 需求 4/可用 4, 适配得分 30/30",
       "当前 GPU 利用率 10%, 负载得分 18/20","近 24h 心跳在线率约 100%, 稳定性得分 10/10"]},
    {"node_id":1,"node_name":"node-a","status":"online","score":63,"verdict":"alternative","reasons":["..."]},
    {"node_id":3,"node_name":"node-c","status":"offline","score":0,"verdict":"unavailable",
     "reasons":["节点离线(心跳超时), 不可调度"]}
  ]
}}
```

打分规则（透明可解释）：健康度 40 + 容量适配 30（best-fit，优先恰好容纳减少碎片）+ 负载 20 + 稳定性 10。

**托管商品容量口径（T-093，docs/26 §26.5.5）**：每个节点项新增两个字段，人工商品行为不变。
- `capacity_source`：`heartbeat` 表示人工商品，`available_cards/total_cards` 取节点心跳上报；`gpu_slots` 表示托管商品（`fulfillment_mode=managed`），按该商品节点的 `gpu_slots` 计算：`total_cards` = 卡槽总数，`available_cards` = 空闲卡槽 + **本订单租约已占卡槽**（给本订单做建议时，这些卡本来就归它）。
- `max_group_free`：托管节点同一拓扑组（`topo_group`）内最多可用卡数（同样含本订单已占），衡量碎片；人工商品为 `null`。
- 打分权重不变（同一 `scoreNode`：健康 40 + 容量 30 + 负载 20 + 稳定 10），仅把容量输入换成卡槽口径。`reasons` 追加卡槽说明：「卡槽: 可用 a/共 t 卡, 拓扑组可用分布 {topo_group}:{可用} …, 同组最多 m 卡」「该订单租约已在本节点预留 n 张卡」「n 卡可在同一拓扑组内分配 / 需跨拓扑组拼接, 卡间通信带宽较低」。
- 托管节点的可调度条件与下单卡槽分配一致，不满足即 `verdict=unavailable, score=0`，首条 reason 依次为：节点离线（同人工）→「节点未通过验收(…), 不可调度」→「节点已暂停调度」→「托管节点非在线状态, 不可调度」→「节点尚无卡槽记录(未完成接入自检)」→ 整机交付「整机交付需整台 N 卡全部空闲且订单卡数等于整机卡数」→ 无磁盘配额「节点不支持磁盘配额, 只能整机(N 卡)出租」/「…当前非全部空闲」。

```json
{"node_id":2,"node_name":"node-b","status":"online","available_cards":4,"total_cards":8,
 "score":92,"verdict":"recommended","capacity_source":"gpu_slots","max_group_free":4,
 "reasons":["节点在线, 心跳正常","容量适配: 需求 4/可用 4, 适配得分 30/30","…",
   "卡槽: 可用 4/共 8 卡, 拓扑组可用分布 g0:4 g1:0, 同组最多 4 卡","4 卡可在同一拓扑组内分配"]}
```

## GET /admin/nodes?status=&page=&page_size= · 节点总览（运营）✅

托管节点额外返回 `managed`（最近探活结论、卡槽统计、SSH 地址、指纹确认状态），字段见 [managed-rental-api.md](managed-rental-api.md#get-adminnodes--节点总览扩展)；运营停用/恢复与任务重试接口也在该文件。

**前端展示建议**
- 商品卡片/详情页展示 `health` 徽章（healthy 绿 / degraded 黄 / offline 灰+禁止下单 / unknown 不展示）；
- 供应方工作台节点列表：状态灯 + 最近心跳时间 + 可用/总卡数；
- 交付页嵌入 schedule-advice：推荐节点高亮，`reasons` 直接展示。


## 订单计量与前端入口（2026-09-14）

- `card_rental` 的订单数量就是所需卡数。`outright`/`center` 按台交易，所需卡数 = 订单台数 ×（商品总卡数 / 商品总台数）。按同一商品的均一配置换算；缺少正数规格或不能整除时返回 `40001`，不猜测每台卡数，不推荐可能不足容量的节点。`colocation` 不提供 GPU 卡数建议。
- `/console/supplier/nodes`：当前供应方节点列表、注册、删除。注册响应的 `node` 为刚创建的对象，客户端只使用返回的 id；状态、心跳与时间从列表重新读取。GET 列表可能返回 `data:null`，表示无节点。列表不会提供 node_key 或 node_key_hash。
- 注册密钥仅在当前成功弹窗中暂存，默认遮挡，可主动显示/复制；关闭、离开或切换账户后不再展示，不进入 Query/Mutation data 或浏览器持久化存储。注册 BFF 响应为 `Cache-Control:no-store`。请求期间阻止重复注册；请求失败保留表单，先核对列表再重试。
- 供应方订单详情与回填交付凭证面板展示按订单查询的建议；`/admin/nodes` 为 operator/admin 的节点状态筛选、分页和订单建议查询入口。服务端继续执行角色与商品归属校验。
- `GET /admin/nodes` 返回 `data:{list,total,page,page_size}`；`list:null` 是空页。`ScheduleAdvice.nodes:null` 表示没有节点。非零业务码/HTTP 失败不能显示为空记录或推荐成功。
- 状态和心跳时间读取后端；页面每 30 秒刷新节点列表。建议在打开交付面板或显式刷新时读取，展示生成时间。推荐不分配资源、不启动实例，不改变交付状态。
- 节点侧按现有 `POST /node/heartbeat` 与 `X-Node-Key` 契约接入；UI 不代替节点发送心跳。删除会使节点密钥失效并重新计算商品健康度。操作测试只针对隔离数据库，生产节点、真实资源编排和生产发布仍需分别验收。
