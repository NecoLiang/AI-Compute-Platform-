# 托管零租 Managed Rental API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 供给方接口需 supplier 角色（只能操作自己商品下的节点）；运营接口需 operator/admin
**设计**：`docs/25`（§25.4 节点接入、§25.8.1 节点状态机、§25.10 API 摘要）。本文件为正式接口契约。

> **当前范围（v1.2 S1，T-076~T-081）**：节点接入、探活、卡槽、运营停用/恢复与任务重试。
> 租约、买家连接、访问网关（S2，T-082 起）的接口由后续阶段追加到本文件，**此处尚未提供**。

**开关**：所有接口仅在 `fleet.enabled=true` 时注册。关闭时（默认）这些路径未注册（HTTP 404），`POST /supplier/nodes` 传 `mgmt_mode:"ssh"` 返回 40001「托管履约未启用(fleet.enabled=false)」，托管商品不可提交审核与下单（40001），人工模式行为不变。

**通用口径**
- 信封 `{code,message,data,request_id}`；错误码只用 40001 / 40100 / 40101 / 40300 / 40400 / 40900 / 42900 / 50000。
- 他人节点、不存在的节点统一 40400「节点不存在或不属于当前供应方」（不泄露存在性）。
- 外部依赖缺失（`security.credential_key`、`accessgw.egress_ips` 等）返回 50000 并写明缺什么，不降级、不伪造成功。
- 节点 JSON 不含任何凭证：管理私钥、公钥原文、`node_key_hash`、`host_fp_claim` 均不出接口。一次性引导密码不落库、不进日志、不进审计。
- 时间为 ISO8601（Asia/Shanghai）；可空为 `null`。

### 节点状态 `verify_status`

| 值 | 含义 | 供给方可做 |
|---|---|---|
| `unverified` | 新建 / 修改了 SSH 信息 / 首次连接待确认指纹 / 运营恢复后 | 填 SSH、发起探活、确认指纹 |
| `verifying` | 完整探活排队或执行中 | 等待（重复发起返回同一任务） |
| `verified` | 探活通过，生成卡槽，计入可售 | 卡槽停用、排空 |
| `failed` | 完整探活未通过（节点同时置 `status=offline`） | 修复后重新发起探活 |
| `key_mismatch` | SSH 主机指纹与已确认值不一致，停止新分配 | 核对后确认新指纹（自动继续探活） |
| `needs_reimage` | 整机租约结束待重装（S2 写入） | 重装后调用重装确认 |
| `suspended` | 运营停用 | 无（只读） |

`schedulable`（是否接受新分配）独立于上表，由排空/恢复切换。`status`（online/offline）由 fleet 的 SSH 轻量探活判定，scheduler 的心跳清扫不处理 ssh 节点。

### 卡槽状态 `gpu_slots.state`

`free` 可售 / `withheld` 供给方停用 / `faulty` 健康或压测不通过 / `reserved`、`leased`、`draining`（S2 租约使用）。
商品派生库存 = `verified` 且 `schedulable=1` 且 `status=online` 的节点上 `free` 卡槽数；库存为 0 时托管商品 `active→sold_out`，恢复后 `sold_out→active`（`draft`/审核中/下架状态不受影响）。

---

## POST /supplier/nodes · 添加托管节点 ✅

沿用 docs/23 的注册接口；传 `mgmt_mode:"ssh"` 即为托管节点。商品必须是本人的 `fulfillment_mode=managed` 商品。

