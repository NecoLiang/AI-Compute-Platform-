# 托管零租 Managed Rental API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 供给方接口需 supplier 角色（只能操作自己商品下的节点）；运营接口需 operator/admin
**设计**：`docs/25`（§25.4 节点接入、§25.8.1 节点状态机、§25.10 API 摘要）。本文件为正式接口契约。

> **当前范围**：v1.2 S1（T-076~T-081）节点接入、探活、卡槽、运营停用/恢复与任务重试；S2（T-085）访问网关内部接口（见文末「访问网关内部接口」）；S2（T-086）买家公钥、连接信息、会话记录（「买家连接」）与运营租约/会话检索（「运营接口」）。
> 托管订单的下单、开通、取消退款口径见 [compute-api.md](compute-api.md)「托管订单（S2）」与 [payment-api.md](payment-api.md)「托管订单（S2）」。续租、冻结、到期清理（T-087/T-088）尚未接入。

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

- 保存后节点回到 `unverified`，作废排队中与进行中的探活任务（进行中的探活结论按节点代次作废，不会改动节点状态）；`host` 或 `port` 变化时清除指纹钉死（需重新确认）。`verified` 节点修改后立即退出可售。
- `host`：节点的**公网** IP 或域名。平台按目标地址拒绝名单校验（回环、RFC1918、CGNAT `100.64.0.0/10`（含云元数据 `100.100.100.200`）、链路本地（含 `169.254.169.254`）、ULA `fc00::/7`、组播、保留/文档段，以及运营配置的 `fleet.deny_cidrs`）：IP 直接判断，域名在提交时解析，任一解析结果命中即拒绝 → 40001「host 不允许: 须为节点的公网地址…」；域名无法解析 → 40001「host 无法解析…」；`localhost` 40001。拒绝提示不回显解析出的地址。之后每次连接还会在拨号时按实际地址再校验一次（防 DNS rebinding）。`user`：小写字母数字下划线短横线，≤32 位。
- `bootstrap_password`（可选）：平台用它登录**一次**，幂等写入管理公钥行后丢弃；不重试。失败时连接信息已保存，返回 40001「一次性密码登录安装公钥失败: <原因>…」，原因只取「管理端口不可达」/「SSH 握手失败」/「SSH 登录失败(用户名或密码错误…)」/「写入 authorized_keys 失败(…)」，底层错误只写服务端日志；可改为手动添加公钥行。若该主机已有钉死指纹且不一致，返回 40900 并转 `key_mismatch`。
- `suspended` 节点 40900。

**响应**：`{"node":{Node},"bootstrap":"skipped"|"installed","host_key":{fingerprint,confirmed,confirmed_at,check_command}}`。引导成功时 `host_key.fingerprint` 为本次观测到的指纹（未确认）。

## POST /supplier/nodes/:id/verify · 发起完整探活 ✅

**响应**：`{"task_id":123}`。节点 → `verifying`；已有本代次排队/执行中的**完整**探活时返回同一 `task_id`（排队中的端口复测不能代替完整探活：会被作废并另行入队完整探活；在跑的端口复测结束后再执行完整探活）。
完整探活若因平台侧原因无法完成（平台配置缺失、重试耗尽等），节点 `verifying → failed` 并收到站内信「节点探活未能完成」，可再次发起。
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
- 连接类失败的 `detail` 为通用提示：P1「管理端口不可达」、P2「SSH 握手失败」、P11「经网关连接 host:port 失败(检查是否已按上架说明放行网关出口 IP)」；不包含解析出的地址或底层错误串（只写服务端日志）。
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

仅 `needs_reimage` 可调（否则 40900）。节点 → `verifying`，清除指纹钉死后入队完整探活（重装前已在跑的探活结论作废）：新系统首次连接会再次进入「确认主机指纹」。响应 `{"task_id":125}`。

---

## 买家连接（S2 T-086）✅

