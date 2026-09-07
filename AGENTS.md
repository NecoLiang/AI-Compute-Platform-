# OmniS Backend — Agent Guide

本仓库是 OmniS 的 Go 服务端及业务文档仓库，远端为 `NecoLiang/AI-Compute-Platform-`。在全栈 workspace 中通过 `backend/` 目录链接接入；前端是独立的 `Dylan-Nihilo/compute-exchange` 仓库。**本文件的路径以服务端仓库根为基准；Go module 在仓库内的 `backend/`，不是当前根目录。**

## 开始工作

- 先检查 `git status -sb`、`git remote -v` 和分支。通过 OmniS workspace 工作时同时阅读其根 AGENTS.md / CONTEXT.md；单独 checkout 不要求创建 workspace。
- 根 README 和早期编号文档含“需求阶段、不写代码”等历史快照，不代表当前实现。任务以用户当前要求、真实路由、源码和测试为依据。
- 保留已有未提交改动，不自动 pull、reset、换分支或整理无关文件。Go 与前端修改分别提交，HTTP 是跨端契约。
- 默认中文沟通；代码标识、命令及新增注释用英文，遵循现有文件约定。严格区分本地测试、实际持久化、生产部署和第三方服务开通。

## 启动与配置

技术为 Go / Gin / sqlx / MySQL 8 / Redis，Go 版本以 `backend/go.mod` 为准，module 名为 `tokenfactory`。

先检查已有应用进程、`8080` 端口与 Docker Compose 项目。现有本地 Docker 方案在服务端仓库根执行：

```bash
cd backend
docker compose ps
# Start only when this project is not already running:
docker compose up -d --build app
```

该 Compose 会管理本项目 app、MySQL、Redis，并保留已有命名卷。修改 Go 源码后容器不会自动热更新；只在确需验证新代码时重建 app，不用 `down -v` 清库。

如选择宿主机 Go 进程，在独立 MySQL/Redis 已可访问且 `backend/config.yaml` 指向它们时运行：

```bash
cd backend
go run ./cmd/server
```

两种方式择一，不能同时争用端口。Compose 的 MySQL/Redis 默认未向宿主机发布端口，不能直接假设宿主机 Go 可连接容器内部地址。

- API 默认 http://127.0.0.1:8080/api/v1；健康检查 http://127.0.0.1:8080/health。业务成功还要核对 JSON `code`。
- `backend/cmd/server/main.go` 读取工作目录的 `config.yaml`；Compose 将 `backend/config.docker.yaml` 挂载为应用配置。
- `backend/pkg/config/config.go` 支持点号配置映射为大写下划线环境变量，例如 `DATABASE_DSN`。沿用已有安全配置，不打印完整 DSN、JWT/加密密钥或外部凭据。
- 本地短信/微信配置分别见 `backend/.env.sms.example`、`backend/.env.wechat.example`；Compose 可读取对应 `.local` 文件。不要覆盖已存在配置，也不要在联调时默认发送真实短信或请求支付渠道。
- MySQL 会话时区与 Go DSN 必须一致；复用 `backend/pkg/db/mysql.go` 的 parseTime/时区校验，不能绕过它来消除启动错误。时间偏差会影响订单超时和访问凭证有效期。

## 代码入口与职责

| 路径 | 职责 |
|---|---|
| `backend/cmd/server/main.go` | 依赖装配、路由组、中间件和后台任务启动 |
| `backend/internal/auth/`、`backend/internal/sms/` | Cap、验证码、注册/登录、会话、微信身份绑定 |
| `backend/internal/user/` | 个人/企业 KYC、角色与业务身份 |
| `backend/internal/compute/` | 供给方资质、商品、库存、订单、交付、结算与交易状态 |
| `backend/internal/catalog/`、`backend/internal/scheduler/` | GPU 型号目录、节点健康与调度 |
| `backend/internal/payment/` | 支付状态、回调和分账；真实渠道仍未接入 |
| `backend/internal/legal/` | 协议版本校验和同意记录 |
| `backend/internal/invoice/`、`backend/internal/ticket/`、`backend/internal/notification/` | 发票、工单、通知 |
| `backend/internal/equipment/`、`backend/internal/intermediary/`、`backend/internal/admin/`、`backend/internal/blockchain/` | 设备、线索、运营与存证 |
| `backend/pkg/middleware/`、`backend/pkg/response/` | 鉴权/角色、请求上下文、统一响应 |
| `backend/api/swagger.yaml`、`docs/api/` | API 契约和前端联调说明 |
| `backend/migrations/` | 数据库增量 SQL |

沿用 handler → service → repository 分工：handler 做请求解析和响应，service 执行业务规则，repository 操作数据库。先查现有调用方与事务边界，优先修复共用入口，不添加单一实现的抽象或重复封装。

## 必须保持的业务约束

