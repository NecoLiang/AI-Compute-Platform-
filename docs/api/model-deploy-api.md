# 一键部署 Model Deploy API（v1.2）

**Base**: `http://localhost:8080/api/v1` | 设计依据：[docs/27](../27-模型一键部署与健康检测设计.md) §27.2 / §27.3 / §27.7 / §27.8

| 状态 | 范围 |
|---|---|
| ✅ 已实现（T-096） | 模型目录 CRUD、真机验证记录、上线/退役（运营） |
| ✅ 已实现（T-097） | `GET /models`；`GET /orders/:id/deploy-plans` 判定函数与接口；托管租约数据源已由 `internal/fleet` 接入（仅 `fleet.enabled=true` 时注入，否则固定 50000）。F3–F6 所需数据由周期探活采集（migration 031），**数据缺失或过期时对应项判"无法判定"、`feasible=false`**，见下文"数据源接入现状"；真机采集未验收 |
| ✅ 已实现（T-098/T-099） | `POST/GET /orders/:id/deployments*`、`GET /admin/deployments`；部署流水线（`deploy_model/pull_model/stop_model` 节点任务）、60s 健康轮询、自动重启、采样 7 天清理，均挂在 `internal/fleet` 运行期上（仅 `fleet.enabled=true`）。集成测试以测试内 SSH 服务器回放；**真机 GPU/vLLM、ModelScope 拉取未验收**（docs/27 §27.9 真机验收清单） |

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

### GET /orders/:id/deploy-plans?model_id=&gpu_mem_util= ✅（数据源已接入，部分输入待采集）

`:id` 为订单号（`ORD…`/`REN…`）。只读，不写库；提交部署（T-098）时服务端会**重新判定**，不信任本结果。

| 参数 | 必填 | 说明 |
|---|:--:|---|
| model_id | 是 | `GET /models` 返回的 id；非 active 条目 40400 |
| gpu_mem_util | 否 | 显存利用率上限 `u`，小数最多两位，`0.80`–`0.95`；**不带该参数**时取 `0.90`。一旦带上（包括空值、`0`、`0.0`）就必须在范围内，否则 40001，不会回落到默认值 |

> **当前状态**：托管租约与卡数据由 `internal/fleet`（`fleet.ModelDeployLeases`）提供，`main.go` 仅在 `fleet.enabled=true` 时注入。
> `fleet.enabled=false`（未注入）时，参数合法的请求一律返回
> `{"code":50000,"message":"托管租约能力尚未上线(S2 未接入), 暂无法计算部署方案"}`，不使用任何假数据。
> 注入后，探活尚未采集的输入不给默认值，对应检查写明"无法判定"及原因，因此**目前所有订单 `feasible=false`**（F3、F6 恒为 error），
> 详见"数据源接入现状"。

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
| F1 | 型号库 `compute_capability` ≥ 条目 `min_compute_capability`；型号未登记即"无法判定" | error，message 末尾列出"其他可用目录条目"：所有 `min_compute_capability` ≤ 该卡型的其他 active 条目（按 model_key 字典序）。目录无"基础模型"字段，**不限于同一模型的量化变体** |
| F2 | 节点实测驱动 ≥ `min_driver_version`；驱动未知即"无法判定" | error |
| F3 | 各卡当前显存占用 ≤ 2%；数据源未采集即"无法判定" | error |
| F4 | 节点已缓存该模型，或缓存区剩余 + 可按 LRU 清理 ≥ 权重×1.1；未缓存且剩余空间未采集即"无法判定" | error |
| F5 | 存在 tp>1 的可行组合且卡间无 NVLink；NVLink 未知时提示"无法判定" | warning |
| F6 | 无故障卡、近 1 小时无致命 Xid；有故障卡先报故障，否则 Xid 历史未采集即"无法判定" | error |

`basis.undetermined`（可选）：`{输入键: 原因}`，列出本次判定中数据源声明未采集的输入（键见下表）。
| F7 | `commercial_use` 非 `allowed`（`restricted` 与 `unknown`） | confirm，`ack=license_ack` |
| F8 | 租约卡型（型号库 `model_name` 精确匹配）在该条目下无 `result=pass` 验证记录 | confirm，`ack=untested_ack` |

**错误码**

