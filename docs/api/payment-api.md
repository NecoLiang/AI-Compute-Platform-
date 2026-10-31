# 支付分账 Payment API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 除 callback 外均需 `Bearer <token>`

> ⚠️ **易宝支付未接入**。`POST /payment/pay` 和 `POST /payment/callback` 当前返回错误。沙箱环境需联系易宝商务开通。

---

## POST /payment/pay · 发起支付 ✅ buyer

```
curl -X POST http://localhost:8080/api/v1/payment/pay \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"order_no":"ORD20260713001","channel":"wechat"}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| order_no | string | ✅ | 下单返回的订单号 |
| channel | string | ✅ | wechat / alipay / bank |

**当前返回**（易宝未接入）
```json
{"code":50000,"message":"易宝支付未接入: 需配置 merchant_no + RSA私钥 + API地址"}
```

**接入后预期返回**
```json
{"code":0,"data":{"pay_url":"https://yeepay.com/...","tx_id":"YP..."}}
```
> 前端拿到 HTTPS `pay_url` 后跳转收银台；当前未实现二维码展示。

---

## POST /payment/yeepay/notify/:kind · 易宝异步通知（易宝调用，无需登录）

`:kind` ∈ `pay`（支付结果）/ `clear`（清算）/ `refund`（退款结果，含 T-106 部分退款，按退款请求号路由）。公开路由，信任来源是报文解密验签，验签失败一律拒绝。（旧文档中的 `POST /payment/callback` 从未注册，以本路由为准。）

---

## GET /payment/status/:order_no · 支付状态 ✅ buyer

```
curl http://localhost:8080/api/v1/payment/status/ORD20260713001 \
  -H "Authorization: Bearer <token>"
```

---

## POST /payment/supplier/onboard · 供给方进件 ✅ supplier

## GET /payment/supplier/onboard/status · 进件状态 ✅ supplier

## GET /payment/settlements · 分账记录 ✅ supplier

```
curl "http://localhost:8080/api/v1/payment/settlements?order_no=ORD20260713001" \
  -H "Authorization: Bearer <token>"
