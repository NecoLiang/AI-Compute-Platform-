# 17 · API 接口设计

> 版本：v1.0 ｜ 日期：2026-07-13 ｜ 状态：待审查 ｜ 产出：ARCH
> Base URL: `https://api.[platform].com/api/v1`
> 本文档定义**全部接口的契约**。开发时按此实现，联调时按此验证。

---

## 1. 通用规范

### 1.1 统一响应
```json
{ "code": 0, "message": "success", "data": {}, "request_id": "uuid" }
```

### 1.2 分页
```
GET /api/v1/orders?page=1&page_size=20
Response: { "code": 0, "data": { "list": [...], "total": 100, "page": 1, "page_size": 20 } }
```

### 1.3 鉴权
- Header: `Authorization: Bearer <access_token>`
- 401: token 无效/过期 → 前端调 `/auth/refresh` 或跳登录
- 403: 无权限

### 1.4 错误码
| 码 | 含义 |
|:--:|------|
| 0 | 成功 |
| 40001 | 参数错误 |
| 40100 | 未认证 |
| 40101 | token 过期 |
| 40300 | 无权限 |
| 40400 | 资源不存在 |
| 40900 | 冲突（如余量不足） |
| 42900 | 请求过于频繁 |
| 50000 | 服务内部错误 |

---

## 2. 认证模块 · `/auth`

| 方法 | 路径 | 说明 | 鉴权 |
|------|------|------|:--:|
| POST | `/auth/register` | 注册（手机/邮箱+验证码+密码） | ❌ |
| POST | `/auth/login` | 登录 → 返回 access_token + refresh_token | ❌ |
| POST | `/auth/refresh` | 刷新 token | ❌（用 refresh_token） |
| POST | `/auth/logout` | 登出 → token 加入黑名单 | ✅ |
| GET | `/auth/me` | 当前用户信息+角色列表 | ✅ |

**POST /auth/register**
```json
// Request
{ "phone": "13800138000", "sms_code": "123456", "password": "Abc12345!", "agree_tos": true }
// Response
{ "code": 0, "data": { "user_id": 1 } }
```

**POST /auth/login**
```json
// Request
{ "account": "13800138000", "password": "Abc12345!" }
// Response
{ "code": 0, "data": {
  "access_token": "eyJ...", "expires_in": 900,
  "refresh_token": "eyJ...",
  "user": { "id": 1, "phone": "138****8000", "roles": ["buyer"] }
}}
```

---

## 3. 用户与 KYC · `/user`

| 方法 | 路径 | 说明 | 鉴权 |
|------|------|------|:--:|
| GET | `/user/profile` | 个人信息 | ✅ |
| PUT | `/user/profile` | 更新个人信息 | ✅ |
| POST | `/user/kyc/personal` | 个人实名认证 | ✅ |
| POST | `/user/kyc/enterprise` | 企业认证 | ✅ |
| GET | `/user/kyc/status` | 认证状态 | ✅ |

**POST /user/kyc/personal**
```json
{ "real_name": "张三", "id_card": "110101199001011234" }
```

当前试运行阶段不调用第三方核验服务，提交后状态直接写为 `verified`。

供给方身份不能由用户直接添加。完成 KYC 后通过 `POST /supplier-applications` 提交入驻资料，Admin 审核通过后由服务端授予 `supplier` 角色。

**POST /user/kyc/enterprise**
```json
{ "enterprise_name": "某科技有限公司", "uscc": "91110108MA0******", "license_url": "/uploads/xxx.jpg", "legal_person": "张三" }
```

---

## 4. 算力撮合 · `/compute`

### 4.1 商品

| 方法 | 路径 | 说明 | 鉴权 | 角色 |
|------|------|------|:--:|------|
| GET | `/products` | 商品列表（筛选+排序+分页） | ❌ | — |
| GET | `/products/:id` | 商品详情+供给方信用 | ❌ | — |
| POST | `/products` | 上架商品（供给方） | ✅ | supplier |
| PUT | `/products/:id` | 编辑商品 | ✅ | supplier |
| PATCH | `/products/:id/status` | 上下架 | ✅ | supplier |
| GET | `/supplier/products` | 我的商品 | ✅ | supplier |
| GET | `/supplier/qualifications` | 我的资质 | ✅ | supplier |
| POST | `/supplier/qualifications` | 提交资质 | ✅ | supplier |
| GET | `/supplier-applications` | 我的供给方入驻申请 | ✅ | 已登录用户 |
| POST | `/supplier-applications` | 提交供给方入驻申请 | ✅ | 已完成 KYC 的用户 |

**GET /products**
```
Query: ?gpu_model=H100&region=北京&pricing_mode=hourly&price_min=20&price_max=50&sort=price_asc&page=1&page_size=20
```