| 码 | 场景 |
|---|---|
| 40001 | 订单号格式无效；model_id 缺失/非法；gpu_mem_util 格式或范围错误；订单不是托管租用；整机(bare_metal)租约（引擎依赖租约容器网络与 CPU/内存配额，整机不支持一键部署） |
| 40400 | 订单不存在或不属于当前用户；模型不存在或未上线 |
| 40900 | 托管租约未处于 active |
| 50000 | 托管租约数据源未接入（`fleet.enabled=false`）；租约卡槽数与卡数不一致、节点/商品缺失等数据不一致；内部错误 |

### 数据源接口

`backend/internal/modeldeploy/lease_source.go`：

```go
type LeaseSource interface {
    ManagedLeaseSnapshot(ctx context.Context, buyerID int64, orderNo string, modelID int64) (*LeaseSnapshot, error)
}
// 哨兵错误(可 %w 包装): ErrOrderNotFound→40400, ErrNotManaged→40001, ErrBareMetalLease→40001, ErrLeaseNotActive→40900
```

实现方负责订单归属、托管履约与租约 active 校验；卡数据取自 `gpu_slots`（lease_id 归属）与最近一次探活/采样，不得用商品声明值代替。快照字段：`GPUModel`（型号库 model_name）、`DriverVersion`、`NVLink`、`Cards[]{VRAMTotalBytes,VRAMUsedBytes,Faulty,FatalXidLastHour}`、`ModelCached`、`CacheFreeBytes`、`CacheEvictableBytes`、`Unknown`。

`Unknown map[string]string`：实现方**尚未采集或数据已过期**的输入必须列在这里（值为面向买家的原因），不得以 0/false 冒充实测值。允许的键：
`vram_usage`（F3）、`model_cache`（F4）、`nvlink`（F5）、`fatal_xid`（F6）；出现其他键视为实现错误（50000）。

### 数据源接入现状（integration 分支）

实现：`backend/internal/fleet/model_deploy_source.go`（`fleet.ModelDeployLeases`，只读）。接线：`backend/cmd/server/main.go`
`if fleetSvc != nil { modelDeploySvc.SetLeaseSource(fleetSvc.ModelDeployLeaseSource()) }`。

| 快照字段 | 来源（真实数据） |
|---|---|
| 订单归属 / 托管履约 | `orders`（`compute.OrderForAccess`）；他人订单与不存在同为 40400；非托管 40001 |
| 租约与状态 | `leases`（按该订单 `order_id`），非 `active` → 40900 |
| `GPUModel` | `products.gpu_model`（托管商品提审时 P5 已核对节点实测型号与之一致） |
| `DriverVersion` | `supplier_nodes.driver_version`（P4 nvidia-smi 实测，12h 完整探活刷新）；为空 → F2"无法判定" |
| `Cards[].UUID / VRAMTotalBytes` | `gpu_slots.gpu_uuid` / `vram_mb`×2^20（P4 实测 memory.total，MiB） |
| `Cards[].Faulty` | `gpu_slots.state='faulty'` |
| `Cards[].VRAMUsedBytes`（F3） | `gpu_slots.vram_used_mb`×2^20（轻量探活 60s 逐卡 `memory.used`） |
| `Cards[].FatalXidLastHour`（F6） | `gpu_xid_events` 中 `occurred_at ≥ now-1h` 且 xid 属致命清单（`fleet.FatalXids`）、PCI 地址与该卡 `gpu_slots.pci_bus_id` 相同的事件 |
| `NVLink`（F5） | `gpu_slots.topo_links`（完整探活 P6 两两矩阵）：租约内每对卡的连接均为 `NV#` 才为是；单卡为否。无有效矩阵时退回 `topo_group` 分组：跨组、同组但非 `-nvlink` 组 → 确定为否，同属 `-nvlink` 组 → 未知 |
| `ModelCached` | `node_model_cache` 中 `state='ready' AND verified_at IS NOT NULL` 的行 |
| `CacheFreeBytes`（F4） | `supplier_nodes.cache_free_bytes`（轻量探活 60s 对缓存目录 `df -B1` 的可用量，完整探活也写；来源记在 `cache_source`）− `downloading` 条目的完整权重 − 采集之后才校验完成的 `ready` 条目权重（均按 `model_catalog.weights_bytes` 最坏情况预扣），下限 0 |
| `CacheEvictableBytes`（F4） | 已校验 `ready` 条目中，未被该节点未终止部署（`model_deployments.state` 不是 `stopped/failed`）引用的条目 `weights_bytes` 之和（docs/27 §27.4.3） |