登录即可调用（buyer 组，任何角色），归属在服务层校验。仅 `fleet.enabled=true` 时注册。
买家通过访问网关 `ssh -p <gateway_port> <login>@<gateway_host>` 登录：公钥（本节管理）或访问凭证（密码，沿用 compute 的 `GET /orders/:id/access-credential` 与 `POST /orders/:id/access-credential/reveal`）。

### GET /user/ssh-keys · 我的公钥 ✅

只返回未删除的公钥，按添加时间倒序。

```json
{"code":0,"message":"success","data":[
  {"id":3,"name":"alice@laptop","key_type":"ssh-ed25519",
   "public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...",
   "fingerprint":"SHA256:9oQf0m4vJ4bXo3m2pY0Yt2bKq9Hk7cY2l7XvN1yq5jE",
   "created_at":"2026-10-28T10:00:00+08:00"}],
 "request_id":"..."}
```

### POST /user/ssh-keys · 添加公钥 ✅

```
curl -X POST http://localhost:8080/api/v1/user/ssh-keys \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"办公笔记本","public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... alice@laptop"}'
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `public_key` | 是 | `.pub` 文件的**一行**。支持 `ssh-ed25519`、`ecdsa-sha2-nistp256/384/521`、`ssh-rsa`（≥ 3072 位）。拒绝：带选项的行（`from=`/`command=` 等）、多行/多把、证书（`*-cert-v01@openssh.com`）、安全密钥（`sk-*`）、`ssh-dss`、RSA < 3072 位 |
| `name` | 否 | ≤ 64 字（超长 40001）；缺省取公钥注释（截断到 64 字），无注释为「SSH 公钥」 |

**响应**：`data` 为一条 `BuyerKey`（同上）。入库的 `public_key` 是解析后重新序列化的规范形式（**不含注释**），`fingerprint` 为 `SHA256:` 格式。
**规则**：每个账户最多 10 把未删除的公钥（第 11 把 40900）；同一把已添加 40900「该公钥已添加」；曾删除的同一把重新添加时复用原记录（`id` 不变，`created_at` 刷新）。同一把公钥可被不同账户各自添加。写审计 `buyer_ssh_key.add`。
**错误**：40001 `public_key` 缺失 / 格式或类型不支持 / `name` 超长；40900 重复 / 已达上限。

### DELETE /user/ssh-keys/:id · 删除公钥 ✅

→ `{"code":0,"message":"success","data":{"id":3,"deleted":true},"request_id":"..."}`
- 软删除（`revoked_at`），同一事务内刷新本人 `active/parked/suspended/grace` 租约的 `access_changed_at`：网关在下一次撤销轮询（≤ 5s 轮询 + 2s 滞后）清掉按该公钥缓存的放行，之后用该公钥登录被拒。
- **已建立的 SSH 会话不会被断开**（sshpiperd v1.6.1 插件无法切断已建立的管道，见 docs/25 §25.6.3 S2 实现口径）。
- 写审计 `buyer_ssh_key.revoke`。40001 `id` 非法；40400 不存在 / 非本人 / 已删除。

### GET /orders/:id/access · 托管订单连接信息 ✅

`:id` 为订单号（与 compute 订单接口一致）。

```json
{"code":0,"message":"success","data":{
  "gateway_host":"gw.omnisline.com","gateway_port":2222,
  "login":"l-4g7bq4xc","lease_state":"active",
  "access_until":"2026-11-28T10:00:00+08:00","grace_until":null,
  "auth_methods":["publickey","password"]},
 "request_id":"..."}
