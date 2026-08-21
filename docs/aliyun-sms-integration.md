# 阿里云短信验证码登录/注册接入调研

> 调研日期：2026-08-18
> 范围：阿里云短信服务 `Dysmsapi/2017-05-25`，中国内地手机号，Go 服务端
> 状态：官方资料与实现依据；未调用计费短信接口

## 1. 结论

用户提供的 [`CreateSmsSign`](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-createsmssign) 是**申请短信签名**的控制面接口，不是业务运行时发送接口。签名和模板应在上线前完成申请、审核与运营商实名制报备；服务端运行时只调用 [`SendSms`](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-sendsms)，不应持有创建或修改签名、模板的权限。

本项目已有 `POST /api/v1/auth/register` 和 `sms_code` 入参，但 `backend/internal/auth/service.go` 当前会拒绝所有验证码。最小接入应为：

1. 新增发送验证码接口，区分 `register`、`login` 用途。
2. 使用 `crypto/rand` 生成 6 位数字验证码，调用阿里云 `SendSms`。
3. Redis 保存验证码摘要、用途、有效期和失败次数；校验成功后原子删除。
4. 注册沿用现有 `agree_tos` 和密码流程；短信登录仅允许已有用户登录，不隐式注册，避免绕过用户协议确认。
5. 对手机号、IP、用途同时限流，并配置阿里云控制台防盗刷、用量预警和硬限额。

第 2～5 项是结合本项目现状给出的工程建议，不是阿里云 API 的强制字段。

## 2. 上线前必须完成的控制台工作

### 2.1 资质、签名、模板和报备

