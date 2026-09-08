# 续租发布与回退 — 2026-09-08

目标：后端 [#9](https://github.com/NecoLiang/AI-Compute-Platform-/issues/9)，前端 [#21](https://github.com/Dylan-Nihilo/compute-exchange/issues/21)。基线：后端 `dd29842`，前端 `efde712`。本轮同时支持到期前延长和到期后重新预留、交付、签收。

## 发布顺序与授权

1. 生产执行前另行确认 021 迁移授权；先前 020 授权不涵盖本次新表。
2. 备份生产数据库（`mysqldump --single-transaction --routines --triggers`），目录权限 700、文件 600；计算并记录备份 SHA-256、两个旧镜像 revision 和当前 orders/products 数量。备份不进入 Git。
3. 核对 020 的 `orders.stock_reserved` 已存在，021 表尚不存在。执行后端 `backend/migrations/021_order_renewals.up.sql`，仅新增 `order_renewals` 和索引/约束，不回填历史续租、不修改原订单、库存或协议记录。
4. SQL SHA-256：`19a26272fbb71d1c9c9adbd31192fe0d734a6acb7ba69a0c53b1343c82b71e33`。核对部署服务器 SQL 哈希一致后执行；既有库不会因 Compose 初始化挂载自动升级。
5. 确认表字段、父子外键和 `(parent_order_id, request_id)` 唯一索引，再合并后端 PR。后端 CI 成功后核对实际镜像 SHA、健康码与续租 API，再合并前端 PR并核对其自动部署、页面和 BFF。
6. 真实易宝未配置时仍拒绝真实支付；本轮测试网关只在独立本地测试二进制中存在，未引入生产开关或模拟支付入口。真实支付、分账、退款到账、机房侧凭证撤销分别留在原 Issue #12 / #8，不因本轮测试关闭。

迁移前只读核对：

```sql
SELECT COUNT(*) FROM information_schema.columns
WHERE table_schema=DATABASE() AND table_name='orders' AND column_name='stock_reserved';
SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema=DATABASE() AND table_name='order_renewals';
SELECT COUNT(*) AS orders_count FROM orders;
SELECT COUNT(*) AS products_count FROM products;
SELECT COUNT(*) AS unreconciled_orders FROM orders WHERE stock_reserved IS NULL;
```

迁移后核对：

```sql
SHOW CREATE TABLE order_renewals;
SHOW INDEX FROM order_renewals;
SELECT COUNT(*) FROM order_renewals;
```

已存在的表不能被重复覆盖；如发现表已存在，先比对结构和数据，不直接重跑或删除。历史 `stock_reserved=NULL` 仍需人工核对，本轮不会猜测占用量。

## 回退

- 发布期间旧后端可继续运行：021 为附加表，原业务表结构未变。
- 已产生续租后，优先回退前端入口并保留修复后的后端；旧后端不了解父子库存和当前履约周期，不能继续处理存在续租的业务。紧急回退后端前必须封闭相关订单、支付回调、交付与到期任务并核对在途续租，不能只关闭新建交易开关。
- 保留续租、支付、同意与交付历史，禁止通过整库旧备份覆盖新业务。021.down.sql 主动拒绝自动删表；删表需独立归档与审核方案。
- 退款只自动撤销未使用且尚未被后续续期覆盖的部分；若申请后开始使用，完成退款会明确拒绝自动回退，由平台核对。不得将订单 `refunded` 当作渠道资金已到账。

## 本地验收

- MySQL / Redis 使用本轮独立容器，浏览器 API 为 8081、前端为 3031。原 8080、3000、Cap 与共享容器保持运行。
- 回归覆盖：动态报价与旧金额保持、显式同意、重复/并发提交、重复/错误回调、取消/冻结/超时/退款不虚增库存、到期后重新交付、旧交付存证保持，以及到期任务等锁后重新判断租期。
- 浏览器通过真实 JWT → BFF → Go → MySQL 验证，支付网关在测试二进制中替换；覆盖协议默认未勾选/改周期重置、失效报价重试、待付续租恢复、支付返回与原页轮询刷新、失败/取消确认、库存不足及重新交付签收、390px 页面。
- 自动化统计与 PR/CI 链接在对应 PR、Issue 中记录；生产迁移与发布尚待授权执行，不用本地结果代替生产验收。
