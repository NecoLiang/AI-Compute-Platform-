# 智能选型 Agent Search API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 需 `Bearer <token>`（登录用户）

**口径**
- 设计见 `docs/23`：LLM 负责**算力推定**（任务需要多大显存/多少卡，推导带公式与数字）
  与需求解析（分析步骤 + 结构化条件）；商品匹配由平台确定性打分完成 ——
  `matches` 里全部是平台真实在售商品，价格库存不会编造。
- 用户未明确卡数时，按 `compute_estimate.min_cards` 兜底做库存与预算校验。
- 每用户 10 次/分钟限流（42900）；需求描述 ≤500 字。
- 模型网关未配置时返回 50000 + 明确错误信息（不做规则降级）。

---

## POST /market/agent-search · 智能选型 ✅

```
curl -X POST http://localhost:8080/api/v1/market/agent-search \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"query":"我想部署一个 72B 的大模型做在线推理, INT8 量化, 并发不高, 预算每月10万以内"}'
```

**响应（真实联调输出，2026-09-01 DeepSeek-V4-Flash 实测）**
```json
{"code":0,"data":{
  "relevant":true,
  "analysis_steps":[
    {"title":"理解需求","detail":"您需要部署72B大模型做在线推理，使用INT8量化，并发不高，预算每月10万元以内。"},
    {"title":"算力推定","detail":"INT8推理显存需求约72B×1字节×1.3≈94GB，需至少2张80G显卡（如A100-80G或H100）。"},
    {"title":"确定筛选条件","detail":"根据显存需求，筛选80G级显卡，推荐A100-80G或H100，按月度计费，预算上限10万元/月。"}
  ],
  "compute_estimate":{
    "total_vram_gb":94,"per_card_vram_gb":80,"min_cards":2,
    "compute_class":"推理-中等规模",
    "basis":"显存≈72B×1字节(INT8)×1.3≈94GB，单卡80G需2卡"
  },
  "requirement":{"purpose":"72B 大模型在线推理","gpu_models":["A100-80G","H100"],"card_count":2,
    "pricing_mode":"monthly","duration_hint":1,"budget_fen_max":10000000,"region":""},
  "matches":[{
    "product":{"id":1,"gpu_model":"A100-80G","stock":16,"unit_price":1200000,"region":"华北-廊坊","...":"..."},
    "score":90,
    "reasons":["GPU 型号 A100-80G 符合需求","库存 16 可满足算力推定的 2 卡需求",
      "预估费用 24000.00 元在预算内","计费模式匹配(monthly)"]
  }]
}}
```

**响应（与算力无关的问题 → 拒答，不输出分析）**
```json
{"code":0,"data":{"relevant":false,"reject_reason":"该问题与算力资源采购无关","matches":[]}}
```

**响应（无匹配）**：`matches:[]` + `note:"当前在售商品中暂无满足条件的配置..."`

**前端展示建议**
- `analysis_steps` 逐步打字机/流式动画展示（这是页面上唯一的"AI 过程"），随后亮出 `matches` 商品卡片；
- `compute_estimate` 做成醒目的「算力推定卡」：总显存 / 建议卡数 / 推导依据（`basis` 带公式，是智能性的核心展示位）；
- 商品卡片上直接展示 `reasons`（每分都有依据，这是"智能可信"的卖点）；
- `relevant=false` 时展示 `reject_reason`，不渲染分析区。

| 错误码 | 场景 |
|---|---|
| 40001 | query 缺失/超长 |
| 42900 | 触发限流（10 次/分钟） |
| 50000 | 模型网关未配置或调用失败 |