- 开通短信服务，并保证账户余额或套餐包可用；未开通、停机或余额不足分别可能返回 `isv.PRODUCT_UN_SUBSCRIPT`、`isv.OUT_OF_SERVICE`、`isv.AMOUNT_NOT_ENOUGH`。[官方错误码](https://help.aliyun.com/zh/sms/developer-reference/api-error-codes)
- 国内短信需先准备审核通过的企业资质，再申请签名；发送时必须使用审核通过的 `SignName`。[申请短信签名](https://help.aliyun.com/zh/sms/user-guide/create-signatures)
- 模板应在签名通过后申请，发送时必须使用审核通过的 `TemplateCode`。[申请短信模板](https://help.aliyun.com/zh/sms/user-guide/create-message-templates-1/)
- 所有短信签名必须完成运营商实名制报备，否则可能返回 `PORT_NOT_REGISTERED`。阿里云建议新业务至少提前 10 个工作日准备报备。[签名实名制报备](https://help.aliyun.com/zh/sms/user-guide/real-name-reporting-of-sms-sign-name)

业务运行时只配置审核通过的签名名称和模板 Code。`CreateSmsSign`、`CreateSmsTemplate`、`Update*`、`Delete*` 均不属于线上应用权限。

### 2.2 验证码模板

验证码模板用于登录、注册等安全验证场景。国内验证码内容必须包含“验证码、注册码、校验码、动态码（动态密码）”之一，并体现平台、用途、失效时间中的至少一项；验证码变量为 4～6 位数字或字母。模板不得包含营销内容、退订文案、联系方式或链接。[验证码模板规范](https://help.aliyun.com/zh/sms/user-guide/verification-code-template-specifications/)

建议申请单变量模板：

```text
您的验证码${code}，该验证码5分钟内有效，请勿泄露给他人！
```

Compute Exchange 涉及金融业务语境，模板必须保持为纯登录/注册验证内容。阿里云明确说明“他用”签名不支持申请与金融相关的验证码模板，因此优先使用本企业自用资质和企业签名。[验证码模板规范](https://help.aliyun.com/zh/sms/user-guide/verification-code-template-specifications/)

## 3. 生产凭据与最小权限

阿里云推荐使用 RAM 用户或 RAM 角色，不使用主账号直接调用 OpenAPI；在 ECS 上应优先绑定实例 RAM 角色，通过 STS 临时凭据和 SDK 默认凭据链调用，避免把永久 AccessKey 放进容器。[短信服务身份管理](https://help.aliyun.com/zh/sms/identity-management)、[Go SDK 凭据管理](https://help.aliyun.com/zh/sdk/developer-reference/v2-manage-go-access-credentials)、[ECS 身份与访问安全](https://help.aliyun.com/zh/ecs/user-guide/identity-and-access-security/)

`SendSms` 的权限点是 `dysms:SendSms`，且不支持资源级授权，因此最小 RAM 策略为：[SendSms 授权信息](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-sendsms)

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["dysms:SendSms"],
      "Resource": "*"
    }
  ]
}
```

如果后续由应用调用 `QuerySendDetails`，再单独加入对应权限；不要为了快速接入直接给线上应用 `AliyunDysmsFullAccess`。

凭据顺序注意事项：Go SDK 默认凭据链会先读取 `ALIBABA_CLOUD_ACCESS_KEY_ID`、`ALIBABA_CLOUD_ACCESS_KEY_SECRET`（以及可选的 `ALIBABA_CLOUD_SECURITY_TOKEN`），之后才尝试 ECS RAM 角色。因此服务器残留的静态 AK 环境变量会覆盖实例角色。[Go SDK 默认凭据链](https://help.aliyun.com/zh/sdk/developer-reference/v2-manage-go-access-credentials)

生产建议：

- ECS 绑定仅含 `dysms:SendSms` 的实例 RAM 角色。
- 容器不注入永久 AK；使用 `credentials.NewCredential(nil)`。
- 可设置 `ALIBABA_CLOUD_ECS_METADATA` 指定角色，并设置 `ALIBABA_CLOUD_IMDSV1_DISABLED=true` 强制 IMDSv2。[Go SDK ECS RAM 角色凭据](https://help.aliyun.com/zh/sdk/developer-reference/v2-manage-go-access-credentials)
- 若暂时无法使用实例角色，退回到专用 RAM 用户 AK：环境变量注入、最小权限、定期轮转，绝不写入仓库或日志。[AccessKey 安全建议](https://help.aliyun.com/zh/ram/support/faq-about-accesskey-pairs)

## 4. `SendSms` 请求与响应

中国站全局 Endpoint 为 `dysmsapi.aliyuncs.com`，API 版本为 `2017-05-25`，RPC 风格，支持 GET/POST，官方推荐 POST。[集成概览](https://help.aliyun.com/zh/sms/developer-reference/using-openapi/)、[SendSms](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-sendsms)

验证码应每次只向一个号码发送。`SendSms` 虽支持最多 1000 个号码使用相同签名和模板变量，但批量发送有延迟，官方建议验证码单条发送。[SendSms 接口说明](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-sendsms)

| 字段 | 必填 | 本项目用法 |
|---|:---:|---|
| `PhoneNumbers` | 是 | 单个中国内地手机号 |
| `SignName` | 是 | 已审核并完成报备的签名名称 |
| `TemplateCode` | 是 | 已审核的验证码模板 Code |
| `TemplateParam` | 模板含变量时是 | JSON 字符串，如 `{"code":"123456"}` |
| `SmsUpExtendCode` | 否 | 不接入 |
| `OutId` | 否 | 首版不接入 |

字段定义来源：[SendSms 请求参数](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-sendsms)

返回字段为 `Code`、`Message`、`BizId`、`RequestId`。只有 `Code == "OK"` 表示请求提交成功；这不等于运营商最终送达，`BizId` 可用于 `QuerySendDetails` 查询状态。[SendSms 返回参数](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-sendsms)

`SendSms` 是计费接口且不支持幂等。发生超时后不能自动重试，否则可能重复发送；官方建议先检查发送/回执状态再决定是否重试，国内短信超时至少设置 1 秒。[SendSms 注意事项](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-sendsms)

`SendSms` 默认配额为 5000 QPS，但这是 OpenAPI 配额，不是验证码业务安全额度。[Dysmsapi 流控信息](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-quota)

## 5. Go SDK 最小调用

阿里云短信 V1.0 SDK 已停止维护，官方推荐 V2.0 SDK；国内短信使用 `dysmsapi20170525`。当前官方 Go 仓库安装路径为 `v5`。[短信 SDK 参考](https://help.aliyun.com/zh/sms/developer-reference/sdk-product-overview/)、[官方 Go SDK 仓库](https://github.com/alibabacloud-go/dysmsapi-20170525/)

```bash
go get github.com/alibabacloud-go/dysmsapi-20170525/v5
go get github.com/alibabacloud-go/darabonba-openapi/v2
go get github.com/aliyun/credentials-go
```

以下示例只演示 SDK 调用边界，不包含验证码生成、Redis、限流和 HTTP Handler：

```go
package sms

import (
	"encoding/json"
	"fmt"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	dysmsapi "github.com/alibabacloud-go/dysmsapi-20170525/v5/client"
	"github.com/aliyun/credentials-go/credentials"
)

func SendCode(phone, code, signName, templateCode string) error {
	credential, err := credentials.NewCredential(nil)
	if err != nil {
		return err
	}

	client, err := dysmsapi.NewClient(new(openapi.Config).
		SetCredential(credential).
		SetEndpoint("dysmsapi.aliyuncs.com"))
	if err != nil {
		return err
	}

	templateParam, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return err
	}

	resp, err := client.SendSms(new(dysmsapi.SendSmsRequest).
		SetPhoneNumbers(phone).
		SetSignName(signName).
		SetTemplateCode(templateCode).
		SetTemplateParam(string(templateParam)))
	if err != nil {
		return err
	}
	if resp == nil || resp.Body == nil || resp.Body.Code == nil || *resp.Body.Code != "OK" {
		return fmt.Errorf("aliyun sms request rejected")
	}
	return nil
}
```

类型和方法可在官方源码核对：[`SendSmsRequest`](https://github.com/alibabacloud-go/dysmsapi-20170525/blob/master/client/send_sms_request_model.go)、[`Client.SendSms`](https://github.com/alibabacloud-go/dysmsapi-20170525/blob/master/client/client.go)。正式实现应保留 `Code`、`RequestId`、`BizId` 供排障，但不得记录验证码、完整手机号、AccessKey 或 STS Token。

## 6. 本项目建议的接口和 Redis 状态

### 6.1 HTTP 接口

建议新增：

```text
POST /api/v1/auth/sms/code
{"phone":"13800138000","purpose":"register"}

POST /api/v1/auth/sms/login
{"phone":"13800138000","sms_code":"123456"}
```

现有注册接口保持：

```text
POST /api/v1/auth/register
{"phone":"13800138000","sms_code":"123456","password":"Abc12345!","agree_tos":true}
```

发送接口无论手机号是否存在都返回同一类响应，避免泄露账号状态；服务端内部根据用途判断：

- `register`：手机号已存在时不发送，但对外仍返回通用响应。
- `login`：手机号不存在时不发送，但对外仍返回通用响应。
- 短信登录不自动注册；注册必须经过 `agree_tos`。

### 6.2 Redis 键

项目已使用 Redis 7，首版无需新表：

```text
auth:sms:cooldown:{purpose}:{phone_hash}  TTL 60s
auth:sms:code:{purpose}:{phone_hash}      TTL 300s
auth:sms:attempts:{purpose}:{phone_hash}  TTL 300s
auth:sms:phone-hour:{phone_hash}          TTL 1h
auth:sms:phone-day:{phone_hash}           TTL 24h
auth:sms:ip-minute:{ip_hash}              TTL 1m
auth:sms:ip-hour:{ip_hash}                TTL 1h
```

实现约束：

- 使用 `crypto/rand` 生成 6 位数字码，不能使用 `math/rand`。
- Redis 不存验证码明文；存 `HMAC-SHA256(server_secret, purpose|phone|code)`。
- 发送前用 `SET NX EX` 抢占 60 秒冷却锁，防止并发请求产生多条计费短信。
- 仅当 `Code == OK` 时写入新的验证码摘要；新验证码覆盖旧验证码。
- 校验最多允许 5 次失败；成功后用 Redis 原子操作消费验证码，防止重放。
- 阿里云调用超时不自动重发，保留冷却锁并返回通用失败响应。
- 日志仅记录脱敏手机号、用途、阿里云 `Code`、`RequestId`、`BizId`；不记录验证码和凭据。

## 7. 频控、防盗刷与合规门槛

阿里云对中国内地验证码的默认频控为：同签名、同号码 1 条/分钟、5 条/小时、10 条/天；同一号码跨多个短信发送方最多 40 条/天。[短信发送规则](https://help.aliyun.com/zh/sms/user-guide/message-rules/)

业务侧至少采用相同或更严格的手机号限制，并同时限制 IP。阿里云官方建议验证码最小获取间隔一般为 60 秒、设置有效期、限制请求 IP 和手机号，并在异常流量场景接入图形验证码或验证码 2.0；控制台还应开启防盗刷监控、发送频率限制及日/月用量预警和硬限额。[验证码防盗刷](https://help.aliyun.com/zh/sms/user-guide/verification-code-scams-and-message-flooding-1)、[设置短信发送频率](https://help.aliyun.com/zh/sms/user-guide/configure-delivery-frequency-and-whitelist/)

建议首版阈值：

| 维度 | 阈值 |
|---|---|
| 同手机号 | 1 次/分钟、5 次/小时、10 次/24 小时 |
| 同 IP | 10 次/分钟、50 次/小时 |
| 验证码有效期 | 5 分钟 |
| 错误尝试 | 5 次后验证码作废 |

IP 阈值是本项目初始建议，应依据真实流量调整；不能用阿里云 5000 QPS 配额代替业务限流。

## 8. 错误码映射

服务端不应把阿里云原始 `Message` 直接返回客户端；记录内部诊断信息，对外使用稳定业务错误码。[国内消息 API 错误码](https://help.aliyun.com/zh/sms/developer-reference/api-error-codes)

| 阿里云 `Code` | 含义 | 建议处理 |
|---|---|---|
| `OK` | 请求提交成功 | 写入验证码摘要；不视为最终送达 |
| `isv.MOBILE_NUMBER_ILLEGAL` | 手机号格式错误 | HTTP 400 / 项目参数错误 |
| `isv.BUSINESS_LIMIT_CONTROL` | 短信频控 | HTTP 429，不重试 |
| `isv.DAY_LIMIT_CONTROL` / `isv.MONTH_LIMIT_CONTROL` | 总量限额 | HTTP 503，告警 |
| `isv.SMS_SIGNATURE_ILLEGAL` / `isv.SIGN_STATE_ILLEGAL` | 签名错误或不可用 | HTTP 503，告警 |
| `isv.SMS_TEMPLATE_ILLEGAL` / `isv.TEMPLATE_MISSING_PARAMETERS` | 模板或变量错误 | HTTP 503，告警 |
| `isp.RAM_PERMISSION_DENY` | RAM 权限不足 | HTTP 503，告警 |
| `PORT_NOT_REGISTERED` | 运营商实名制报备未完成 | HTTP 503，告警 |
| `isv.OUT_OF_SERVICE` / `isv.AMOUNT_NOT_ENOUGH` | 停机或余额不足 | HTTP 503，告警 |
| `isp.SYSTEM_ERROR` | 阿里云系统错误 | HTTP 503；因接口非幂等，不自动重发 |

## 9. 实施顺序与验收

1. 控制台完成企业资质、签名、验证码模板、运营商实名制报备。
2. 给 ECS 绑定最小权限实例 RAM 角色；确认容器不存在覆盖角色的静态 AK 环境变量。
3. 引入官方 Go SDK，封装单一 `SendCode` 调用边界。
4. 实现发送接口、Redis 验证码状态、手机号/IP 限流和一次性消费。
5. 接入现有注册流程，并新增“已有用户短信登录”；保留密码登录。
6. 使用阿里云已绑定的测试手机号做小流量端到端测试，再启用正式签名和模板。
7. 验收发送成功、错误码映射、过期、错误次数、并发重复请求、限流、验证码重放、阿里云超时不重试以及日志脱敏。

上线门槛：签名和模板审核通过、运营商报备可用、实例角色生效、控制台防盗刷与用量限额已配置、服务端限流和一次性验证码测试通过。未满足任一项时，注册接口继续保持当前的“拒绝所有验证码”安全行为。

## 10. 本地联调

1. 复制 `backend/.env.sms.example` 为 `backend/.env.sms.local`，填入专用 RAM 用户的 AK、已审核签名和登录/注册模板 Code；`.env.sms.local` 已被 Git 忽略。
2. 在 `backend/` 运行 `docker compose up -d --build app`。Compose 会把本地文件注入容器，Viper 读取 `SMS_*`，阿里云 SDK 默认凭据链读取 `ALIBABA_CLOUD_*`。
3. 先确认容器健康且日志不包含验证码、完整手机号或凭据，再使用已绑定的测试手机号执行单条发送。`SendSms` 会计费且不幂等，超时后不自动重发。
4. 联调需同时验证：Cap token 一次性消费、60 秒冷却、验证码写入 Redis 后不以明文保存、正确验证码登录/注册后原子删除、错误验证码累计 5 次后失效。