**新鲜度规则**（时间均为平台时钟；不满足即列入 `Unknown`，不用旧值）：

| `Unknown` 键 | 视为已知的条件 | 不满足时（原因示例） | 影响 |
|---|---|---|---|
| `vram_usage` | 租约内**每张卡**的 `telemetry_at` 在 3 分钟内（3 个轻探活周期）且 `vram_used_mb` 非空 | 尚未采集 / 超过 3 分钟未更新（节点可能失联） | F3 error"无法判定" |
| `fatal_xid` | 节点 `xid_scan_ok_at` 在 3 分钟内，且连续覆盖起点 `xid_covered_from ≤ now-1h`；各卡有 PCI 地址 | 覆盖不足 1 小时（新接入/刚恢复/中断后需累计）/ 扫描超过 3 分钟未成功 / 扫描不可用（附 `xid_scan_note`，如无权读内核日志） | 无故障卡时 F6 error"无法判定" |
| `model_cache` | 已缓存该模型；或 `cache_observed_at` 在 3 分钟内（与显存占用相同）；或最新读数来自完整探活（`cache_source='full'`，即轻探活一直读不到 df、没有更新的读数）且在 13 小时内 —— 同时下载中条目都有目录权重 | 未采集 / 缓存目录不存在（附 `cache_note`）/ 轻探活读数超过 3 分钟未更新（附最近一次读取失败原因）/ 完整探活读数超过 13 小时 | 未缓存时 F4 error"无法判定" |
| `nvlink` | 每张卡的 `topo_at` 在 13 小时内且矩阵覆盖租约内各卡；或分组能确定为否 | 未采集或过期，且同属 `-nvlink` 组 | F5 warning"无法判定"（不阻断） |

**时区前提**：以上时间全部由应用写入、由应用时钟比较；成立的前提是数据库会话时区与应用一致（+08:00，DSN `loc=Asia/Shanghai`）。平台启动时已实测校验（`backend/pkg/db/mysql.go` `verifyTimezone`），不一致直接拒绝启动；本数据源不再单独处理。

覆盖区间终点是最近一次成功扫描（≤3 分钟前），其后至今的几分钟由下一次轻探活补上；同样，显存占用是 ≤3 分钟前的实测值。
提交部署（T-098）时由部署任务在节点上再次确认，不以本快照为准。

**未验收**：以上数据均由测试内 SSH 服务器按真实输出格式回放验证；真实 GPU 节点上的 `nvidia-smi` 字段别名、`journalctl`/`dmesg` 时间格式与权限、`topo -m` 矩阵、缓存目录 `df` 尚未在真机验收。

**运行期接入（T-098/T-099）**：`backend/internal/fleet/model_deploy_runtime.go` 实现 `modeldeploy.Runtime`，main 中 `fleetSvc.AttachModelDeploy(modelDeploySvc)` + `modelDeploySvc.SetDeployRuntime(...)`（仅 `fleet.enabled=true`）。

| 运行期能力 | 来源 |
|---|---|
| 节点命令（`Dial`） | fleet 管理通道 SSH（与开通/探活同一把节点密钥、主机指纹校验） |
| `LeaseRuntime` | `leases`（状态、容器/网络名、`lease_no`）+ 租约卡槽 + 节点总配额扣除预留后按卡数折算的租约完整配额（`--cpus/--memory`） |
| 任务入队（`Enqueue`） | `node_tasks`，与部署状态迁移**同一事务**；执行体 `deploy_model/pull_model/stop_model` 注册在 fleet worker |
| `CardHealth`（L5） | 轻探活写入 `gpu_slots` 的逐卡遥测（温度/降频/ECC，3 分钟内为新鲜）+ `gpu_xid_events` 近 1 小时致命 Xid；**健康轮询不另跑 nvidia-smi** |
| 租约钩子 | 冻结 `lease_frozen`、到期转宽限 `lease_expired`、清除 `lease_purged`：在 fleet 租约状态迁移的同一事务内调用 `StopForLeaseTx`；租约停止/清除命令也会删除残留引擎容器 |
| 周期任务 | 健康轮询 60s（含拉取进度跟踪 `PollPulls`）；采样清理每日一次，删除 `sampled_at < now-7d` |
| 实时检查 | 部署任务在节点上重新执行 F3（逐卡显存）/F4（缓存目录 `df`）/F6，并核对卡 UUID 属于本租约；不以快照为准 |

