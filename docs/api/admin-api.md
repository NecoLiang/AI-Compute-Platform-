# 运营管理 Admin API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 需 operator 或 admin 角色

---

## 审核中心

### GET /admin/audits/qualifications · 资质审核列表

默认返回待审核记录；传 `status=all` 返回包含已通过、已驳回在内的完整审核台账。

### GET /admin/audits/qualifications/:id/document · 下载申请附件
### POST /admin/audits/qualifications/:id/approve · 通过
### POST /admin/audits/qualifications/:id/reject · 驳回

`supplier_onboarding` 类型为供给方入驻申请；通过时服务端会在同一事务中授予申请账户 `supplier` 角色并写入审计日志。

```
curl -X POST http://localhost:8080/api/v1/admin/audits/qualifications/1/reject \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"reason":"证照已过期，请更新后重新提交"}'
```

### POST /admin/audits/products/:id/approve · 通过商品审核
### POST /admin/audits/products/:id/reject · 驳回商品审核

---

## 订单管理

### GET /admin/orders · 全平台订单
```
curl "http://localhost:8080/api/v1/admin/orders?status=active&page=1&page_size=20" \
  -H "Authorization: Bearer <token>"
```

### PATCH /admin/orders/:id/status · 订单干预
```
curl -X PATCH http://localhost:8080/api/v1/admin/orders/ORD001/status \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"status":"frozen"}'
```

---

## 商品管理

### GET /admin/products · 全平台商品
### PATCH /admin/products/:id/offline · 强制下架

---

## 风控工作台

### GET /admin/risk/alerts · 告警列表

```
curl "http://localhost:8080/api/v1/admin/risk/alerts?level=high&page=1&page_size=20" \
  -H "Authorization: Bearer <token>"
```

**响应**
```json
{"code":0,"data":{"list":[{
  "id":1,"level":"high","alert_type":"suspected_mining",
  "target_type":"order","target_id":32,"status":"pending"
}],"total":1}}
```

### POST /admin/risk/alerts/:id/freeze · 冻结处置
### POST /admin/risk/alerts/:id/dismiss · 标记误报

冻结接口无请求体，按告警保存的 `target_type/target_id` 解析对象：`order` 冻结订单并吊销凭证；`user/account` 冻结账户并使已有会话失效。不能冻结当前操作人。对象/告警不存在返回 `40400`，无效类型返回 `40001`，状态冲突返回 `40900`。

目标冻结、审计与告警 `processing` 在同一 MySQL 事务提交；账户会话失效处理成功后才变为 `resolved`。会话失效处理失败返回 `50000`，保持 `processing`，允许重试冻结；数据库账户状态也会阻止已有令牌访问。重复冻结不会重复写冻结审计。`dismiss` 只允许 pending，重复 dismissed 幂等，不允许忽略 processing/resolved。


---

## 用户管理

### GET /admin/users · 用户列表
### PATCH /admin/users/:id/freeze · 冻结用户

---

## 审计与配置

### GET /admin/audit-logs · 审计日志

```
curl "http://localhost:8080/api/v1/admin/audit-logs?page=1&page_size=20" \
  -H "Authorization: Bearer <token>"
```

### GET /admin/config · 系统配置
```json
{"code":0,"data":{"trading_enabled":true,"fee_rate":500}}
```

### PUT /admin/config · 更新配置（合规开关）

配置写入 `system_config`，服务重启后保持，并同步记录审计日志。GET/PUT 均允许 operator/admin。

- `trading_enabled=false` 阻止新订单创建，返回 `40900`；已有订单支付、交付、退款继续按原流程处理。
- `fee_rate` 是 0–10000 的整数基点，PUT 的 value 使用字符串，例如 `{"key":"fee_rate","value":"650"}` 表示 6.5%。订单创建事务读取并锁定配置，使用整数分计算费用；既有订单费用及结算不随配置变化。
- 买家预览使用公开的 `GET /trading-config`，返回同样两个字段，`Cache-Control: no-store`。读取失败、字段缺失或值非法时返回 HTTP 503 / code 50000；前端禁止提交，不能回退为 5% 或开放交易。

```
curl -X PUT http://localhost:8080/api/v1/admin/config \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"key":"trading_enabled","value":"false"}'
```

---

## 内容管理

### GET /admin/cms/notices · 已发布公告
### POST /admin/cms/notices · 发布公告

公告与审计日志在同一事务中写入；发布成功后可由 GET 接口立即回读。
```json
{"content":"<p>系统维护通知</p>"}
```

---

## 告警级别

| level | 含义 |
|-------|------|
| high | 🔴 高危（如疑似挖矿） |
| medium | 🟠 中等 |
| low | 🟡 低 |