```
curl -X POST http://localhost:8080/api/v1/supplier/nodes \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"product_id":12,"node_name":"bj-h100-01","total_cards":8,"mgmt_mode":"ssh",
       "disk_per_card_gb":200,"lease_port_start":40000,"lease_port_end":40099}'
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `product_id` | 是 | 本人托管商品 |
| `node_name` | 是 | ≤ 64 字，同商品下唯一（重复 40900） |
| `total_cards` | 是 | 1–64；探活 P5 核对 |
| `mgmt_mode` | 是 | `"ssh"`；缺省/`"push"` 走原心跳节点逻辑（托管商品拒绝 push 节点 40001） |
| `disk_per_card_gb` | 分卡容器商品必填 | 每卡数据盘 GB，探活 P8 核对 |
| `lease_port_start/end` | 否 | 默认 40000–40099；须在 1024–65535、跨度 ≤ 1000、端口数 ≥ 卡数+1（含探活自检端口） |

**响应**：`{"code":0,"data":{"node":{Node}}}`。平台为该节点生成专属 ed25519 管理密钥（私钥 AES-256-GCM 加密入库），不返回 `node_key`。
**错误**：40001 参数 / 商品非托管；40400 商品不存在或非本人；40900 同名；50000 `security.credential_key` 未配置。

`Node` 字段：`id, supplier_id, product_id, node_name, status, total_cards, available_cards, last_heartbeat_at, mgmt_mode, ssh_host, ssh_port, ssh_user, host_key_fingerprint, host_key_confirmed_at, verify_status, schedulable, verified_at, last_probe_ok_at, probe_fail_count, cpu_cores, mem_gb, disk_free_gb, disk_per_card_gb, disk_quota_supported, driver_version, cuda_version, lease_port_start, lease_port_end, created_at, updated_at`。

## GET /supplier/nodes · 我的节点 ✅ / DELETE /supplier/nodes/:id · 删除 ✅

列表沿用 docs/23，托管节点带 `mgmt_mode:"ssh"`、`verify_status`、`schedulable`。
删除托管节点：存在 `reserved/leased/draining` 卡槽或绑定租约的卡槽时 40900「节点上有进行中的租约」；否则同一事务内删除卡槽、作废排队与执行中的任务、释放指纹占用、删除节点并重算商品库存。管理密钥随节点作废。

## GET /supplier/nodes/:id/onboarding · 上架说明 ✅

```json
{"code":0,"data":{
  "node_id":7,"verify_status":"unverified",
  "authorized_keys_line":"from=\"198.51.100.10,198.51.100.11\",no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-pty ssh-ed25519 AAAA... omnis-node-7",
  "controller_egress_ips":["198.51.100.10","198.51.100.11"],
  "gateway_egress_ips":["192.0.2.20"],
  "lease_port_start":40000,"lease_port_end":40099,
  "ssh_host":null,"ssh_port":null,"ssh_user":null,
  "host_key":{"fingerprint":null,"confirmed":false,"confirmed_at":null,
              "check_command":"ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"},
  "firewall_scripts":{"debian":"#!/bin/sh\nset -e\n...","rhel":"#!/bin/sh\nset -e\n..."},
  "requirements":["Linux x86_64 + systemd; NVIDIA 驱动 ≥ 535.104", "..."]
}}
```

- `authorized_keys_line` 供给方追加到管理账户 `~/.ssh/authorized_keys`（推荐方式，无需提交密码）。`from=` 限定为控制面出口 IP（`fleet.controller_egress_ips`，只接受单个 IP，不接受 CIDR）。
- `firewall_scripts` 在 `DOCKER-USER` 链按 `conntrack --ctorigdstport` 只放行网关出口 IP 访问租约端口段，幂等（先删后加），附发行版持久化命令。
- 50000：`accessgw.egress_ips` 未配置（无法生成放行规则）或节点缺少管理公钥。

## PUT /supplier/nodes/:id/ssh · 连接信息 / 一次性引导 ✅

```
curl -X PUT .../supplier/nodes/7/ssh -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"host":"203.0.113.10","port":22,"user":"root","bootstrap_password":"<仅本次使用>"}'
```

- 保存后节点回到 `unverified`，作废排队中的探活任务；`host` 或 `port` 变化时清除指纹钉死（需重新确认）。`verified` 节点修改后立即退出可售。
- `host`：IP 或域名，拒绝回环 / 链路本地 / 组播 / 未指定地址及 `localhost`（40001）；`user`：小写字母数字下划线短横线，≤32 位。
- `bootstrap_password`（可选）：平台用它登录**一次**，幂等写入管理公钥行后丢弃；不重试。失败时连接信息已保存，返回 40001「一次性密码登录安装公钥失败: …」，可改为手动添加公钥行。若该主机已有钉死指纹且不一致，返回 40900 并转 `key_mismatch`。
- `suspended` 节点 40900。

**响应**：`{"node":{Node},"bootstrap":"skipped"|"installed","host_key":{fingerprint,confirmed,confirmed_at,check_command}}`。引导成功时 `host_key.fingerprint` 为本次观测到的指纹（未确认）。

## POST /supplier/nodes/:id/verify · 发起完整探活 ✅

**响应**：`{"task_id":123}`。节点 → `verifying`；已有排队/执行中的探活时返回同一 `task_id`。
**错误**：40001 未填 SSH 信息；40900 `suspended` / `key_mismatch`（先确认新指纹）/ `needs_reimage`（先重装确认）。

首次连接（指纹未确认）时探活只执行 P1–P3：记录观测指纹，写一份 `fail` 报告（P2 详情给出指纹与核对命令），节点回 `unverified`，站内信「请确认节点主机指纹」；**不执行任何 nvidia-smi / docker 命令**。

## POST /supplier/nodes/:id/host-key/confirm · 确认主机指纹 ✅

`{"fingerprint":"SHA256:..."}` → `{"confirmed":true,"task_id":124}`
- 与平台观测值一致才钉死，并自动入队完整探活（`task_id` 为 0 表示 SSH 信息不全未入队）。
- 40001 指纹不一致（返回平台观测值与核对命令）；40900 尚未观测到指纹 / `suspended`。
- 从 `key_mismatch` 确认新指纹写审计 `node.host_key_reconfirmed`。

## GET /supplier/nodes/:id/probe-reports?limit= · 探活报告 ✅

`limit` 默认 10、最大 50，按时间倒序。

```json
[{"id":31,"node_id":7,"kind":"verify","result":"pass","created_at":"...",
  "checks":[{"id":"P1","name":"TCP 可达","ok":true,"required":true,"detail":"203.0.113.10:22 可达"},
            {"id":"P10","name":"磁盘配额能力","ok":false,"required":false,"detail":"--storage-opt size= 不可用 ..."},
            {"id":"P13","name":"短压测","ok":true,"required":true,"detail":"压测 8 张空闲卡 60s, 8 张通过; ..."}],
  "inventory":{"cpu_cores":224,"mem_gb":2015,"disk_free_gb":12946,"disk_path":"/data","disk_quota_supported":true,
               "driver_version":"535.104.05","cuda_version":"12.2","os":"Ubuntu 22.04.4 LTS","kernel":"5.15.0-113-generic",
               "docker_version":"24.0.7","gpus":[{"index":0,"uuid":"GPU-...","name":"NVIDIA H100 80GB HBM3",
               "memory_mib":81559,"pci_bus_id":"00000000:18:00.0","driver_version":"535.104.05"}],
               "topo_groups":{"0":"g0-nvlink"},"faulty":{"3":"实测 300.5 TFLOPS 低于基准 ..."}}}]
