# 智能选型 Agent Search API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 需 `Bearer <token>`（登录用户）

**口径**
- 设计见 `docs/23`：LLM 只做需求解析（输出分析步骤 + 结构化条件），商品匹配由平台
  确定性打分完成 —— `matches` 里全部是平台真实在售商品，价格库存不会编造。
- 每用户 10 次/分钟限流（42900）；需求描述 ≤500 字。
- 模型网关未配置时返回 50000 + 明确错误信息（不做规则降级）。

---

## POST /market/agent-search · 智能选型 ✅

```
curl -X POST http://localhost:8080/api/v1/market/agent-search \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"query":"我要微调一个7B模型, 预算每月20万, 最好在华北"}'
```

**响应（匹配成功）**
```json
{"code":0,"data":{
  "relevant":true,
  "analysis_steps":[
    {"title":"识别任务类型","detail":"7B 参数模型微调, 属于中等规模训练任务, 显存需求高"},
    {"title":"推导硬件配置","detail":"建议 8 卡 80G 显存级别 GPU, 如 A100-80G"},
    {"title":"确定筛选条件","detail":"型号 A100-80G、8 卡、月租、预算 20 万内、华北优先"}
  ],
  "requirement":{"purpose":"7B 模型微调","gpu_models":["A100-80G"],"card_count":8,
    "pricing_mode":"monthly","duration_hint":1,"budget_fen_max":20000000,"region":"华北"},
  "matches":[{
    "product":{"id":1,"gpu_model":"A100-80G","stock":16,"unit_price":1200000,"region":"华北-廊坊","...":"..."},
    "score":100,
    "reasons":["GPU 型号 A100-80G 符合需求","库存 16 可满足 8 卡需求",
      "预估费用 96000.00 元在预算内","地域 华北-廊坊 符合要求","计费模式匹配(monthly)"]
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
- 商品卡片上直接展示 `reasons`（每分都有依据，这是"智能可信"的卖点）；
- `relevant=false` 时展示 `reject_reason`，不渲染分析区。

| 错误码 | 场景 |
|---|---|
| 40001 | query 缺失/超长 |
| 42900 | 触发限流（10 次/分钟） |
| 50000 | 模型网关未配置或调用失败 |