**POST /products**
```json
{
  "gpu_model": "NVIDIA H100 SXM 80GB",
  "card_count": 64,
  "cpu_spec": "2× Intel Xeon 8480+",
  "memory_spec": "2TB DDR5",
  "storage_spec": "30TB NVMe",
  "bandwidth_spec": "10Gbps",
  "delivery_mode": "bare_metal",
  "pricing_mode": "hourly",
  "unit_price": 35.00,
  "available_hours": "全天 24h",
  "stock": 64,
  "min_order": 1,
  "min_duration": 1,
  "region": "北京",
  "compliance_agreed": true
}
```

### 4.2 订单

| 方法 | 路径 | 说明 | 鉴权 | 角色 |
|------|------|------|:--:|------|
| POST | `/orders` | 下单 | ✅ | buyer |
| GET | `/orders` | 我的订单（买家） | ✅ | buyer |
| GET | `/orders/:order_no` | 买家订单详情 | ✅ | buyer |
| POST | `/orders/:id/deliver` | 回填交付凭证（供给方） | ✅ | supplier |
| POST | `/orders/:id/confirm` | 确认签收（买家） | ✅ | buyer |
| POST | `/orders/:id/renew` | 续费 | ✅ | buyer |
| POST | `/orders/:id/refund` | 申请退款 | ✅ | buyer |
| GET | `/supplier/orders` | 我的订单（供给方） | ✅ | supplier |

**POST /orders**
```json
{ "product_id": 1, "quantity": 8, "duration": 1, "compliance_agreed": true }
// Response
{ "code": 0, "data": {
  "order_no": "ORD20260713001", "total_amount": 20160000,
  "platform_fee": 1008000, "status": "pending_payment",
  "payment_expires_at": "2026-07-13T15:00:00Z"
}}
```

**POST /orders/:id/deliver**
```json
{ "ip_address": "10.0.1.128", "ssh_port": 22, "username": "root", "credential_note": "密码已通过站内信发送" }
```

**GET /orders/:order_no（买家视角）**
```json
{ "code": 0, "data": {
  "order": { "order_no": "ORD20260713001", "status": "active", "quantity": 8, "duration": 1,
    "unit_price": 2520000, "total_amount": 20160000, "platform_fee": 1008000 },
  "product": { "id": 1, "product_type": "card_rental", "gpu_model": "H100",
    "card_count": 8, "pricing_mode": "monthly", "region": "北京", "self_operated": false },
  "supplier": { "name": "中联数据", "self_operated": false, "credit": null },
  "delivery": { "access_status": "delivered", "confirmed_by_buyer": true,
    "buyer_confirmed_at": "2026-07-13T15:02:00Z" },
  "actions": { "can_confirm": false, "can_renew": true, "can_refund": true,
    "can_view_credential": true }
}}
```

该接口仅按 JWT 当前用户查询订单，不接受数据库自增 ID，也不返回访问凭证明文。现有数据模型没有订单事件表或不可变商品快照，因此区块链时间线暂不属于本接口，`product` / `supplier` 为当前关联资料。

---

## 5. 支付分账 · `/payment`

| 方法 | 路径 | 说明 | 鉴权 | 角色 |
|------|------|------|:--:|------|
| POST | `/payment/pay` | 发起支付 → 返回易宝收银台 URL | ✅ | buyer |
| POST | `/payment/callback` | 易宝支付回调（验签） | ❌ | — |
| GET | `/payment/status/:order_no` | 查询支付状态 | ✅ | buyer |
| POST | `/payment/supplier/onboard` | 供给方易宝进件 | ✅ | supplier |
| GET | `/payment/supplier/onboard/status` | 进件状态 | ✅ | supplier |
| GET | `/payment/settlements` | 我的分账记录 | ✅ | supplier |
| GET | `/admin/payment/reconcile` | 对账（运营） | ✅ | operator |

**POST /payment/pay**
```json
{ "order_no": "ORD20260713001", "channel": "wechat" }
// Response
{ "code": 0, "data": { "pay_url": "https://yeepay.com/..." } }
```

**易宝回调（平台接收）**
```
POST /payment/callback  (验RSA签 → 更新订单状态 → 触发分账 → 通知供给方)
```

---

## 6. 居间与金融 · `/intermediary`