```

- `kind`：`verify`（完整探活）/ `port_recheck`（端口复测）。`result=pass` 表示全部必检项通过（P10 为非必检）。
- `checks[].skipped=true` 表示前置项未通过未执行。`inventory` 在连接阶段失败或待确认指纹时为 `null`。

## GET /supplier/nodes/:id/slots · 卡槽 ✅

`[{"id":1,"node_id":7,"gpu_index":0,"gpu_uuid":"GPU-...","pci_bus_id":"...","vram_mb":81559,"topo_group":"g0-nvlink","state":"free","lease_id":null,"state_changed_at":"..."}]`
卡槽在探活通过后生成。`lease_id` 只给租约 ID，不含买家身份。

## PUT /supplier/nodes/:id/slots/:gpu_index · 停用/启用单卡 ✅

`{"state":"withheld"|"free"}` → `{"gpu_index":3,"state":"withheld"}`；仅 `free↔withheld`，写审计并重算库存。
40001 `state` 取其他值；40400 节点或卡槽不存在；40900 卡槽当前为在租/预留/故障等其他状态。

## POST /supplier/nodes/:id/drain · 排空 ✅ / POST /supplier/nodes/:id/undrain · 恢复接单 ✅

切换 `schedulable`（重复调用幂等），存量租约不受影响；写审计并重算库存。响应 `{"schedulable":false|true}`。

## POST /supplier/nodes/:id/reimage-confirm · 重装确认 ✅

仅 `needs_reimage` 可调（否则 40900）。清除指纹钉死后入队完整探活：新系统首次连接会再次进入「确认主机指纹」。响应 `{"task_id":125}`。

---

## 运营接口

### GET /admin/nodes · 节点总览（扩展）✅

沿用 docs/23（见 [scheduler-api.md](scheduler-api.md)）。托管节点额外返回：`mgmt_mode`、`verify_status`、`schedulable` 与
`managed:{"last_probe":{"id","kind","result","failed_checks":[],"created_at"}|null,"slots":{"free":6,"faulty":1,"withheld":1},"ssh_host","ssh_port","host_key_fingerprint","host_key_confirmed"}`。

### POST /admin/nodes/:id/suspend · 停用节点 ✅

`{"reason":"≤200 字，可选"}` → `{"verify_status":"suspended"}`。停止新分配、释放空闲/停用/无租约故障卡槽（卡 UUID 与指纹占用随之释放）、作废排队任务、重算库存、通知供给方、写审计。存量租约不受影响。40400 非托管节点；40900 已停用。

### POST /admin/nodes/:id/resume · 恢复节点 ✅

→ `{"verify_status":"unverified","task_id":126}`。恢复后自动入队完整探活，**通过后才回到 `verified`**（停用期间卡槽已释放，须重新核验）。40900 节点不在停用状态。

### GET /admin/node-tasks?state=&node_id=&page=&page_size= · 节点任务 ✅

`state` ∈ `pending/running/succeeded/failed/dead`（其他值 40001），缺省为全部。分页信封 `data:{list,total,page,page_size}`。
`Task`：`id, node_id, lease_id, deployment_id, kind, state, priority, attempts, max_attempts, run_after, locked_until, payload, result, last_error, created_at, updated_at`；`payload/result` 为 JSON（未完成任务 `result` 为 `null`）。探活任务 `result`：`{"mode":"full","pass":true,"report_id":31,"failed_checks":[],"awaiting_host_key":"SHA256:...","discarded":false}`。

### POST /admin/node-tasks/:id/retry · 重试死信 ✅

仅 `dead` 可重试（否则 40900），次数清零回 `pending`，写审计 `node_task.retry`。40400 任务不存在。响应 `{"task_id":9,"state":"pending"}`。

---

## 探活检查项与判定（实现口径）

| # | 检查 | 实现要点 |
|---|---|---|
| P1 | TCP 可达 | 3 次，单次 5s |
| P2 | SSH 握手与指纹 | 已钉死则比对，不一致 → `key_mismatch`；未钉死 → 记录观测值并在 P3 后停止 |
| P3 | 管理密钥认证 | 平台私钥登录 |
| P4 | GPU 清单 | `nvidia-smi --query-gpu=...`；卡 UUID 已被其他节点登记 → 不通过 |
| P5 | 声明一致性 | 型号按 `gpu_catalog` 归一、卡数、显存误差 ≤ 3%（GDDR 卡开 ECC 时按可见显存少 1/16 再比） |
| P6 | 拓扑 | `nvidia-smi topo -m` → `topo_group` |
| P7 | 健康 | 不可纠正 ECC、退役页/行重映射待生效或失败、24h 致命 Xid；问题卡标 `faulty`；**全部卡故障才判不通过** |
| P8 | 主机资源 | CPU/内存/Docker 数据目录所在盘/OS/内核 + 驱动 ≥ `fleet.min_driver_version` |
| P9 | 容器运行时 | Docker ≥ 24；拉取 `fleet.probe_image`；`docker run --gpus "device=<空闲卡>"` 可见卡数一致 |
| P10 | 磁盘配额（非必检） | `--storage-opt size=1G`；不支持则只能整机出租 |
| P11 | 租约端口：网关可达 | 端口段内**从末尾向下**选空闲端口起临时 TCP 监听容器，由访问网关做连通自检。**网关（S2）未接入时判不通过** |
| P12 | 租约端口：未裸露 | 同一端口从主服务出口直连，必须连不上 |
| P13 | 短压测 | 空闲卡逐卡 FP16 矩阵乘（默认 60s）+ 降频采样 + 压测后复扫 Xid/ECC；低于 `gpu_catalog.probe_baseline_tflops × fleet.probe.min_perf_ratio`、降频、掉卡的卡标 `faulty`；**全部卡不通过才判不通过**；无空闲卡时通过并注明 |

在租/预留/排空的卡只做只读检查（P4/P7），侵入项只用空闲卡与未分配端口。

**结论落库**：全部必检项通过 → `verified`、`status=online`、同步卡槽（健康卡 `free`、问题卡 `faulty`，此前 faulty 的卡恢复健康后回 `free`）、重算库存。任一必检项不通过 → `failed` + `status=offline`，库存扣除。周期补跑的完整探活若只是连不上（P1–P3）则只写报告，在线状态交给轻量探活判定。探活期间 SSH 信息被修改或节点被停用 → 结论作废（任务 `result.discarded=true`），不改节点状态。

**周期**：`verified` 节点每 60s SSH 轻量探活（`nvidia-smi` 利用率/温度/ECC + Docker 守护进程 + 近 2 分钟 Xid），成功写心跳与 `available_cards`；距上次成功超过 90s → `offline` + 通知，恢复后自动上线。空闲卡掉卡或出现致命错误 → `faulty` + 通知；在租卡异常仅记运营告警（租约侧处理在 S2）。完整探活每 12h、端口复测每 24h 自动入队。