```

- `gateway_host/port` 来自 `accessgw.public_host/public_port`；`login` 即租约号，换节点/续租不变。**不返回节点地址与容器端口。**
- 能否登录以 `lease_state=active` 且未过 `access_until` 为准；其他状态（`pending/provisioning/parked/suspended/grace/...`）照常返回本结构，前端据 `lease_state` 提示。
- **错误**（按此顺序判定）：40400 订单不存在；40300 非本人订单；40400 人工交付订单「该订单为人工交付, 没有网关连接信息」；40400 订单尚无租约。

### GET /orders/:id/sessions?page=&page_size= · 会话记录 ✅

仅本人托管订单（错误同上）。分页 `page` 默认 1，`page_size` 默认 20、> 100 回落为 20；按 `started_at` 倒序。会话记录保留 180 天。

```json
{"code":0,"message":"success","data":{"list":[
  {"id":12,"lease_no":"l-4g7bq4xc","remote_ip":"198.51.100.1","auth_method":"publickey",
   "key_fingerprint":"SHA256:9oQf0m4vJ4bXo3m2pY0Yt2bKq9Hk7cY2l7XvN1yq5jE","key_name":"办公笔记本",
   "started_at":"2026-10-28T08:00:00+08:00","ended_at":"2026-10-28T09:00:00+08:00",
   "bytes_up":0,"bytes_down":0,"end_reason":"closed"},
  {"id":13,"lease_no":"l-4g7bq4xc","remote_ip":"2001:db8::1","auth_method":"password",
   "key_fingerprint":null,"key_name":null,
   "started_at":"2026-10-28T09:50:00+08:00","ended_at":null,
   "bytes_up":0,"bytes_down":0,"end_reason":null}],
  "total":2,"page":1,"page_size":20},
 "request_id":"..."}
```

- `key_name`：本人登记该指纹时的名称（公钥删除后仍显示，便于核对）；密码登录或指纹未在本人名下登记为 `null`。
- `ended_at=null` 表示会话进行中（或网关尚未上报结束）。`bytes_up/bytes_down` 在 S2 恒为 0（sshpiperd v1.6.1 插件拿不到字节数）。系统自检会话不记录。

---

## 运营接口

### GET /admin/nodes · 节点总览（扩展）✅

沿用 docs/23（见 [scheduler-api.md](scheduler-api.md)）。托管节点额外返回：`mgmt_mode`、`verify_status`、`schedulable` 与
`managed:{"last_probe":{"id","kind","result","failed_checks":[],"created_at"}|null,"slots":{"free":6,"faulty":1,"withheld":1},"ssh_host","ssh_port","host_key_fingerprint","host_key_confirmed"}`。

### POST /admin/nodes/:id/suspend · 停用节点 ✅

`{"reason":"≤200 字，可选"}` → `{"verify_status":"suspended"}`。停止新分配、释放空闲/停用/无租约故障卡槽（卡 UUID 与指纹占用随之释放）、作废排队任务、重算库存、通知供给方、写审计。作废该节点**排队中与进行中**的探活任务（任务 `failed`；进行中的探活即使随后通过也不会把节点改回 `verified`，`result.discarded=true`）；租约类任务（stop/purge/kill_sessions/provision）不作废，存量租约不受影响，排队中的开通在执行时发现节点已停用会走重试→换节点/退款。40400 非托管节点；40900 已停用。

### POST /admin/nodes/:id/resume · 恢复节点 ✅

→ `{"verify_status":"unverified","task_id":126}`。恢复后自动入队完整探活，**通过后才回到 `verified`**（停用期间卡槽已释放，须重新核验）。40900 节点不在停用状态。

### GET /admin/node-tasks?state=&node_id=&page=&page_size= · 节点任务 ✅

`state` ∈ `pending/running/succeeded/failed/dead`（其他值 40001），缺省为全部。分页信封 `data:{list,total,page,page_size}`。
`Task`：`id, node_id, lease_id, deployment_id, kind, state, priority, attempts, max_attempts, run_after, locked_until, payload, result, last_error, created_at, updated_at`；`payload/result` 为 JSON（未完成任务 `result` 为 `null`）。`running` 任务的 `locked_until` 是心跳租约（约 3 分钟，执行期间每分钟续期）；已过期仍为 `running` 表示 worker 已中断，1 分钟内被回收。探活任务 `result`：`{"mode":"full","pass":true,"report_id":31,"failed_checks":[],"awaiting_host_key":"SHA256:...","discarded":false}`。

### POST /admin/node-tasks/:id/retry · 重试死信 ✅

仅 `dead` 可重试（否则 40900），次数清零回 `pending`，写审计 `node_task.retry`。`kind=probe`：payload 按节点当前代次重写，完整探活同时把节点置 `verifying`；节点已删除、`suspended`/`needs_reimage`/`key_mismatch` 或已有未结束的探活时 40900。`kind=provision` 的死信不可重试（40900「开通任务死信已自动善后(换节点或取消订单退款), 不可手动重试」，S2 T-083）。40400 任务不存在。响应 `{"task_id":9,"state":"pending"}`。

### GET /admin/leases?state=&node_id=&buyer_id=&page=&page_size= · 租约检索 ✅（S2 T-086）

三个条件可组合，均缺省为全部；按 `id` 倒序；分页同上（默认 20，> 100 回落 20）。
`state` ∈ `reserved/pending/provisioning/active/parked/suspended/grace/purging/purged/failed/cancelled`（其他值 40001）；`node_id`/`buyer_id` 须为正整数（否则 40001）。

```json
{"code":0,"message":"success","data":{"list":[
  {"id":5,"lease_no":"l-4g7bq4xc","order_id":88,"buyer_id":21,"product_id":12,"node_id":7,"cards":2,
   "delivery_kind":"container","state":"active","upstream_port":40000,
   "upstream_host_key_fp":"SHA256:...","container_name":"omnis-l-4g7bq4xc","volume_name":"omnis-l-4g7bq4xc-data",
   "access_until":"2026-11-28T10:00:00+08:00","access_changed_at":"2026-10-28T10:00:00+08:00","grace_until":null,
   "provisioned_at":"2026-10-28T10:00:00+08:00","purged_at":null,"fail_reason":null,
   "resource_alert":null,"outage_remedy":"extend",
   "created_at":"2026-10-28T09:58:00+08:00","updated_at":"2026-10-28T10:00:00+08:00"}],
  "total":1,"page":1,"page_size":20},
 "request_id":"..."}
