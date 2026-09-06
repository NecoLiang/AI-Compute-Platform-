# 微信网站扫码登录

本功能使用微信开放平台的「网站应用」，scope 为 `snsapi_login`。公众号网页授权、企业微信和小程序不在本次范围。

## 配置与启用

1. 准备审核通过、已获得微信登录能力的网站应用。在开放平台填写授权回调域名（例如 `omnisline.com`）。
2. 先在目标数据库执行 `backend/migrations/018_wechat_login.up.sql`。生产迁移需要先备份并单独批准；本次没有执行生产迁移。
3. 仅后端配置 `WECHAT_APP_ID`、`WECHAT_APP_SECRET`、`WECHAT_CALLBACK_URL`。三项必须齐全，否则启动时报错；三项全空则保持禁用。
4. 正式回调 URL 为 `https://omnisline.com/api/auth/wechat/callback`，必须指向前端 BFF，与微信登记域名一致。debug 可使用 localhost HTTP，但微信仍要求可访问且匹配的授权域名。
5. 本地 Docker 可复制 `backend/.env.wechat.example` 到已忽略的 `.env.wechat.local`，自行填写；生产使用已有私有部署环境文件。AppSecret 不进入前端变量、仓库或聊天。
6. 重启受影响服务后，登录页依据后端能力状态自动开放微信按钮。配置存在不代表微信平台审核通过；上线前仍须真实手机扫码验证。

## 用户流程

- 点击微信登录，跳转微信官方扫码页。授权后回到 BFF，由后端交换 code；微信 access_token 不发送到浏览器，也不持久化。
- 已绑定：读取当前平台账户状态和角色，签发原有 JWT，由 BFF 写入 HttpOnly Cookie，沿用 `next` 与角色路由规则。
- 未绑定：BFF 保存 10 分钟 HttpOnly 绑定票据，跳转 `/auth/wechat/bind`。用户明确选择「登录并绑定微信」或「创建账户并绑定微信」，复用短信验证和注册条款。
- 绑定前先完成短信登录或注册。若后续绑定失败，短信账户/会话仍有效，界面显示失败并要求重新扫码；不会删除账户或覆盖已有绑定。
- `(app_id, openid)` 唯一绑定账户；同一账户在一个 app_id 下只绑定一个微信。UnionID 仅记录，不自动跨应用合并账户。绑定不授予额外角色、不改变认证或商家资质。

## 服务端 HTTP 契约

Base: `/api/v1`。响应采用 `{code,message,data,request_id}`，必须检查业务 `code`。

| 方法 | 路径 | 请求 | 成功 data |
|---|---|---|---|
| GET | `/auth/wechat/status` | 无，公开 | `{enabled: boolean}` |
| POST | `/auth/wechat/start` | `{browser_verifier: "64位随机hex"}`，公开 | `{authorize_url: "https://open.weixin.qq.com/connect/qrconnect?..."}` |
| POST | `/auth/wechat/exchange` | `{code,state,browser_verifier}`，公开 | 已绑定返回与短信登录一致的 token 和 user；未绑定返回 `{binding_required:true,binding_ticket:"64位随机hex"}` |
| POST | `/auth/wechat/bind` | Bearer JWT + `{binding_ticket}` | 无 data |

BFF 提供 `GET/POST /api/auth/wechat`、`GET /api/auth/wechat/callback`、`POST /api/auth/wechat/bind`。创建授权和绑定要求同源 Origin。浏览器 verifier、绑定票据和 JWT 均保存在 HttpOnly Cookie，不出现在跳转 URL/localStorage。

state 与浏览器 verifier 绑定，Redis 原子校验后消费，有效期 10 分钟。绑定票据也原子消费、10 分钟过期。数据库唯一约束防止并发绑定覆盖。

业务错误：`40100` 授权无效/过期/重放，`40300` 未配置或账户冻结，`40900` 绑定冲突，`50000` 上游/存储不可用。受保护绑定接口未登录还会返回 HTTP 401。微信请求 URL 含 AppSecret，网络错误必须去除原始 URL 后再记录。

## 验证

- 后端 `go test ./...`。设置 `TEST_MYSQL_DSN` 和 `TEST_REDIS_ADDR` 后，`go test ./internal/auth -run TestWeChat -count=1` 使用独立临时数据库测试完整绑定和再登录，覆盖回调浏览器不匹配、重放、绑定冲突、上游拒绝和冻结账号。微信 HTTP 响应为模拟；不向微信发送测试凭据。
- 前端 `pnpm check`，包含跳转地址/Origin 校验与短信绑定调用顺序检查；构建后运行 `node scripts/check-wechat-bff.mjs`，启动临时 Next.js 服务和本地上游模拟，检查真实 BFF Cookie、回跳与 HTTPS 代理行为。
- 真实扫码必须在平台审核和私有配置完成后验证；自动化测试不能替代该步骤。

参考：[微信开放平台网站应用登录指南](https://developers.weixin.qq.com/doc/oplatform/Website_App/WeChat_Login/Wechat_Login.html)。
