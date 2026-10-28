# 一键部署 Model Deploy API（v1.2）

**Base**: `http://localhost:8080/api/v1` | 设计依据：[docs/27](../27-模型一键部署与健康检测设计.md) §27.2 / §27.3 / §27.7 / §27.8

| 状态 | 范围 |
|---|---|
| ✅ 已实现（T-096） | 模型目录 CRUD、真机验证记录、上线/退役（运营） |
| ⏳ 未实现（T-098/T-099） | `POST/GET /orders/:id/deployments*`、`GET /admin/deployments`（依赖 S2 托管租约与节点任务） |

统一信封 `{code,message,data,request_id}`；错误码只用 40001/40100/40101/40300/40400/40900/42900/50000。

---

## 模型目录（运营，operator/admin）

### 状态机

```
draft ──activate(须≥1条 pass 验证)──▶ active ──retire──▶ retired(终态)
  └──────────────────retire──────────────────────────────▶
```

- 只可编辑 `draft`（PUT 整体替换）。`active` 条目要改任何字段须**新建条目**——`revision` 变化即为新模型，旧条目可退役。
- `activate`：已是 `active` 幂等返回；`retired` 不可重新上线（40900）；无 `result=pass` 验证记录 40001。
- `retire`：`draft`/`active` 均可退役，已退役幂等返回。退役只影响新部署，**不影响运行中部署**。
- 所有状态迁移为事务内 `SELECT … FOR UPDATE` + 守卫式 `UPDATE … WHERE status IN (…)`。

### GET /admin/model-catalog?status= · 列表 ✅

`status` 可选 `draft`/`active`/`retired`，为空返回全部；其他值 40001。响应 `{"list":[条目…],"total":N}`，按 id 倒序。

### GET /admin/model-catalog/:id · 详情 ✅

条目全字段 + `verifications`（按 `tested_at` 倒序）。不存在 40400。

### POST /admin/model-catalog · 新建 ✅（一律为 `draft`）

```json
{
  "model_key": "qwen3-32b-bf16",
  "display_name": "Qwen3 32B",
  "source": "modelscope",
  "repo": "Qwen/Qwen3-32B",
  "revision": "<40 或 64 位小写十六进制提交号>",
  "manifest": [{"path": "model-00001-of-00017.safetensors", "size": 3963750880, "sha256": "<64hex>"}],
  "weights_bytes": 69793218560,
  "quant": "bf16",
  "min_compute_capability": "8.0",
  "valid_tp": "1,2,4,8",
  "kv_bytes_per_token": 262144,
  "kv_replicated_across_tp": false,
  "runtime_overhead_bytes": 4294967296,
  "max_model_len": 131072,
  "min_context_tokens": 8192,
  "engine": "vllm",
  "engine_image": "registry.example.com/vllm/vllm-openai:v0.11.0@sha256:<64hex>",
  "min_driver_version": "535.104.05",
  "engine_args": {"enable-prefix-caching": true, "max-num-seqs": 256},
  "metrics_map": {
    "requests_running": "vllm:num_requests_running",
    "requests_waiting": "vllm:num_requests_waiting",
    "kv_cache_usage": "vllm:gpu_cache_usage_perc",
    "ttft_seconds": "vllm:time_to_first_token_seconds",
    "generation_tokens_total": "vllm:generation_tokens_total",
    "prompt_tokens_total": "vllm:prompt_tokens_total",
    "preemptions_total": "vllm:num_preemptions_total"
  },
  "startup_timeout_s": 1200,
  "license": "Apache-2.0",
  "commercial_use": "allowed"
}
```

**校验规则（任一不满足 40001，`message` 指明字段）**