```

- `resource_alert`：轻量探活发现的在租卡异常（如卡掉线 / UUID 变化），为空表示正常。`port_claim`、`commit_image` 不出接口。
- 40001 参数非法。

### GET /admin/access-sessions?lease_no=&ip=&page=&page_size= · 会话检索 ✅（S2 T-086）

`lease_no` 与 `ip` 可单独或组合，均缺省为全部；按 `started_at` 倒序；分页同上。结构同买家会话记录，另带 `buyer_id`：

```json
{"code":0,"message":"success","data":{"list":[
  {"id":13,"lease_no":"l-4g7bq4xc","buyer_id":21,"remote_ip":"2001:db8::1","auth_method":"password",
   "key_fingerprint":null,"key_name":null,"started_at":"2026-10-28T09:50:00+08:00","ended_at":null,
   "bytes_up":0,"bytes_down":0,"end_reason":null}],
  "total":1,"page":1,"page_size":20},
 "request_id":"..."}
```

- `lease_no` 须为 `l-` + 8 位小写字母/数字 2–7（否则 40001）；格式合法但不存在时返回空列表。
- `ip` 按规范形式匹配（`2001:0db8:0:0::1` 与 `2001:db8::1` 等价）；非法 IP 40001。

---

## 探活检查项与判定（实现口径）

| # | 检查 | 实现要点 |
|---|---|---|
| P1 | TCP 可达 | 3 次，单次 5s |
| P2 | SSH 握手与指纹 | 已钉死则比对，不一致 → `key_mismatch`；未钉死 → 记录观测值并在 P3 后停止 |
| P3 | 管理密钥认证 | 平台私钥登录 |
| P4 | GPU 清单 | `nvidia-smi --query-gpu=...`；卡 UUID 已被其他节点登记 → 不通过 |
| P5 | 声明一致性 | 型号按 `gpu_catalog` 归一后**完全相同**（忽略品牌/显存容量/HBM 类型/PCIe·SXM 形态；`4090 D`、`V100S`、`H100 NVL` 等后缀视为不同型号）、卡数、显存误差 ≤ 3%（GDDR 卡开 ECC 时按可见显存少 1/16 再比） |
| P6 | 拓扑 | `nvidia-smi topo -m` → `topo_group` |
| P7 | 健康 | 不可纠正 ECC、退役页/行重映射待生效或失败、24h 致命 Xid；问题卡标 `faulty`；**全部卡故障才判不通过** |
| P8 | 主机资源 | CPU/内存/Docker 数据目录所在盘/OS/内核 + 驱动 ≥ `fleet.min_driver_version` |
| P9 | 容器运行时 | Docker ≥ 24；拉取 `fleet.probe_image`；`docker run --gpus "device=<空闲卡>"` 可见卡数一致 |
| P10 | 磁盘配额（非必检） | `--storage-opt size=1G`；不支持则只能整机出租 |
| P11 | 租约端口：网关可达 | 端口段内**从末尾向下**选空闲端口起临时 TCP 监听容器，由访问网关从其出口做 TCP 连通自检（S2 T-085：系统自检身份登录 `p-<token>` 下发任务，见下文「访问网关内部接口」`probe`）。网关未配置、不可达、主机指纹不符或未在 10s 内回报 → 判不通过 |
| P12 | 租约端口：未裸露 | 同一端口从主服务出口直连，必须连不上 |
| P13 | 短压测 | 空闲卡逐卡 FP16 矩阵乘（默认 60s）+ 降频采样 + 压测后复扫 Xid/ECC；低于 `gpu_catalog.probe_baseline_tflops × fleet.probe.min_perf_ratio`、降频、掉卡的卡标 `faulty`；**全部卡不通过才判不通过**；无空闲卡时通过并注明 |

在租/预留/排空的卡只做只读检查（P4/P7），侵入项只用空闲卡与未分配端口。

**结论落库**：全部必检项通过 → `verified`、`status=online`、同步卡槽（健康卡 `free`、问题卡 `faulty`，此前 faulty 的卡恢复健康后回 `free`）、重算库存。任一必检项不通过 → `failed` + `status=offline`，库存扣除。周期补跑的完整探活若只是连不上（P1–P3）则只写报告，在线状态交给轻量探活判定。探活期间节点被停用/恢复、重装确认、SSH 信息被修改或指纹被确认（节点代次变化）→ 结论作废（任务 `result.discarded=true`，不写报告），不改节点状态；代次已变化后才开始执行的旧任务直接 `failed`、不连接节点。只有 `verifying`（本代次完整探活）或 `verified`（周期补跑）的节点能被探活结论置为 `verified`。

**周期**：`verified` 节点每 60s SSH 轻量探活（`nvidia-smi` 利用率/温度/ECC + Docker 守护进程 + 近 2 分钟 Xid），成功写心跳与 `available_cards`；距上次成功超过 90s → `offline` + 通知，恢复后自动上线。空闲卡掉卡或出现致命错误 → `faulty` + 通知；在租卡异常仅记运营告警（租约侧处理在 S2）。完整探活每 12h、端口复测每 24h 自动入队。

---

## 访问网关内部接口（S2 T-085）✅

仅供访问网关插件（`backend/cmd/accessgw`，运行在 sshpiperd v1.6.1 下）调用。**不在公网 API 上**：主服务在 `accessgw.internal_listen`（内网地址）上单独起一个 HTTP 服务，只挂下列路径；公网路由组不注册它们。设计见 docs/25 §25.6.2/§25.6.3 及其「S2 实现口径（T-085）」。

**Base**：`http://<accessgw.internal_listen>/api/v1/internal/accessgw`