---

## 部署（买家，订单本人）✅（T-098/T-099，仅 `fleet.enabled=true` 时注册）

`fleet.enabled=false` 时以下路由**不注册**（404，行为与 v1.1 一致）。已注册但运行期缺配置时写操作返回 50000 并说明缺什么：`fleet.model_cache_dir`、`fleet.model_pull_image`（须 `ref@sha256:<64hex>`）、`fleet.engine_port_range`（默认 `41000-41999`）；日志接口另需 Redis。

所有接口先校验订单归属：订单号格式非法 40001；订单不存在**或不属于当前用户**统一 40400；`deploy_no` 不属于该订单 40400。

### 部署对象 `DeploymentView`

`GET` 详情、`POST` 提交/停止/重启均返回此结构（列表项不含 `events`；运营列表额外带 `buyer_id`、`node_id`）：

```json
{
  "deploy_no": "d-wynew4in",
  "order_no": "TF20261031223030ABCD",
  "model": {"id": 3, "model_key": "qwen2.5-7b-instruct", "display_name": "Qwen2.5 7B Instruct"},
  "state": "running",
  "health": "degraded",
  "health_reason": "capacity_saturated",
  "health_reason_text": "请求排队且 KV 缓存已满，建议降低并发、缩短上下文或增加卡数",
  "tp": 2,
  "dp": 1,
  "max_model_len": 32768,
  "gpu_mem_util": 0.9,
  "gpu_uuids": ["GPU-6f1c…", "GPU-9a02…"],
  "canary_enabled": true,
  "engine_cpus": 63,
  "engine_memory_gb": 508,
  "access": {
    "base_url": "http://omnis-engine:8000/v1",
    "served_model": "qwen2.5-7b-instruct",
    "port_forward_cmd": "ssh -p 2222 -N -L 8000:omnis-engine:8000 l-7f3k9q2a@gw.omnisline.com"
  },
  "cache": null,
  "restart_count": 1,
  "last_restart_at": "2026-10-31T21:02:11+08:00",
  "next_restart_at": null,
  "fail_reason": null,
  "stop_reason": null,
  "started_at": "2026-10-31T20:41:05+08:00",
  "stopped_at": null,
  "events": [
    {"kind": "auto_restart", "reason": "container_down", "detail": null, "created_at": "2026-10-31T21:02:11.412+08:00"},
    {"kind": "running", "reason": null, "detail": null, "created_at": "2026-10-31T20:41:05.000+08:00"}
  ],
  "created_at": "2026-10-31T20:30:00+08:00",
  "updated_at": "2026-10-31T21:05:00+08:00"
}
```

| 字段 | 说明 |
|---|---|
| `state` | `pending/checking/pulling/starting/running/restarting/stopping/stopped/failed`（§27.6） |
| `health` | `unknown/healthy/degraded/unhealthy`；只在 `running/restarting` 有意义 |
| `health_reason` | `container_down` `oom_killed` `health_failed` `canary_failed` `gpu_fatal`（unhealthy）；`capacity_saturated` `preempting` `ttft_slow` `gpu_hot` `ecc_corrected_rising` `recently_restarted`（degraded）；`node_unreachable`（unknown）。`health_reason_text` 为中文说明 |
| `access` | 仅 `starting/running/restarting` 非 null。`base_url` 在租约容器内可用；`port_forward_cmd` 仅平台配置了网关地址时给出（否则省略） |
| `cache` | 仅 `pulling` 时非 null：`{"state":"downloading","bytes_done":8053063680,"weights_bytes":16000000000}`；`state` 取 `downloading/ready/failed/evicting` |
| `engine_cpus/engine_memory_gb` | 引擎容器配额（租约配额对半，见 docs/27 "S5 实现口径"），启动前为 null |
| `next_restart_at` | 已排定的自动重启时间（退避中）；为 null 表示没有等待中的重启 |
| `fail_reason` | `failed`（以及失败清理中的 `stopping`）时的原因；引擎日志末尾经 logs 接口 `source=stored` 读取 |
| `stop_reason` | `buyer` `lease_expired` `lease_frozen` `lease_purged`（`nightly` 预留，未启用） |
| `events` | 最近 20 条，新→旧；`kind`：`auto_restart` `manual_restart` `resume` `stop` `fail` `running` `degraded_notice` `unhealthy_notice` |

