# 认证 Auth API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 除注册/登录/刷新外均需 `Bearer <token>`

---

## POST /auth/register · 注册

> ⚠️ 短信验证码未接入，当前拒绝所有注册请求

```
curl -X POST http://localhost:8080/api/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{"phone":"13800138000","sms_code":"123456","password":"Abc12345!","agree_tos":true}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| phone | string | ✅ | 手机号 |
| sms_code | string | ✅ | 短信验证码 |
| password | string | ✅ | ≥8位 |
| agree_tos | bool | ✅ | 同意用户协议 |

**成功** `200`
```json
{"code":0,"message":"success","data":{"user_id":1},"request_id":"..."}
```
**失败** `200` - 短信服务未接入
```json
{"code":50000,"message":"短信验证码服务未接入: 请配置短信服务商(AccessKey+签名+模板ID)"}
```

---

## POST /auth/login · 登录

```
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"account":"13800138000","password":"Abc12345!"}'
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| account | string | ✅ | 手机号 |
| password | string | ✅ | 密码 |

**成功** `200`
```json
{"code":0,"data":{
  "access_token":"eyJ...","refresh_token":"eyJ...","expires_in":900,
  "user":{"id":1,"phone":"138****8000","roles":["buyer"]}
}}
```
> 前端需存储 `access_token`，后续请求加 `Authorization: Bearer <access_token>`

**测试账号**：`admin` / `admin123`（有 buyer + operator + admin 角色）

---

## POST /auth/refresh · 刷新 Token

```
curl -X POST http://localhost:8080/api/v1/auth/refresh \
  -H "Content-Type: application/json" \
  -d '{"refresh_token":"eyJ..."}'
```

---

## POST /auth/logout · 登出

```
curl -X POST http://localhost:8080/api/v1/auth/logout \
  -H "Authorization: Bearer <access_token>"
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

## 通用错误

| code | 说明 |
|:----:|------|
| 40001 | 参数错误 |
| 40100 | 未认证/token无效 |
| 40101 | token过期 |
| 40300 | 账号被冻结 |
| 40900 | 手机号已注册 |
| 42900 | 请求过于频繁 |