**签名（每个请求必带）**

| 请求头 | 值 |
|---|---|
| `X-Gw-Timestamp` | Unix 秒；与主服务时钟偏差 > 60s 拒绝 |
| `X-Gw-Nonce` | 16 字节随机数的小写 hex（32 位）；300s 内不可重复 |
| `X-Gw-Signature` | `hex(HMAC-SHA256(accessgw.hmac_secret, method + "\n" + uri + "\n" + ts + "\n" + nonce + "\n" + hex(sha256(body))))`，小写 hex |

`uri` 为 RequestURI（路径 + 查询串，按实际发送的编码），例如 `/api/v1/internal/accessgw/revocations?cursor=1793181263-42`；GET 的 body 为空串。请求体上限 1 MiB。

**错误**：信封与 HTTP 200 同公网接口。签名/时间戳/nonce 不合法或重放 → `40100`「签名校验失败」；Redis 不可用（nonce 去重、爆破计数、探活任务）→ `50000`；签名通过但路径不存在 → `40400`；请求体非法 → `40001`。

### POST /auth · 鉴权与路由

请求：

```json
{"login":"l-7f3k9q2a","method":"publickey","pubkey_fp":"SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU","remote_ip":"198.51.100.7"}
```

```json
{"login":"l-7f3k9q2a","method":"password","password":"<访问凭证 access_value>","remote_ip":"2001:db8::7"}
```