- 用户 ID、角色与资源归属以认证上下文及数据库为准，不信任请求中的身份字段。注册默认 buyer，私有数据与公开市场使用各自权限入口。
- 商品公开详情仅允许 active；供给方、运营与历史订单仍须在鉴权后访问相关商品，不要让公开过滤破坏私有流程。
- 下单的价格、库存、租期、角色、KYC 和同意校验在服务端完成。取消/超时/退款等状态变化需同步处理库存、交付访问权限与通知；重复操作不能重复释放库存或重复记账。
- 区分数据库数字 ID 与 `order_no`。运营关闭订单兼容两类标识，先解析真实订单，再执行状态流转。
- 版本校验与同意契约见 `docs/api/legal-consent-api.md`。注册先校验版本再消费验证码；KYC 要独立敏感信息同意；发布/重提和订单要明确同意当前规范。
- 同意与注册、KYC、商品或订单必须原子提交；复用 `backend/internal/legal/` 的事务记录，使用服务端时间，不回填历史同意。版本变更需与前端正文/adapter 同步，并保留历史正文。
- 验证码与微信 state/binding ticket 维持一次性消费、有效期和账户归属；配置不全时明确禁用，不跳过校验或返回模拟成功。
- 当前 KYC 是后端持久化的试点自动通过，不是真实身份核验。易宝 `CreatePayment` / `VerifyCallback` / `CreateSplit` 未接渠道时应明确拒绝；测试替身不能变成生产放行路径。
- 不在日志、公开或无授权的响应、提交中泄露完整身份材料、访问凭证、真实短信码、Cookie、私钥；保留 request_id 等必要排障信息。本地验证码预览只能使用受控的 debug 配置，不能带入生产。

## 验证

从 Go module 目录执行基础检查：

```bash
cd backend
go test ./...
```

- Go 代码修改运行 gofmt，并先执行覆盖实际问题的回归，再运行受影响模块检查。纯文档只检查内容、路径、命令与 `git diff --check`。
- 未设置 `TEST_MYSQL_DSN` 会跳过相关数据库集成测试；注册同意测试还需要 `TEST_REDIS_ADDR`。测试跳过不能汇报为数据库回归通过。
- 测试凭据只注入当前测试进程。使用隔离、可丢弃的 MySQL/Redis，**绝不连接生产**；不要把某个包专用的 DSN 全局导出后套用所有 Go 测试。
- `backend/internal/compute/trade_flow_integration_test.go` 会创建/删除临时数据库；`backend/internal/user/kyc_integration_test.go` 会重建固定的 `tokenfactory_kyc_test`，同一实例不要并行跑多份 KYC 测试。

准备好隔离测试环境后，按目标运行：

```bash
# Run from the Go module directory; inject test environment securely first.
go test ./internal/compute -run 'TestTrade|TestRegistrationRecordsExplicitVersionedConsent|TestKYCSensitiveConsentIsSeparateAndAtomic' -count=1
go test ./internal/user -run 'TestKYCSubmissionIsAutoVerified|TestEnterpriseKYCSubmissionPersistsCompleteApplication' -count=1
```

契约变更同时核对 handler/DTO、Swagger、`docs/api/`、前端 adapter/schema/BFF 及两端测试；单边单测不能替代端到端结论。

## 迁移、提交与发布

- 新数据库由 `backend/docker-compose.yml` 的初始化挂载建表；已有卷不会重新执行 SQL。按完整迁移文件名和实际表结构核对，不能仅凭编号或挂载存在判断已迁移。
- 生产迁移需先准备备份、校验哈希、增量 SQL、影响和回滚方案，单独获得授权后执行，再验证实际 schema。应用回滚不自动删除业务、微信关联或同意记录。
- 只 stage 服务端本次改动并独立提交；不混入前端源码、环境文件、备份和本地账号数据。push 前 fetch 并检查远端分支与待发布差异，保留远端其他人的提交。
- `.github/workflows/deploy-backend.yml` 对 main 的 `backend/**` 和工作流自身改动自动测试/部署；也支持手动触发，部署 job 只在 main 执行。根 AGENTS.md 的纯文档变更不匹配该路径过滤，但同批其他提交可能匹配，必须检查完整待推送范围。
- 未授权发布时推送对应工作分支，不能为文档更新顺带推进待迁移的业务到 main。workspace 根目录没有远端不妨碍本仓库按自己的 origin 推送。
- 发布说明与脚本见 `backend/deploy/README.md`、`backend/deploy/deploy.sh`、`backend/deploy/compose.yml`。仅管理 `wanxiang-backend`，不重建前端或停止共享中间件；生产 8080 仅绑定 `127.0.0.1`。
- 先验证候选容器，再切换并保留旧镜像；部署完成需有 CI、提交 SHA、运行镜像、健康状态及实际 API 业务响应证据。生产配置、Secrets、第三方调用不属于普通代码提交授权。

## 待发布事项 — 2026-09-07

业务版本 `43785c2` 已在远端 `release/legal-consent-20260906`，生产仍为 `783fbfb`。微信关联 migration `018_wechat_login` 与同意记录 migration `019_legal_consents` 尚未在生产执行，需用户单独确认后再继续发布；前端协议页面已上线不能证明后端留痕已启用。真实支付、外部 KYC 和微信生产配置仍未开放。继续工作时重查当前状态，本段不授予迁移或部署权限。