### POST /orders/:id/deployments · 提交部署

```json
{"model_id": 3, "tp": 2, "dp": 1, "max_model_len": 32768, "gpu_mem_util": 0.90,
 "canary_enabled": true, "license_ack": true, "untested_ack": false}
```

| 字段 | 规则 |
|---|---|
| `model_id/tp/dp` | 必填正整数；`tp×dp` 组合须在服务端**重新**判定出的 `plans` 内 |
| `max_model_len` | 可省略/0 = 取所选方案上限；否则须在 `[min_context_tokens, 方案上限]` |
| `gpu_mem_util` | 数字或字符串，`0.80–0.95`、最多两位小数；省略/null = 0.90 |
| `canary_enabled` | 省略 = true |
| `license_ack/untested_ack` | F7/F8 需要确认时必须为 true |

成功返回 `DeploymentView`（`state=pending`），同事务入队 `deploy_model`。错误：

| code | 场景 | `data` |
|---|---|---|
| 40001 | 参数非法；判定不通过 | `{"checks":[{id,ok,level,message}]}` |
| 40001 | F7/F8 未确认 | `{"checks":[未确认的项]}` |
| 40001 | `tp/dp` 不在可行方案内 | `{"plans":[…]}` |
| 40001 | 非托管订单；整机(bare_metal)租约不支持一键部署 | — |
| 40400 | 订单不存在/非本人；模型不存在或未上线 | — |
| 40900 | 已有未终止部署 | `{"deploy_no":"d-…","state":"running"}`（并发提交被唯一键拦下时无 data） |
| 40900 | 租约非 `active`；租约可用（非故障）卡数不足 | — |
| 50000 | 运行期未接入/缺配置（见上） | — |

### GET /orders/:id/deployments · 部署历史

```json
{"list": [DeploymentView…], "total": 2}
```
新→旧，不含 `events`，不分页（单租约部署数有限）。

### GET /orders/:id/deployments/:deploy_no · 详情

返回 `DeploymentView`（含 `events`）。

### GET /orders/:id/deployments/:deploy_no/health?range=1h|24h|7d · 健康曲线

`range` 缺省 `1h`；其他值 40001。1h = 60 秒原始点；24h = 300 秒桶；7d = 3600 秒桶。

```json
{
  "deploy_no": "d-wynew4in",
  "range": "24h",
  "bucket_seconds": 300,
  "health": "healthy",
  "health_reason": null,
  "health_reason_text": null,
  "baseline_canary_ms": 148,
  "points": [
    {"t": "2026-10-31T20:40:00+08:00", "container_up": 1, "health_ok": 1, "models_ok": 1, "canary_ok": 0.8,
     "canary_ms": 152, "requests_running": 3.4, "requests_waiting": 0, "kv_usage_pct": 41.25,
     "ttft_p50_ms": 180, "ttft_p95_ms": 420, "gen_tokens_per_s": 512.3, "prompt_tokens_per_s": 2210.5,
     "preemptions": 0, "gpu_temp_max_c": 71, "throttled": 0, "health": "healthy"}
  ]
}
```

降采样口径：布尔列（`container_up/health_ok/models_ok/canary_ok`）取桶内比例 0–1；`canary_ms/ttft_p50_ms` 取平均取整；`requests_*`、`kv_usage_pct`、吞吐取平均（两位小数）；`ttft_p95_ms`、`gpu_temp_max_c`、`throttled` 取桶内最大；`preemptions` 取桶内增量之和；`health` 取桶内最差。原始点（1h）各列为 0/1 或实测值。所有数值列在未采集时为 null（如 L4 指标拉取失败、卡遥测超过 3 分钟未更新、金丝雀关闭）。

### GET /orders/:id/deployments/:deploy_no/logs?tail=200 · 引擎日志