- `method` ∈ `publickey` / `password`，其他 40001；`publickey` 时 `pubkey_fp` 必须以 `SHA256:` 开头，否则 40001；`remote_ip` 必须是合法 IP（不带端口），否则 40001。
- 不存在/非法的登录名、凭证不符、租约不可登录都**不是错误**：返回 `allow:false`，网关统一提示认证失败。
- `upstream_host` 恒为 **IP 字面量**：主服务按目标地址拒绝名单（含 `fleet.deny_cidrs`）解析并校验节点 host 后下发，网关不自行解析。节点 host 无法解析或命中拒绝名单时返回 **50000**「租约上游地址不可用」（网关拒绝该次登录，不计入爆破失败）。P11 探活任务的 `probe.host` 同样是 IP 字面量，网关对非 IP 目标直接回报「探活任务地址非法」，并在自身拨号钩子里按内置拒绝名单再拦截。

放行（买家）：

```json
{"code":0,"message":"success","data":{"allow":true,"upstream_host":"203.0.113.20","upstream_port":40001,"upstream_host_key_fp":"SHA256:Zq0n6n9Lr0m7y3G2b1J1p3b5fK0wQ3Xc9eN8uT4sV2A","access_until":"2026-11-27T10:00:00+08:00","cache_ttl_s":30},"request_id":"..."}
```

拒绝：

```json
{"code":0,"message":"success","data":{"allow":false,"cache_ttl_s":0},"request_id":"..."}
```

系统自检身份（`fleet.selftest_key_file` 公钥）登录 `provisioning` 租约（开通自检）：`allow:true`、`"system":true`、`"cache_ttl_s":0`，其余字段同买家放行。系统身份登录 `p-<token>`（P11 端口自检，token 为 32 位小写 hex，已由主服务登记）：

```json
{"code":0,"message":"success","data":{"allow":false,"cache_ttl_s":0,"probe":{"token":"9f2c4b1e0a7d4c3b8e6f5a4d3c2b1a09","host":"203.0.113.20","port":40999}},"request_id":"..."}
```

网关收到 `probe` 后从自身出口对 `host:port` 做 5s TCP 连接，调用 `/probe-result` 回报，然后拒绝该 SSH 登录。

判定规则：

