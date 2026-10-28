# 一键部署 Model Deploy API（v1.2）

**Base**: `http://localhost:8080/api/v1` | 设计依据：[docs/27](../27-模型一键部署与健康检测设计.md) §27.2 / §27.3 / §27.7 / §27.8

| 状态 | 范围 |
|---|---|
| ✅ 已实现（T-096） | 模型目录 CRUD、真机验证记录、上线/退役（运营） |
| ✅ 已实现（T-097） | `GET /models`；`GET /orders/:id/deploy-plans` 判定函数与接口（**托管租约数据源未接入前固定返回 50000**） |
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
| manifest | 非空数组 `[{path,size,sha256}]`，≤10000 个文件，不允许其他字段；path 为相对路径，不得以 `/` 开头、不得含 `..`/`\`/空段，不重复；单文件 size 1 – 1 TiB（2^40）；sha256 为 64 位小写十六进制；总大小按溢出检测累加 |
| weights_bytes | 1 – 4 TiB（2^42），且 ≤ manifest 文件总大小 |
| quant | `bf16`/`fp16`/`fp8`/`int8`/`int4_awq`/`int4_gptq`/`mxfp4`/`nvfp4` |
| min_compute_capability | `主.次`，如 `8.0`/`8.9`/`9.0` |
| valid_tp | 1–64 的整数列表，不重复；存储时规范为升序（`"8,1,2,4"` → `"1,2,4,8"`） |
| kv_bytes_per_token | 1 – 64 MiB（2^26） |
| runtime_overhead_bytes | 1 – 256 GiB（2^38），取真机实测值 |
| min_context_tokens | 缺省 8192；须为 1024 的正整数倍 |
| max_model_len | min_context_tokens – 16777216（2^24） |
| engine | 缺省 `vllm`，目前只支持 `vllm` |
| engine_image | 须以 `@sha256:<64hex>` 结尾（按 digest 固定，禁止 `:latest`） |
| min_driver_version | 点分数字，最多 3 段 |
| engine_args | JSON 对象；键须在白名单内；值只能是布尔、非负数字或安全字符集字符串 `[A-Za-z0-9._:/=+-]{1,128}`，且**不得以 `-` 开头**（防止 `"--served-model-name=x"` 这类值被引擎当作另一个参数） |
| metrics_map | 必须恰好包含上例 7 个逻辑名，值为 Prometheus 指标名 |
| startup_timeout_s | 缺省 1200；60–7200 |
| license | 必填，≤64 字符 |
| commercial_use | `allowed`/`restricted`/`unknown` |

**engine_args 白名单**：`dtype`、`quantization`、`kv-cache-dtype`、`enable-prefix-caching`、`enable-chunked-prefill`、`max-num-seqs`、`max-num-batched-tokens`、`block-size`、`enforce-eager`、`tokenizer-mode`、`reasoning-parser`、`tool-call-parser`、`enable-auto-tool-choice`、`enable-expert-parallel`、`swap-space`、`disable-custom-all-reduce`。
由平台按部署选择决定的参数（`model`、`served-model-name`、`tensor-parallel-size`、`data-parallel-size`、`max-model-len`、`gpu-memory-utilization`、端口/监听地址）**不在白名单**，出现即 40001。
`trust-remote-code` 同样**不在白名单**（会执行权重仓库内的 Python 代码，属远程代码执行），出现即 40001；依赖自定义代码的模型不进目录。

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

## 模型列表（买家，登录即可）

### GET /models ✅

市场路由组（需登录，不进 Public 组）。只返回 `active` 条目，按 id 倒序。不含清单、引擎镜像、引擎参数、指标映射等运维字段。

```json
{"code":0,"data":{"list":[{
  "id":3, "model_key":"qwen3-32b-bf16", "display_name":"Qwen3 32B",
  "source":"modelscope", "repo":"Qwen/Qwen3-32B", "revision":"<固定提交号>",
  "quant":"bf16", "weights_bytes":69793218560, "min_compute_capability":"8.0",
  "valid_tp":[1,2,4,8], "max_model_len":131072, "min_context_tokens":8192,
  "license":"Apache-2.0", "commercial_use":"allowed",
  "verified_gpu_models":["H100-80G"]      // 有 result=pass 真机验证记录的卡型(型号库 model_name); 无则 []
}],"total":1}}
```

---

## 部署方案判定（买家，订单本人）

### GET /orders/:id/deploy-plans?model_id=&gpu_mem_util= ✅（数据源待 S2 接入）

`:id` 为订单号（`ORD…`/`REN…`）。只读，不写库；提交部署（T-098）时服务端会**重新判定**，不信任本结果。

| 参数 | 必填 | 说明 |
|---|:--:|---|
| model_id | 是 | `GET /models` 返回的 id；非 active 条目 40400 |
| gpu_mem_util | 否 | 显存利用率上限 `u`，小数最多两位，`0.80`–`0.95`；**不带该参数**时取 `0.90`。一旦带上（包括空值、`0`、`0.0`）就必须在范围内，否则 40001，不会回落到默认值 |

> **当前状态**：托管租约与实时卡数据由 S2 `internal/fleet` 提供（实现 `modeldeploy.LeaseSource` 并注入）。接入前，参数合法的请求一律返回
> `{"code":50000,"message":"托管租约能力尚未上线(S2 未接入), 暂无法计算部署方案"}`，不使用任何假数据。

**响应**

```json
{"code":0,"data":{
  "feasible":true,
  "plans":[
    {"tp":1,"dp":2,"cards_used":2,"max_model_len":12288,"kv_budget_bytes_per_card":3221225472,
     "kv_capacity_tokens":12288,"context_tight":true,"recommended":false},
    {"tp":2,"dp":1,"cards_used":2,"max_model_len":131072,"kv_budget_bytes_per_card":38117834752,
     "kv_capacity_tokens":290816,"context_tight":false,"recommended":true}
  ],
  "checks":[
    {"id":"M1","ok":true,"level":"info","message":"共 2 个可行组合"},
    {"id":"M2","ok":true,"level":"info","message":"推荐组合可用上下文 131072 tokens"},
    {"id":"F1","ok":true,"level":"info","message":"GPU 架构 9.0 满足要求 ≥ 8.0"},
    "… F2–F7 …",
    {"id":"F8","ok":false,"level":"confirm","message":"该模型未在 H800-80G 上实测, 显存估算可能有偏差","ack":"untested_ack"}
  ],
  "memory":[{"tp":1,"feasible":true,"kv_budget_bytes_per_card":3221225472,"kv_capacity_tokens":12288},
            {"tp":4,"feasible":false,"kv_budget_bytes_per_card":0,"kv_capacity_tokens":0,"reason":"需要 4 张卡, 租约只有 2 张"}],
  "basis":{"model_id":3,"model_key":"qwen3-32b-bf16","gpu_model":"H800-80G","cards":2,"vram_bytes_per_card":85899345920,
           "gpu_mem_util_pct":90,"compute_capability":"9.0","driver_version":"550.54.15", "…":"…"}
}}
```

- `feasible=false` 时 `plans` 为 `[]`；原因看 `checks` 中 `ok=false && level=error` 的项与 `memory[].reason`。
- `level`：`info` 通过；`error` 阻断；`warning` 仅提示；`confirm` 可行但提交部署时须带对应 `ack` 字段为 `true`（`license_ack` / `untested_ack`），否则 40001。
- `basis` 是判定输入摘要，T-098 提交时连同结果写入部署记录 `feasibility` 字段。

**显存与上下文（§27.3.1）**：对 `valid_tp` 中每个 `tp ≤ 卡数 k`，`V` 取租约内最小单卡显存：

```
B = V×u − W/tp − O              （每卡 KV 预算）
T = B×tp/K                      （KV 按 TP 切分）
T = B/K                         （kv_replicated_across_tp=true）
可行：B > 0 且 T ≥ min_context_tokens
dp = floor(k/tp)；建议上下文 = min(max_model_len, floor(T/1024)×1024)
context_tight：建议上下文 < 2×min_context_tokens
```

全部用 int64 整数精确计算（不经浮点），同一输入任何机器结果一致；每一步乘减都做溢出检测，任何输入都不会 panic。
目录参数超出上述校验范围（库中可能存有校验收紧前写入的旧行）时不做显存计算，`feasible=false`、`plans=[]`、`memory=[]`，
M1 为 `error`：“目录条目 … 参数超出合理范围(…), 无法判定”。租约侧单卡显存 ≤0 或 >1 TiB 时 M1 同样判“无法判定”；
显存占用为负或超过总量的卡 F3 不通过；缓存容量为负或数值溢出时 F4 判“无法判定”。**推荐**：用卡最多者；同等用卡下上下文不紧张者优先；再取 tp 最小者。

**检查项**

| id | 检查 | 不满足时 |
|---|---|---|
| M1 | 至少一个 (tp,dp) 满足显存与最低上下文 | error |
| M2 | 推荐组合上下文是否紧张 | warning |
| F1 | 型号库 `compute_capability` ≥ 条目 `min_compute_capability`；型号未登记即"无法判定" | error，并列出目录中可在该卡型运行的其他 active 条目 |
| F2 | 节点实测驱动 ≥ `min_driver_version`；驱动未知即"无法判定" | error |
| F3 | 各卡当前显存占用 ≤ 2% | error |
| F4 | 节点已缓存该模型，或缓存区剩余 + 可按 LRU 清理 ≥ 权重×1.1 | error |
| F5 | 存在 tp>1 的可行组合且卡间无 NVLink | warning |
| F6 | 无故障卡、近 1 小时无致命 Xid | error |
| F7 | `commercial_use` 非 `allowed`（`restricted` 与 `unknown`） | confirm，`ack=license_ack` |
| F8 | 租约卡型（型号库 `model_name` 精确匹配）在该条目下无 `result=pass` 验证记录 | confirm，`ack=untested_ack` |

**错误码**

| 码 | 场景 |
|---|---|
| 40001 | 订单号格式无效；model_id 缺失/非法；gpu_mem_util 格式或范围错误；订单不是托管租用 |
| 40400 | 订单不存在或不属于当前用户；模型不存在或未上线 |
| 40900 | 托管租约未处于 active |
| 50000 | 托管租约数据源未接入（见上）；内部错误 |

### 数据源接口（供 S2 实现）

`backend/internal/modeldeploy/lease_source.go`：

```go
type LeaseSource interface {
    ManagedLeaseSnapshot(ctx context.Context, buyerID int64, orderNo string, modelID int64) (*LeaseSnapshot, error)
}
// 哨兵错误(可 %w 包装): ErrOrderNotFound→40400, ErrNotManaged→40001, ErrLeaseNotActive→40900
```

实现方负责订单归属、托管履约与租约 active 校验；卡数据取自 `gpu_slots`（lease_id 归属）与最近一次探活/采样，不得用商品声明值代替。快照字段：`GPUModel`（型号库 model_name）、`DriverVersion`、`NVLink`、`Cards[]{VRAMTotalBytes,VRAMUsedBytes,Faulty,FatalXidLastHour}`、`ModelCached`、`CacheFreeBytes`、`CacheEvictableBytes`。接入方式：`main.go` 中 `modelDeploySvc.SetLeaseSource(<fleet 实现>)`。

---

## 数据迁移

`backend/migrations/030_model_deployment.up.sql`：`gpu_catalog.compute_capability`、`supplier_nodes.model_cache_gb`，新建 `model_catalog`、`model_catalog_verifications`、`node_model_cache`、`model_deployments`、`deployment_health_samples`（结构同 docs/27 §27.7）。

`030_model_deployment.down.sql` 无条件 `SIGNAL SQLSTATE '45000'`：部署记录、真机验证记录与健康采样属于业务数据，回退须经审计归档，禁止自动删除。