| 方法 | 路径 | 说明 | 鉴权 | 角色 |
|------|------|------|:--:|------|
| POST | `/leads` | 创建居间线索（设备/施工/融资） | ✅ | buyer |
| GET | `/leads` | 我的线索 | ✅ | 全部 |
| POST | `/leads/:id/quote` | 报价（厂商/运营） | ✅ | vendor/operator |
| POST | `/leads/:id/close` | 成交登记 | ✅ | vendor/operator |
| GET | `/vendor/leads` | 分配给厂商的线索 | ✅ | vendor |
| GET | `/commissions` | 佣金台账 | ✅ | vendor |
| GET | `/finance/lease` | 融资租赁介绍（公开） | ❌ | — |
| POST | `/finance/lease/contact` | 融资租赁留资 | ❌ | — |

**POST /leads**
```json
{ "type": "equipment", "contact_name": "李四", "contact_phone": "139****5678", "description": "需要 20台 H100 服务器", "budget": "5000000" }
```

**POST /leads/:id/close**
```json
{ "deal_amount": 4800000.00, "commission_rate": 3.0, "note": "已签约" }
```

---

## 7. 运营后台 · `/admin`

| 方法 | 路径 | 说明 | 鉴权 | 角色 |
|------|------|------|:--:|------|
| **审核** | | | | |
| GET | `/admin/audits` | 审核列表（?type=supplier|product|vendor） | ✅ | operator |
| GET | `/admin/audits/:id` | 审核详情 | ✅ | operator |
| POST | `/admin/audits/:id/approve` | 通过 | ✅ | operator |
| POST | `/admin/audits/:id/reject` | 驳回（必填原因） | ✅ | operator |
| **管理** | | | | |
| GET | `/admin/products` | 全平台商品 | ✅ | operator |
| PATCH | `/admin/products/:id/offline` | 强制下架 | ✅ | operator |
| GET | `/admin/orders` | 全平台订单 | ✅ | operator |
| PATCH | `/admin/orders/:id/status` | 订单干预 | ✅ | operator |
| GET | `/admin/users` | 用户列表 | ✅ | operator |
| PATCH | `/admin/users/:id/freeze` | 冻结/解冻 | ✅ | operator |
| GET | `/admin/settlements` | 分账管理 | ✅ | operator |
| GET | `/admin/reconcile` | 对账中心 | ✅ | operator |
| **内容** | | | | |
| GET | `/admin/cms/pages` | 页面内容 | ✅ | operator |
| PUT | `/admin/cms/pages/:key` | 编辑页面 | ✅ | operator |
| POST | `/admin/cms/notices` | 发布公告 | ✅ | operator |
| **风控** | | | | |
| GET | `/admin/risk/alerts` | 告警列表 | ✅ | operator |
| POST | `/admin/risk/alerts/:id/freeze` | 冻结（订单/账号） | ✅ | operator |
| POST | `/admin/risk/alerts/:id/dismiss` | 标记误报 | ✅ | operator |
| **配置** | | | | |
| GET | `/admin/config` | 系统配置 | ✅ | admin |
| PUT | `/admin/config` | 更新配置（含合规开关） | ✅ | admin |
| GET | `/admin/audit-logs` | 审计日志 | ✅ | operator |

**POST /admin/audits/:id/reject**
```json
{ "reason": "证照已过期，请更新后重新提交" }
```

**PUT /admin/config**
```json
{ "key": "trading_enabled", "value": "false" }
```

---

## 8. 区块链存证 · `/blockchain`

| 方法 | 路径 | 说明 | 鉴权 |
|------|------|------|:--:|
| GET | `/blockchain/verify` | 验证链上存证（?type=order&id=xxx） | ❌ |
| GET | `/blockchain/attestations/:target_type/:target_id` | 查询存证记录 | ✅ |

**GET /blockchain/verify?type=order&id=123**
```json
{ "code": 0, "data": {
  "verified": true,
  "tx_id": "0x7a3b...c21d",
  "chain_timestamp": "2026-07-13T14:30:18Z",
  "verify_url": "https://bsnscan.com/tx/0x7a3b...c21d"
}}
```

---

## 9. 通用 · `/common`

| 方法 | 路径 | 说明 | 鉴权 |
|------|------|------|:--:|
| POST | `/common/sms/send` | 发送短信验证码 | ❌（有限流） |
| POST | `/common/upload` | 文件上传（证照/附件） | ✅ |

---

## 10. API 文档同步（开发阶段）

DEV 实现每个模块后，同步更新 `docs/api/` 下对应文件：
- `docs/api/auth-api.md`
- `docs/api/compute-api.md`
- `docs/api/payment-api.md`
- `docs/api/intermediary-api.md`
- `docs/api/admin-api.md`
- `docs/api/blockchain-api.md`

各文件为本文档对应章节的**详细版**（含 curl 示例、错误响应示例、业务逻辑说明）。前端联调时以 `docs/api/` 为准。

---

> **审查状态**：待审查 · **审查人**：CR · **日期**：2026-07-13
