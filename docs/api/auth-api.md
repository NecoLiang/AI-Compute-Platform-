# 认证 Auth API

微信网站扫码登录、首次手机号绑定和配置见 [微信登录](wechat-login-api.md)。

**Base**: `http://localhost:8080/api/v1` | **Auth**: `/auth/me` 需 `Bearer <token>`

---

## POST /auth/sms/code · 获取短信验证码

登录用途下，未注册手机号在通过 Cap 后返回业务码 `40400`，提示「该手机号尚未注册，请先注册」，不发送验证码。该请求仍受手机号冷却与 IP 频率限制；前端显示错误并保留注册入口，不开始发送成功倒计时。注册用途的已存在账户仍返回 `40900`。


`captcha_token` 由 Cap programmatic mode 生成，只能在本接口消费一次。

```json
{"phone":"13800138000","purpose":"login","captcha_token":"..."}
```

**成功** `200`
```json
{"code":0,"message":"success","data":{"expires_in":300,"resend_after":60}}
```

本地 Docker 的 debug preview 模式会额外返回 `data.preview_code`，用于本地联调；release 模式会拒绝启用该能力。

注册用途的手机号已存在时不会发送验证码，并返回：
```json
{"code":40900,"message":"用户已存在"}
```

`expires_in` 是验证码有效期，`resend_after` 是可重新获取的倒计时，两者不可混用。

---

## POST /auth/register · 手机号验证码注册并建立会话

```
curl -X POST http://localhost:8080/api/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{"phone":"13800138000","sms_code":"123456","agree_tos":true,"terms_version":"2026-09-06.1","privacy_version":"2026-09-06.1"}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| phone | string | ✅ | 手机号 |
| sms_code | string | ✅ | 短信验证码 |
| agree_tos | bool | ✅ | 已阅读并同意用户服务协议及隐私政策，必须为 true |
| terms_version | string | ✅ | 当前用户服务协议版本：`2026-09-06.1` |
| privacy_version | string | ✅ | 当前隐私政策版本：`2026-09-06.1` |

**成功** `200`
```json
{"code":0,"message":"success","data":{
  "access_token":"eyJ...","refresh_token":"eyJ...","expires_in":900,
  "user":{"id":1,"phone":"138****8000","roles":["buyer"]}
}}
```

---

## POST /auth/sms/login · 手机号验证码登录

```
curl -X POST http://localhost:8080/api/v1/auth/sms/login \
  -H "Content-Type: application/json" \
  -d '{"phone":"13800138000","sms_code":"123456"}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| phone | string | ✅ | 已注册手机号 |
| sms_code | string | ✅ | 登录用途的短信验证码 |

**成功** `200`
```json
{"code":0,"data":{
  "access_token":"eyJ...","refresh_token":"eyJ...","expires_in":900,
  "user":{"id":1,"phone":"138****8000","roles":["buyer"]}
}}
```
> 浏览器前端通过同源 BFF 将 token 写入 HttpOnly Cookie，不把 token 暴露给客户端状态或 localStorage。账号密码登录尚未开放公开路由。

---

## POST /auth/refresh · 刷新 Token

`refresh_token` 仅能成功使用一次，刷新后必须使用新 token。

```
curl -X POST http://localhost:8080/api/v1/auth/refresh \
  -H "Content-Type: application/json" \
  -d '{"refresh_token":"eyJ..."}'
```

---

## POST /auth/logout · 登出

```
curl -X POST http://localhost:8080/api/v1/auth/logout \
  -H "Authorization: Bearer <access_token>" \
  -H "Content-Type: application/json" \
  -d '{"refresh_token":"eyJ..."}'
```

---

## GET /auth/me · 当前用户

```
curl http://localhost:8080/api/v1/auth/me \
  -H "Authorization: Bearer <access_token>"
```

**响应**
```json
{"code":0,"data":{"id":1,"phone":"138****8000","email":"","roles":["buyer"]}}
```

---

## POST /user/kyc/enterprise · 企业认证

使用 `multipart/form-data` 提交企业名称、统一社会信用代码、法定代表人及证件号、对公账户信息和营业执照文件。营业执照支持 PDF/JPG/PNG，最大 5MB；完整申请资料与文件写入 MySQL。试运行阶段不调用外部核验服务，提交后状态直接写为 `verified`。

```bash
curl -X POST http://localhost:8080/api/v1/user/kyc/enterprise \
  -H "Authorization: Bearer <access_token>" \
  -F 'enterprise_name=某科技有限公司' \
  -F 'uscc=91110108MA0123456X' \
  -F 'legal_person=张三' \
  -F 'legal_person_id_card=110101199001011234' \
  -F 'bank_name=招商银行北京中关村支行' \
  -F 'bank_account_name=某科技有限公司' \
  -F 'bank_account_number=6225888888888888' \
  -F 'sensitive_data_agreed=true' \
  -F 'privacy_version=2026-09-06.1' \
  -F 'business_license=@./license.pdf'
```

---

## 通用错误

| code | 说明 |
|:----:|------|
| 40001 | 参数错误 |
| 40100 | 未认证/token无效 |
| 40101 | token过期 |
| 40300 | 账号被冻结 |
| 40900 | 手机号已注册 |
| 42900 | 请求过于频繁 |

## 协议同意记录（2026-09-06）

注册必须传入两个当前协议版本并明确同意。缺失、旧版本或未同意返回 `40001`；在短信校验前拒绝，不消费有效短信验证码。两条同意记录与用户、默认 buyer 角色在同一数据库事务提交。既有账户不补造历史同意。

`GET /auth/consents`（前端 BFF：`GET /api/auth/consents`）需登录，返回当前用户最近 100 条记录（ID 倒序），不接受指定其他用户。`data` 为数组，每项包含 `document`、`version`、`action`、`reference`、`accepted_at`（带时区的服务端时间）。记录范围含注册、个人/企业认证、商品发布/重提和下单。

参见 [协议页面与同意契约](legal-consent-api.md)。

## 认证敏感信息的单独同意

个人认证 JSON 和企业认证 multipart 均必填 `sensitive_data_agreed=true` 与 `privacy_version="2026-09-06.1"`（multipart 使用字符串 `true`）。注册时同意隐私政策不代替本次认证授权；身份真实性确认也不代替此同意。缺失、未同意或旧版本返回 `40001`，不保存申请。认证同意与申请同一事务保存，记录文档为 `privacy`，操作为 `kyc_personal` / `kyc_enterprise`，引用为用户 ID；存储失败时申请回滚。重复的已提交/已认证申请返回 `40900`，不追加记录。既有认证状态不回填同意。
