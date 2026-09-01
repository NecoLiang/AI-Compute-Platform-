# 节点探活与调度 Scheduler API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 供应方接口需 supplier 角色；admin 接口需 operator/admin；心跳接口凭 `X-Node-Key`

**口径**（设计见 `docs/23`）
- 节点状态：`online`（心跳新鲜且有余量）/ `degraded`（心跳新鲜但无余量或负载≥95%）/ `offline`（心跳超时 90s）。
- 商品健康度 `products.health`：`healthy`/`degraded`/`offline`/`unknown`（未接入探活）。
  **offline 的商品下单被拦截**，恢复心跳立即解除；健康变化站内信通知供应方。
- `node_key` 明文仅注册时返回一次，平台只存哈希，丢失需删除节点重新注册。

---

## POST /supplier/nodes · 注册节点 ✅

```
curl -X POST http://localhost:8080/api/v1/supplier/nodes \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"product_id":1,"node_name":"gpu-node-1","total_cards":8}'
```

**响应**：`{"code":0,"data":{"node":{...},"node_key":"nk-a1b2...","notice":"node_key 仅本次展示..."}}`

## GET /supplier/nodes · 我的节点列表 ✅
## DELETE /supplier/nodes/:id · 删除节点 ✅

## POST /node/heartbeat · 节点心跳（机器侧，30s 一次）✅

```
curl -X POST http://localhost:8080/api/v1/node/heartbeat \
  -H "X-Node-Key: nk-a1b2..." -H "Content-Type: application/json" \
  -d '{"node_id":1,"available_cards":8,"gpu_util_pct":35,"vram_util_pct":40}'
```

- `available_cards` 必填（0 表示无可调度容量 → degraded）；utilization 选填。
- 密钥错误 / available>total → 40001。

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

## GET /admin/nodes?status=&page=&page_size= · 节点总览（运营）✅

**前端展示建议**
- 商品卡片/详情页展示 `health` 徽章（healthy 绿 / degraded 黄 / offline 灰+禁止下单 / unknown 不展示）；
- 供应方工作台节点列表：状态灯 + 最近心跳时间 + 可用/总卡数；
- 交付页嵌入 schedule-advice：推荐节点高亮，`reasons` 直接展示。