| 条件 | 结果 |
|---|---|
| 来源 IP 处于封禁期（10 分钟内密码失败 20 次 → 封 30 分钟，按规范化后的 IP） | 拒绝（所有方式） |
| `password` 且「登录名+IP」处于锁定期（10 分钟内失败 10 次 → 锁 15 分钟） | 拒绝（公钥不受影响） |
| 租约 `state=active`、`delivery_kind=container`、`now < access_until`、已有上游端口与主机指纹 | 才进入凭证比对 |
| 公钥：指纹属于租约买家且 `revoked_at IS NULL` / 密码：等于订单访问凭证（常量时间比较） | 放行，`cache_ttl_s = min(30, access_until − now)` |
| 系统身份 | 仅 `provisioning` 且路由齐全的租约放行；不计数、不受封禁 |

只有**密码**失败计入爆破计数；公钥失败不计（SSH 客户端会依次尝试多把公钥）。

### GET /revocations?cursor= · 撤销游标

`cursor` 格式 `"<unix 秒>-<lease id>"`；首次传空串取起点。

```json
{"code":0,"message":"success","data":{"logins":[],"cursor":"1793181261-0"},"request_id":"..."}
```

```json
{"code":0,"message":"success","data":{"logins":["l-7f3k9q2a","l-ab2cd3ef"],"cursor":"1793181263-42"},"request_id":"..."}
```

- 返回 `leases.access_changed_at` 在游标之后（按 `(access_changed_at, id)` 严格递增）且**早于当前 2 秒**的租约登录名，每页最多 1000 条；没有新行时 `cursor` 原样返回。空游标返回 `NOW()−2s` 处的起点，不回放历史。
- 网关拿到 `logins` 即清掉这些登录名的放行缓存；`logins` 满 1000 条时立即用新游标再拉（单次轮询最多 20 页）。轮询失败 → 网关清空全部缓存。
- `access_changed_at` 刷新时机（S2 现状）：租约每次状态迁移；买家删除公钥（刷新其名下 `active/parked/suspended/grace` 租约，T-086）。冻结吊销凭证、续租改 `access_until`、访问凭证重置时的刷新属 T-088（`Repository.touchAccess` 已备好）；在此之前这几类变化依赖缓存 TTL（≤ 30s）自然失效。
- 游标格式非法 → 40001。

### POST /sessions · 会话事件上报

```json
{"events":[
  {"gw_session_id":"7b0c…e1","login":"l-7f3k9q2a","remote_ip":"198.51.100.7","auth_method":"publickey","key_fingerprint":"SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU","started_at":"2026-10-28T10:00:00+08:00","bytes_up":0,"bytes_down":0},
  {"gw_session_id":"7b0c…e1","login":"l-7f3k9q2a","remote_ip":"198.51.100.7","auth_method":"publickey","key_fingerprint":"SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU","started_at":"2026-10-28T10:00:00+08:00","ended_at":"2026-10-28T11:30:00+08:00","bytes_up":0,"bytes_down":0,"end_reason":"closed"}
]}
```

```json
{"code":0,"message":"success","data":{"accepted":2,"ignored":0},"request_id":"..."}
```

- 单批最多 500 条，超过 40001。按 `gw_session_id` upsert：开始与结束事件可分批、乱序、重复上报；`ended_at`/`end_reason` 首次写入后不再覆盖，字节数取较大值。
- 计入 `ignored`（不报错）：登录名不是存在的租约、`auth_method` 不是 `publickey/password`、`remote_ip` 非法、`ended_at` 早于 `started_at`、`gw_session_id` 为空或过长、指纹为系统自检身份。
- `bytes_up/bytes_down`：sshpiperd v1.6.1 插件拿不到字节计数，当前恒为 0。`end_reason`：`closed`（正常断开）或错误摘要。不记录会话内容。保留 180 天。

### POST /probe-result · P11 端口自检回报

```json
{"token":"9f2c4b1e0a7d4c3b8e6f5a4d3c2b1a09","reachable":false,"error":"dial tcp 203.0.113.20:40999: connect: connection refused"}
```

```json
{"code":0,"message":"success","data":{},"request_id":"..."}
```

`token` 非 32 位小写 hex → 40001；未登记或已过期（任务 2 分钟有效）→ 40400。结果保存 120s，主服务读取后删除。
