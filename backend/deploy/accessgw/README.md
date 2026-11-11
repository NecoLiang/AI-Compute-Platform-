# 访问网关部署（与主服务同机）

设计见 docs/25 §25.6、§25.12、§25.14。当前形态：网关（sshpiperd v1.6.1 + failtoban + `cmd/accessgw` 插件）与主服务
同在万象 ECS（123.56.177.241）上，用独立出口 IP 隔离；后续迁到独立主机时只需换 `--bind-ip/--egress-ip` 与
`--main-url`，主服务配置不变。

## 为什么要第二个出口 IP

节点上租约端口的 DOCKER-USER 规则只放行 `accessgw.egress_ips`，开通自检 P12 要求"主服务出口直连租约端口必须不通"。
主服务出口是 123.56.177.241（也是 `fleet.controller_egress_ips`，节点管理 SSH 用），网关必须从另一个公网 IP 出去：

```
买家 ──TCP 2222──> EIP2 ──> 辅助私网 IP:2222 ──> omnis-accessgw ──(egress 网络, SNAT 到辅助私网 IP → EIP2)──> 节点租约端口
                                                   │
                                     omnis-accessgw-internal(internal, 172.30.21.0/24)
                                                   │
                                     wanxiang-backend :9090 内部接口(HMAC)，主服务经同一网络连网关 :2222 做自检
```

- `omnis-accessgw-egress`（172.30.20.0/24）由 `setup.sh` 创建，`com.docker.network.host_ipv4=<辅助私网 IP>`。
- `omnis-accessgw-internal` 由主服务 `deploy/compose.yml` 创建（无外网出口，不改变主服务的默认路由与出口 IP）。
  网关在该网络上带别名 `accessgw.public_host`，主服务自检拨 `gw.omnisline.com:2222` 时在容器内直接解析到网关。
- 内部接口 `:9090` 不映射到宿主机；同机其他容器网络（middleware/frontend）理论上可达，靠 HMAC 保护。

## 前置（控制台操作）

1. ECS 控制台 → 实例 → 弹性网卡 → 主网卡 → **分配辅助私网 IP**（同交换机网段 172.21.160.0/20）。
2. EIP 控制台 → 新申请一个 EIP（EIP2）→ 绑定到该实例主网卡的**辅助私网 IP**。
3. 安全组入方向放行 **TCP 2222**（买家连接；可先只放测试 IP）。
4. DNS：`gw.omnisline.com` A 记录 → EIP2。
5. 镜像：Actions `Fleet Images` 工作流构建并冒烟通过后，作业摘要给出四个 `ref@sha256`。国内机器用 ACR 地址
   （仓库 secrets `ACR_REGISTRY`/`ACR_NAMESPACE`/`ACR_USERNAME`/`ACR_PASSWORD`），否则用 ghcr（包须设为 public 或在机器上 `docker login ghcr.io`）。
6. 主服务已部署到包含 `omnis-accessgw-internal` 网络的版本（`docker network inspect omnis-accessgw-internal`）。

## 安装

```bash
# 本地
scp backend/deploy/accessgw/compose.yml backend/deploy/accessgw/setup.sh wanxiang:/opt/omnis-accessgw/
# 服务器(root)
/opt/omnis-accessgw/setup.sh \
  --bind-ip <辅助私网IP> --egress-ip <EIP2> --public-host gw.omnisline.com \
  --image <ACCESSGW_IMAGE ref@sha256> --add-secondary-ip --up
```

`setup.sh` 可重复执行，已存在的密钥/网络/环境项只校验不覆盖（值不同即报错退出）。它会：

- 把辅助私网 IP 持久加到 `cloud-init eth0`（NetworkManager，`nmcli device reapply` 不断网）；
- 创建 egress 网络并**实测**网关出口公网地址等于 `--egress-ip`（不等即退出——意味着 EIP2 没绑对，P12 会失败）；
- 生成 `/opt/omnis-accessgw/keys/{host,upstream}_ed25519`（属主 10022，0400）与主服务自检私钥
  `/opt/wanxiang/backend/secrets/selftest_ed25519`（root，0400，容器内 `/etc/omnis/selftest_ed25519`）；
- 生成 HMAC 共享密钥，写入网关 `.env` 与主服务 `.env`（主服务 `.env` 先备份为 `.env.before-accessgw-<时间>`）；
- 往主服务 `.env` 追加缺少的 `ACCESSGW_PUBLIC_HOST/PUBLIC_PORT/INTERNAL_LISTEN/EGRESS_IPS/UPSTREAM_PUBKEY/HOST_KEY_FINGERPRINT/HMAC_SECRET`
  与 `FLEET_SELFTEST_KEY_FILE`；
- `--up` 时启动网关并确认 `<辅助私网IP>:2222` 返回 SSH 握手。

私钥与 HMAC 不输出；结尾打印上游公钥、主机密钥指纹、出口 IP。外部核对：`ssh-keyscan -p 2222 gw.omnisline.com | ssh-keygen -lf -`
的 ED25519 指纹应等于打印的指纹。

`fleet.enabled=false` 时主服务不启动内部接口，网关每 5s 记一条"拉取撤销列表失败"并拒绝所有登录（fail-closed，属预期）。
可以先不加 `--up`，到打开 fleet 时再启动。

## 打开 fleet（须业务负责人批准）

主服务 `.env` 为 raw 格式（值原样读取，**不要加引号**），在 `setup.sh` 写入的项之外补齐：

```dotenv
FLEET_ENABLED=true
FLEET_CONTROLLER_EGRESS_IPS=123.56.177.241
FLEET_BASE_IMAGES=[{"name":"PyTorch 2.7.1 / CUDA 12.8","ref":"<lease-base ref@sha256>"}]
FLEET_PROBE_IMAGE=<probe ref@sha256>
FLEET_MODEL_PULL_IMAGE=<model-pull ref@sha256>
FLEET_MIN_DRIVER_VERSION=535
FLEET_DENY_CIDRS=172.21.160.0/20,123.56.177.241,<EIP2>
```

`FLEET_OUTAGE_YEEPAY_PARTIAL_REFUND_CONFIRMED` 保持缺省（false），易宝沙箱验证部分退款后再置 true。缺项或非法时主服务
启动即失败并一次列出全部问题；`deploy.sh` 的候选容器会先暴露这个错误，不会切换线上容器。改完 `.env` 后重新执行
`/opt/wanxiang/backend/deploy.sh $(cat /opt/wanxiang/backend/current-release)` 使其生效。

一键部署引擎镜像（模型目录里的 vLLM/SGLang 等）同样要在 ACR 有镜像，国内节点从 Docker Hub/ghcr 拉取极慢。

## 运维

- 升级网关：改 `/opt/omnis-accessgw/.env` 的 `ACCESSGW_IMAGE` → `docker compose -f /opt/omnis-accessgw/compose.yml --project-directory /opt/omnis-accessgw up -d`。
  在线会话随容器重启断开。
- 停用：同目录 `docker compose down`；不影响主服务（internal 网络归主服务所有，不随网关删除）。
- 日志：`docker logs omnis-accessgw`（JSON，20m×5 轮转）；failtoban 封禁 30 分钟/5 次失败，主服务自检来源 172.30.21.0/24 不计入。
- 主机密钥一旦对外（买家 known_hosts、`accessgw.host_key_fingerprint`）不要轮换；上游密钥轮换需同步所有在租容器的 authorized_keys，当前不支持在线轮换。
- 重启 ECS 后核对 `ip -4 addr show eth0` 仍有辅助私网 IP。