`tail` 缺省 200，范围 1–1000（否则 40001）。每买家每部署每分钟 6 次（Redis 固定窗口），超出 42900。每次调用写审计 `audit_logs.action='model_deploy.view_logs'`（`target_type=model_deployment`，`after_value={"deploy_no","tail"}`，含 IP）。

```json
{"deploy_no": "d-wynew4in", "source": "live", "lines": ["INFO 10-31 20:41:02 api_server.py:… Started server process", "…"]}
```

`source=live`：部署处于 `starting/running/restarting` 时经管理通道 `docker logs --tail`；`source=stored`：节点不可达、容器已不存在或部署已结束时返回失败时留存的日志末尾（可能为空数组）。

### POST /orders/:id/deployments/:deploy_no/stop · 停止

返回 `DeploymentView`。可停止的状态 → `stopping`（`stop_reason=buyer`）并入队 `stop_model`（删除引擎容器、归还租约容器完整配额后 → `stopped`）。已是 `stopping/stopped/failed` 时直接返回当前状态（幂等）。

### POST /orders/:id/deployments/:deploy_no/restart · 重启

返回 `DeploymentView`。

| 当前状态 | 行为 |
|---|---|
| `running` | → `restarting`（`manual_restart` 事件，`restart_count+1`），入队 `deploy_model` 重建引擎 |
| `restarting` 且在退避等待 | 跳过等待立即重启 |
| `stopped/failed` | 恢复：→ `pending`，重新走 checking/pulling/starting（`resume` 事件）；租约须 `active`，否则 40900；该租约已有其它未终止部署 40900；租约已被重新分配节点或 GPU（宽限期后 restart 续租 / 开通失败改配），原部署绑定的卡不再属于本租约 → 40900，须重新提交部署 |
| `pending/checking/pulling/starting/restarting`（进行中） | 幂等，返回当前状态 |
| `stopping` | 40900 |

手动重启不计入"1 小时 3 次"自动重启上限。

## 部署（运营，operator/admin）✅

### GET /admin/deployments?state=&health=&page=&page_size= · 全局查看

`state/health` 可选，取值同上，非法 40001；`page_size` 1–100（缺省 20）。按 id 新→旧。

```json
{"list": [DeploymentView（含 buyer_id、node_id，不含 events）…], "total": 37, "page": 1, "page_size": 20}
```

---

## 数据迁移

`backend/migrations/030_model_deployment.up.sql`：`gpu_catalog.compute_capability`、`supplier_nodes.model_cache_gb`，新建 `model_catalog`、`model_catalog_verifications`、`node_model_cache`、`model_deployments`、`deployment_health_samples`（结构同 docs/27 §27.7）。

`backend/migrations/031_probe_telemetry.up.sql`（探活遥测，F3–F6 数据源）：`gpu_slots` 加逐卡遥测列（`vram_used_mb`/`vram_total_mb`/`temp_c`/`power_w`/`throttle_reasons`/`ecc_*`/`telemetry_at`）与两两拓扑（`topo_links`/`topo_at`）；`supplier_nodes` 加 Xid 覆盖区间（`xid_scan_ok_at`/`xid_covered_from`/`xid_scan_note`）与缓存目录容量（`cache_free_bytes`/`cache_total_bytes`/`cache_observed_at`/`cache_note`/`cache_source`）；新建 `gpu_xid_events`。down 为直接删除：全部是可由下一轮探活重新采集的遥测，不含业务数据（停机认定用 `leases.resource_alert` 与 T-105 自己的表）。

`backend/migrations/035_model_deploy_runtime.up.sql`（T-098/T-099 运行期）：`model_deployments` 追加 `state_changed_at`、`next_restart_at`、`engine_cpus/engine_memory_gb`、`lease_cpus/lease_memory_gb`（租约**完整**配额，停止时据此归还）、`monitor_state`（跨轮判定状态 JSON）、`stop_reason` 与 `idx_order/idx_lease/idx_node_model`；`node_model_cache` 追加 `pull_container/pull_started_at`；新建 `deployment_events`。

`030_model_deployment.down.sql` 无条件 `SIGNAL SQLSTATE '45000'`：部署记录、真机验证记录与健康采样属于业务数据，回退须经审计归档，禁止自动删除。
