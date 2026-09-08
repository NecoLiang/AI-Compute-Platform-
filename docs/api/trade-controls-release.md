# 第一批交易控制修复 — 2026-09-08

范围：交易开关、动态费率、风控冻结、库存释放、资料更新假成功、商品健康度。前后端独立仓库、独立发布。本文件记录代码和本地验证，不表示生产已经部署。

## 接口与实际行为

| 能力 | 后端 | 前端 |
|---|---|---|
| 开关 / 费率 | 复用 GET/PUT `/admin/config`，新订单事务锁定配置；已有订单保留 platform_fee | 设置页保存真实配置；结算页读取公开 GET `/trading-config`，禁止缓存；缺失/错误时禁止提交 |
| 风控冻结 | 复用订单/账户冻结；目标、凭证、审计与 processing 同事务；会话失效成功后 resolved，失败可重试 | 告警显示具体对象，冻结前确认；processing 可重试；不支持的对象不提供冻结操作 |
| 库存与续租 | 实际占用数量归还并清零；未知历史占用拒绝自动释放；续租返回 40900 | 使用后端 actions；不开放续租按钮 |
| 资料 | GET `/user/profile` 读数据库；PUT 返回 HTTP 501 / code 50000 | 保留 `/auth/me` 与 KYC 资料只读展示 |
| 健康度 | 商品列表/详情返回 health，既有 offline 下单拒绝保留 | 列表/详情/结算展示；unknown 不显示徽章；offline 禁止购买 |

完整续租、手机号/邮箱编辑及验证、真实支付、外部 KYC、智能选型和节点管理新页面属于后续批次。协议正文及版本不变。

## 增量迁移 020

脚本位于后端 `backend/migrations/020_order_stock_reservation.up.sql`。为 orders 增加可空 stock_reserved：正数表示实际占用量，0 表示未占用或已归还，NULL 表示历史记录尚无可靠占用证据。字段有非负且不超过 quantity 的数据库约束，不对 HTTP 客户端暴露。

迁移仅将 cancelled/refunded/completed 终态置 0，**不根据 ORD/REN/ORDDEV 前缀猜测非终态占用量**。新订单与库存扣减同事务写入占用量；开发夹具显式写 0。

生产操作顺序（本批未执行）：

1. 核对当前两端 SHA、镜像、迁移历史及表结构。已有数据库必须显式执行增量 SQL，Compose 初始化挂载不升级已有表。
2. 准备并校验完整备份、订单/商品数据与占用映射导出、迁移 SQL SHA-256；备份保存到受限目录，不进入 Git。取得生产迁移授权后才执行。
3. 暂停新增订单及会改变订单/库存状态的任务与写入口，避免旧服务在迁移和占用核对期间继续写入。旧版本忽略 trading_enabled，不能只依赖旧后台开关。
4. 执行 020，核对以下查询。对每条非终态 NULL 记录，根据扣减记录、历史订单创建来源与资源盘点进行核对，按明确订单 ID 回填；无可靠证据时保留 NULL，不能批量设 quantity 或 0。
5. 确认库存余额与已核对占用量一致，所有计划继续履约的旧订单均有确定值，再切换新后端镜像。新版本遇到 NULL 时返回错误并回滚状态，后台任务保留订单且记录错误，等待核对。
6. 验证后端新契约后部署前端，再恢复交易写入口。前端 main push 会自动部署；后端 main push 有 backend/** 路径过滤。

迁移前：

```sql
SELECT id, order_no, product_id, quantity, status
FROM orders WHERE status NOT IN ('cancelled','refunded','completed') ORDER BY id;
SELECT id, stock, status FROM products ORDER BY id;
```

迁移后：

```sql
SELECT column_name, column_type, is_nullable
FROM information_schema.columns
WHERE table_schema=DATABASE() AND table_name='orders' AND column_name='stock_reserved';
SELECT id, order_no, product_id, quantity, status, stock_reserved
FROM orders WHERE stock_reserved IS NULL ORDER BY id;
SELECT id FROM orders WHERE stock_reserved < 0 OR stock_reserved > quantity;
```

回填须在锁定对应订单的事务内执行，使用已核对的 ID、原状态和数量作为条件，检查受影响行数，并保留核对依据和审计。仅更新占用元数据不会自动修正旧版本可能已经增加过的商品库存，历史余额异常必须另行核对。

## 回退

- 应用回退保留 stock_reserved、订单和审计等新增数据。020.down.sql 主动拒绝自动降级，防止迁移工具把“未执行”误报为回退成功。
- 后端旧版本仍有错误库存归还逻辑；不能在未封闭订单/库存写入口的情况下直接恢复旧镜像。需要回退业务版本时保留本批库存安全修复，或维持写入暂停直至完成核对。
- 不用旧备份整库覆盖新业务记录。字段移除必须在导出占用证据、核对全部非终态订单并准备专门降级脚本后单独执行。
- 前端回退也需核对费率展示是否匹配当前配置，避免恢复固定 5% 试算。

## 验证

- 后端 `go test ./...`：需设置 TEST_MYSQL_DSN / TEST_REDIS_ADDR 指向独立测试实例；部分旧测试会创建并删除固定命名测试库，不能指向生产或共享业务实例。
- 本批 HTTP 回归：后台改费率 → 新订单费用 / 旧订单保留费用；关开交易 → 拒绝/恢复；公共健康度契约；续租明确拒绝；资料不假保存。
- 库存回归：零占用归还、未知占用拒绝且事务回滚、正常订单超时/退款/到期、重复与并发归还。
- 风控回归：订单和凭证实际冻结；审计失败回滚；缺失/无效目标拒绝；重复不重复审计；账户旧会话失效；会话失效服务失败后 processing 可重试。
- 前端 `pnpm check`；浏览器验证设置、结算、健康度与冻结确认/重试，包含移动宽度。真实支付、短信、模型和链上交易不作为本批验证手段。

### 提交前本地验收快照 — 2026-09-08

- 两端工作分支均为 `fix/trade-controls-20260908`，基线分别为前端 `1c46754`、后端 `1588356`；未提交、推送或部署生产。
- 前端 `pnpm check` 通过：148 项测试、TypeScript、Lint 与生产构建通过；保留两条已有的 `<img>` Lint 提示。
- 后端使用独立 MySQL / Redis 执行 `go test ./...`：438 项通过（含子测试），仅 `TestLive_ComputeEstimation` 跳过。`compute`、`admin`、`user`、`middleware` 的 race 检查通过。
- 费用回归同时验证关闭交易后的旧订单付款，以及不同费率订单按各自保存的费用结算；外部支付网关使用测试替身。库存回归包含冻结后重复取消只归还一次。
- 浏览器 8 项通过：后台改费率并真实下单、关闭交易拦截提交、离线商品禁购 / 未知健康度无徽章、配置失败后重试、冻结确认 / 取消、处理中重试，以及 390px 结算和市场页面无横向溢出。
- 本地业务数据库已在受限目录备份后执行 020，8080 API 已更新，3000 前端可访问。12 条历史非终态订单保留 `stock_reserved=NULL`，须核对实际占用后才能自动释放；没有推测回填或改写其库存余额。
- 此快照时生产迁移、历史占用核对和发布未执行；后续记录见 [问题追踪](../project-progress-20260908.md)。续租和资料编辑按本批范围保持明确未开放。
