# GPU 型号库 GPU Catalog API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 下拉接口公开；管理接口需 operator/admin

**口径**
- 平台**自维护**的权威型号字典（不依赖外部 API），供应方发布商品时下拉选择，替代自由文本。
- 初始 32 款：国产以 **2026-05 国家安可认证目录**为骨架（`secure_certified=true`），海外取数据中心与主流消费卡。
- `fp16_tflops` 统一口径为 **FP16 稠密 Tensor 算力**；规格未经厂商官网核实的为 `null`（宁缺毋错，运营后台可补）。
- 新增型号走管理端接口，保存后**即刻**出现在下拉，无需发版。

---

## GET /gpu-catalog · 型号下拉（公开）✅

```
curl "http://localhost:8080/api/v1/gpu-catalog?origin=domestic"
```

| 参数 | 必填 | 说明 |
|---|:--:|---|
| origin | 否 | `domestic` 国产 / `international` 海外 |
| grade | 否 | `datacenter` 数据中心 / `consumer` 消费级 |
| vendor | 否 | 厂商精确匹配，如 `NVIDIA`、`华为昇腾` |
| q | 否 | 型号名模糊搜索 |

**响应**（仅 enabled 条目，已按 `sort_weight` 降序 → 厂商 → 型号排好序）
```json
{"code":0,"data":{
  "list":[{
    "id":16,
    "vendor":"华为昇腾",          // 厂商
    "model_name":"昇腾910B",      // 标准型号名(全局唯一, 商品 gpu_model 绑定此值)
    "origin":"domestic",          // domestic 国产 / international 海外
    "grade":"datacenter",         // datacenter 数据中心 / consumer 消费级
    "vram_gb":64,                 // 单卡显存 GB; null=待核实
    "vram_type":"HBM2e",          // 显存类型; null=待核实
    "fp16_tflops":320.0,          // FP16 稠密 Tensor 算力; null=待核实
    "interconnect":"HCCS",        // 多卡互联; null=待核实
    "secure_certified":true,      // ⭐ 是否入选国家安可认证目录 → 前端可打「安可认证」徽章
    "status":"enabled",
    "sort_weight":100,
    "created_at":"2026-09-04T10:00:00Z",
    "updated_at":"2026-09-04T10:00:00Z"
  }],
  "total":32
}}
```

**前端展示建议**
- 下拉建议按 `origin` 分组（国产/海外两个分组头），组内顺序直接用返回顺序；
- `secure_certified=true` 加「安可认证」徽章——政企客户的关键决策信息；
- 选中型号后可用 `vram_gb`/`fp16_tflops` 在发布表单旁展示规格摘要（null 则不展示该行）；
- 商品的 `gpu_model` 字段填 `model_name` 原值，保证与型号库、智能搜索归一化一致。

---

## GET /admin/gpu-catalog · 管理端全量列表 ✅

同上筛选参数；额外返回 `disabled` 条目与 `spec_source`（规格来源标注，如「NVIDIA datasheet」「安可目录, 规格待官网复核」）。

## POST /admin/gpu-catalog · 新增型号 ✅

```json
{"vendor":"寒武纪","model_name":"思元690","origin":"domestic","grade":"datacenter",
 "vram_gb":96,"vram_type":"HBM3","fp16_tflops":null,"interconnect":"MLU-Link",
 "secure_certified":false,"spec_source":"寒武纪官网","sort_weight":86}
```
- `vendor`/`model_name`/`origin`/`grade` 必填，其余可空；`model_name` 全局唯一（重复返回 40001「型号已存在」）。
- 新增默认 `enabled`。

## PUT /admin/gpu-catalog/:id · 更新型号 ✅

整体提交（与 POST 同字段 + `status`）。`status:"disabled"` 即从公开下拉隐藏（下架不删除，商品历史引用不受影响）。

| 错误码 | 场景 |
|---|---|
| 40001 | 必填缺失 / 枚举值非法 / 型号重名 / 型号不存在 |
| 50000 | 数据库异常 |