| 字段 | 规则 |
|---|---|
| model_key | `^[a-z0-9][a-z0-9._-]{1,63}$`，全局唯一（重复 40900） |
| display_name | 必填，≤128 字 |
| source | 缺省 `modelscope`，目前只支持 `modelscope` |
| repo | `组织/模型` |
| revision | **完整**提交号：40 或 64 位小写十六进制；不接受分支名、标签、短哈希 |
| manifest | 非空数组 `[{path,size,sha256}]`，不允许其他字段；path 为相对路径，不得以 `/` 开头、不得含 `..`/`\`/空段，不重复；size>0；sha256 为 64 位小写十六进制 |
| weights_bytes | >0 且 ≤ manifest 文件总大小 |
| quant | `bf16`/`fp16`/`fp8`/`int8`/`int4_awq`/`int4_gptq`/`mxfp4`/`nvfp4` |
| min_compute_capability | `主.次`，如 `8.0`/`8.9`/`9.0` |
| valid_tp | 1–64 的整数列表，不重复；存储时规范为升序（`"8,1,2,4"` → `"1,2,4,8"`） |
| kv_bytes_per_token | >0 |
| runtime_overhead_bytes | >0，取真机实测值 |
| min_context_tokens | 缺省 8192；须为 1024 的正整数倍 |
| max_model_len | ≥ min_context_tokens |
| engine | 缺省 `vllm`，目前只支持 `vllm` |
| engine_image | 须以 `@sha256:<64hex>` 结尾（按 digest 固定，禁止 `:latest`） |
| min_driver_version | 点分数字，最多 3 段 |
| engine_args | JSON 对象；键须在白名单内；值只能是布尔、非负数字或安全字符集字符串 `[A-Za-z0-9._:/=+-]{1,128}` |
| metrics_map | 必须恰好包含上例 7 个逻辑名，值为 Prometheus 指标名 |
| startup_timeout_s | 缺省 1200；60–7200 |
| license | 必填，≤64 字符 |
| commercial_use | `allowed`/`restricted`/`unknown` |

**engine_args 白名单**：`dtype`、`quantization`、`kv-cache-dtype`、`enable-prefix-caching`、`enable-chunked-prefill`、`max-num-seqs`、`max-num-batched-tokens`、`block-size`、`enforce-eager`、`trust-remote-code`、`tokenizer-mode`、`reasoning-parser`、`tool-call-parser`、`enable-auto-tool-choice`、`enable-expert-parallel`、`swap-space`、`disable-custom-all-reduce`。
由平台按部署选择决定的参数（`model`、`served-model-name`、`tensor-parallel-size`、`data-parallel-size`、`max-model-len`、`gpu-memory-utilization`、端口/监听地址）**不在白名单**，出现即 40001。

### PUT /admin/model-catalog/:id · 整体更新（仅 draft）✅

请求体同 POST，整体替换（未提交的可选字段回到缺省值）。不存在 40400；非 `draft` 40900；改成已存在的 model_key 40900。

### POST /admin/model-catalog/:id/verifications · 录入真机验证记录 ✅

```json
{"gpu_model":"H100-80G","cards":2,"tp":2,"max_model_len":32768,
 "measured_overhead_bytes":4294967296,"result":"pass","notes":"启动 312s, 推理正常",
 "tested_at":"2026-10-28T10:00:00+08:00"}
```

| 字段 | 规则 |
|---|---|
| gpu_model | 必填，须为 [GPU 型号库](gpu-catalog-api.md) 中存在的 `model_name`（否则 40001），F8 按此精确匹配 |
| cards | ≥1 |
| tp | 须属于条目 `valid_tp`，且 ≤ cards |
| max_model_len | 1 – 条目 `max_model_len` |
| measured_overhead_bytes | 可选，>0 |
| result | `pass` / `fail` |
| notes | 可选，≤512 字；空白视为不填 |
| tested_at | 必填，RFC3339；不得晚于当前时间 5 分钟以上 |

`draft` 与 `active` 条目都可录入（active 条目可补测新卡型以解除 F8 提示）；`retired` 40900；不存在 40400。

### POST /admin/model-catalog/:id/activate · 上线 ✅
### POST /admin/model-catalog/:id/retire · 退役 ✅

无请求体，返回更新后的条目。规则见上方状态机。

### 错误码

| 码 | 场景 |
|---|---|
| 40001 | 字段校验失败；status 筛选值非法；上线时无 pass 验证记录；验证记录卡型不在型号库 |
| 40400 | 条目不存在 |
| 40900 | model_key 重复；编辑非 draft；已退役条目上线或录入验证；并发下状态已变化 |
| 50000 | 数据库异常（已记日志，不外泄细节） |

---

## 数据迁移

`backend/migrations/030_model_deployment.up.sql`：`gpu_catalog.compute_capability`、`supplier_nodes.model_cache_gb`，新建 `model_catalog`、`model_catalog_verifications`、`node_model_cache`、`model_deployments`、`deployment_health_samples`（结构同 docs/27 §27.7）。

`030_model_deployment.down.sql` 无条件 `SIGNAL SQLSTATE '45000'`：部署记录、真机验证记录与健康采样属于业务数据，回退须经审计归档，禁止自动删除。