```

记录（含 `GET /supplier/settlements`）新增两个字段（迁移 037，托管订单才非 0）：`refund_deduction` 已成功部分退款从本行扣减的金额（分；收款时锁定的应结 = `amount + refund_deduction`），`platform_compensation` 仅供给方行，平台责任退款超出平台费而从本行扣下、由平台另行补偿的金额（分）。托管订单的 `amount` 在租期末分账前按部分退款重算，口径见下文「租期末结算与部分退款」。

---

## 金额说明

所有金额单位为**分(fen)**，`int64` 类型：

| 展示值 | 传输值 |
|--------|:------:|
| ¥35.00 | 3500 |
| ¥201,600.00 | 20160000 |
| ¥10,080.00 (5%佣金) | 1008000 |

> 前端展示：金额 / 100，保留两位小数。禁止在前端用浮点数做金额运算。

---

## 分账规则

```
supplierAmount = totalAmount - platformFee
platformFee = totalAmount × feeRate / 10000  (feeRate默认500=5%)
```


## 支付与业务订单衔接（2026-09-05）

`POST /payment/pay` 从登录会话取得买家身份，并验证角色、认证、订单归属、`pending_payment` 状态与 15 分钟有效期。只接收订单号及渠道，不接受客户端金额；金额读取订单快照。重复请求复用已保存的 `pay_url` 和 `tx_id`，不重复创建付款。支付状态查询限定订单本人，未发起时返回空数组。

回调先通过外部渠道验签，再验证交易号对应的订单号、金额与成功状态。在同一事务内更新 payment 为 `paid`、业务订单为 `paid` 并生成两条分账记录，供给方 ID 来自商品，佣金取自订单快照。重复回调不会重复写分账或把交付中订单退回待交付。渠道接受分账后仅记 `processing`，不冒充资金已到账；失败回调可重试既有分账。

支付超时或订单取消后到达的成功回调不会恢复订单或重新消耗已释放库存，需由支付渠道对账处理。真实渠道及晚到款项对账仍需接入易宝。

**当前真实渠道仍未接入**：默认 `YeepayClient` 拒绝创建支付和回调验签。数据库集成测试仅在外部支付边界注入测试替身，不是沙箱扣款或真实支付证明。需要可用商户、渠道接口、验签资料后才能验收真实支付及到账。

数据库增量：`017_payment_checkout_url`；`pay_url` 仅随发起支付响应返回，不加入通用支付记录 JSON。

本地回归：在独立 MySQL 实例设置 `TEST_MYSQL_DSN`，运行 `go test ./internal/compute -run TestTrade -count=1 -v`，覆盖准入、驳回重提、询价进入 CRM、支付归属与金额、回调到交付签收、幂等及超时返库存。测试自动创建并删除独立数据库，不改业务库。

## 真实渠道接入缺口（2026-09-05 复核）

这不是仅填写环境变量即可启用的能力。`NewYeepayClient()` 当前返回空客户端，`CreatePayment`、`VerifyCallback`、`CreateSplit` 均为拒绝执行的占位实现。

- 接入前需确认已签约的聚合支付、合单、分账产品及沙箱权限，取得对应 API 文档、平台服务商与二级商户标识、应用标识及本地密钥配置位置。
- `CallbackReq` 当前只有交易号、订单号、状态、金额，是业务测试使用的简化结构，不能作为易宝报文格式或签名协议。真实接入须按渠道协议处理原始报文、签名或加密字段，完成验证后再进入业务状态流转，并使用渠道规定的回执。
- 渠道适配必须覆盖真实下单返回、重复请求、异步通知重试、晚到款项对账、分账结果查询；当前 `processing` 只表示提交已受理，不证明到账。
- 验收证据须包含沙箱支付流水、合法与非法回调、订单状态、交付签收和渠道分账结果。现有 MySQL 测试仅验证内部业务链路。

## 托管订单（S2，`fulfillment_mode=managed`）

接口与报文不变，以下为托管订单的差异（docs/25 §25.5.1、§25.9；人工订单不受影响）：

- **收款确认触发开通**：回调确认收款（payment → `paid`、订单 → `paid`）的**同一事务**内调用托管收款钩子：租约 `reserved → pending` 并入队开通任务。钩子未接入（`fleet.enabled=false` 却存在托管订单）时确认收款整体失败回滚并报错「托管订单 … 收款钩子未接入(fleet 未启用), 无法确认收款」，不会出现"已收款但不开通"。续租子订单不走此钩子，而走续租落账（T-088，同一事务）：收款前校验租约条件（extend 租约仍在租未到期；restart 预留卡槽仍完整），不满足时不落账、款项排队原路退回；落账时 extend 顺延租约 `access_until`，restart 原订单 → `paid`、租约 → `pending` 并自动重新开通。详见 compute-api.md「托管订单续租（S2 T-088）」。
- **开通失败自动退款**：开通彻底失败时订单 → `cancelled` 且 `lease_start_at=null`，沿用既有「已付但租期未开始即关闭 → 自动排队原路全额退款」逻辑（`refund_status='pending'`，原因「订单已关闭, 款项原路退回」），不新增退款路径；已有 `processing/success` 分账的订单不自动退。
- **分账时点**：托管订单激活（`active`）**不触发分账**，须等租期末结算写入 `orders.settle_ready_at` 后才发起（T-106，见下节）；应结记录照常在收款时生成。续租子订单看母单的 `settle_ready_at`。

### 租期末结算与部分退款（S2 T-106，docs/24 D-10、docs/25 §25.7.6）

**settle_ready_at 写入时点**（fleet 结算任务每分钟一轮，守卫式 `UPDATE … WHERE settle_ready_at IS NULL AND status='completed' AND fulfillment_mode='managed'`，只写根订单）：

1. 根订单 `completed`，且其租约全部已进入 `purging/purged/failed/cancelled`（`failed/cancelled` 出现在清除后 restart 续租开通失败/取消：续租单退款、根订单回到 `completed`，原租期已交付照常分账；从未交付的托管订单开通失败时订单为 `cancelled`，不进入本流程）。**宽限期（grace）内不写**：宽限期内仍可 restart 续租把订单重新打开；`settle_ready_at` 写入后续租报价 / 下单一律 `40900`（`ErrRenewalConflict`，需重新下单）。两者都在根订单行锁下判断，互斥。
2. 续租单全部 `completed/cancelled/refunded`（无待支付 / 退款中的续租）。
3. 租约每段停机都已结束且补偿已落地（`ended_at`、`remedy_applied_at` 均非空）。待退费停机由结算任务兜底提交退款；退费通道未打开时不写并发运营告警（资金继续留在易宝待分账账户）。
4. 链上订单（根 + 续租）的 `payment_refunds` 全部 `success`（已全额退款的支付除外）。

续租子订单不单独写，沿用根订单的值；写入后由 payment 定时任务按订单逐笔分账。

**部分退款 `payment_refunds`**（迁移 027 建表、037 扩展；托管订单专用，全额退款仍走 `payments.refund_status`）：

| 字段 | 说明 |
|---|---|
| `kind` | `outage_compensation`（停机补偿，`source_id`=`lease_outages.id`）/ `supplier_breach`（供给方违约未使用部分，`source_id`=`leases.id`） |
| `order_no` / `payment_id` | 停机所属订单（续租后为续租单）及其已收款支付 |
| `amount` | 分；同一支付所有行（任意状态）累计不得超过实付 |
| `status` | `pending`（已登记，待清算入账后提交）→ `processing`（已向易宝提交，请求号已落库）→ `success` / `failed` |
| `refund_request_id` | 易宝退款请求号（前缀 `P`）；`failed` 重提时换新请求号 |

- **幂等**：`UNIQUE(kind, source_id)`；同一停机段 / 同一租约至多一行，重复调用、并发调用都只调用一次易宝。所有部分退款路径先锁订单族（母单 → 续租单），再锁 `payment_refunds` → `payments` → `settlements`。
- **结果落地**：同步返回 / 定时查询（`processing` 按请求号查）/ 异步通知（`POST /payment/yeepay/notify/refund`，按 `refund_request_id` 路由到部分退款）三路共用一个幂等落地函数。`SUCCESS` 时同事务写 `lease_outages.remedy_applied_at`；`FAILED/CANCEL` 记 `failed` + 运营告警，1 小时后自动换新请求号重提。
- **开关**：仅当 `fleet.enabled=true` 且 `fleet.outage.yeepay_partial_refund_confirmed=true`（易宝部分退款能力已书面确认）时 main 打开通道；易宝未接入时启动失败（不降级为"假装可退"）。未打开时不登记、不提交，选退费返回 `50000`；已在 `processing` 的仍会查询落地。
- **登记条件**：根订单为托管且 `settle_ready_at IS NULL`、支付 `paid` 且未进入全额退款、本订单无 `processing/success` 分账。已进入分账后不能再部分退款。
- **全额退款扣除已成功部分退款**：之后的全额退款金额 = 实付 − 已成功部分退款；有 `processing` 部分退款时全额退款等待；`pending/failed` 的部分退款在全额退款成功后作废（记 `failed`，原因「订单已全额退款, 本笔不再提交」）。

**分账金额（托管订单）**。记收款时锁定的平台行为 `platBase`、供给方行为 `supBase`（`platBase + supBase = 实付 paid`），已成功部分退款中供给方责任（含违约退款）合计 S、平台责任合计 P：

```
platDed1 = floor(S × platBase ÷ paid)          // 供给方责任退款按锁定比例分摊(保守口径: 佣金同比例减少)
supDed1  = S − platDed1
fromPlat = min(P, platBase − platDed1)         // 平台责任先从平台费里扣
overflow = P − fromPlat                         // 平台费不够扣的部分
平台应结 = platBase − platDed1 − fromPlat
供给方应结 = supBase − supDed1 − overflow;  platform_compensation = overflow
```

`overflow` 是平台责任却从待分账资金里退给了买家、因而从供给方应结里扣下的钱，记在供给方行 `platform_compensation`，由平台另行补偿给供给方（记账，不走易宝二清）。`settlements.refund_deduction` 记本行被扣减的金额（锁定应结 = `amount + refund_deduction`）。重算在分账事务内用守卫式 UPDATE 完成，并校验 **不变量：部分退款(成功)合计 + 两行应结 = 实付**，不成立时不分账并报错。两行合计为 0（实付已全部部分退款）时两行置 `cancelled`，不调用分账。

示例（实付 72000、平台费 7200、供给方 64800，单位分）：

| 场景 | 退款 | 平台应结 | 供给方应结 | 平台补偿 | 校验 |
|---|---|---|---|---|---|
| 无停机 | 0 | 7200 | 64800 | 0 | 0+7200+64800=72000 |
| 供给方责任停机 13 分钟（2h 租期，按实付×秒数÷租期秒数=7800） | 7800 | 7200−780=6420 | 64800−7020=57780 | 0 | 7800+6420+57780=72000 |
| 供给方 3000 + 平台责任 5000、4000 | 12000 | 7200−300−6900=0 | 64800−2700−2100=60000 | 2100 | 12000+0+60000=72000 |

**发票**：可开票金额 = 订单金额 − 已成功部分退款；存在未成功的部分退款时该订单暂不可开票；已开票后再发生部分退款的，按既有红冲流程处理（不自动红冲）。

运营查看：`GET /admin/payment/partial-refunds`（见 admin-api.md）。
