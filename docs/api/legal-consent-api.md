# 协议页面与同意契约

版本：`2026-09-06.1`。四个正文页面为无需登录的独立路由：

| 文档键 / 路由 | 文档 | 同意操作 |
|---|---|---|
| `/terms` | 用户服务协议 | 注册 |
| `/privacy` | 隐私政策 | 注册 |
| `/resource-listing-rules` | 算力资源上架规范 | 发布商品、驳回重提 |
| `/resource-usage-rules` | 算力资源使用规范 | 创建订单 |

表单链接附带 `?version=2026-09-06.1` 并在新标签页打开，不丢失输入。未支持的版本返回未找到，不能把新正文冒充旧版本展示。首页页脚提供统一入口。勾选默认关闭，点击正文链接不代替勾选。

## HTTP 请求与记录

- `POST /auth/register`：`agree_tos=true`、`terms_version`、`privacy_version` 都是必填的当前版本。
- `POST /supplier/products`、`PUT /supplier/products/:id`、`POST /orders`：`compliance_agreed=true`、`compliance_version` 必须匹配当前版本。
- 版本在浏览器 adapter 中随当前正文版本发送。BFF 仅转发，不替缺失版本补值；服务端不信任客户端时间。
- 缺失/旧版本/未同意返回业务 `40001`。鉴权、角色、KYC、商品审核和库存规则继续独立生效。
- `GET /auth/consents` 使用 Bearer JWT；浏览器 BFF 为 `GET /api/auth/consents`，返回当前用户最新 100 条。未登录返回 `40100`；其他用户的 ID 不能改变读取范围。

```json
{"code":0,"message":"success","data":[{"document":"resource-usage-rules","version":"2026-09-06.1","action":"order","reference":"ORD20260906000000abcdef","accepted_at":"2026-09-06T08:00:00+08:00"}]}
```

记录只追加，不覆盖。`action` 为 `registration`、`publish`、`resubmit`、`order`；`reference` 分别是用户 ID、商品 ID、商品 ID、订单号。同意记录与业务在同一事务中提交；任何一步失败全部回滚。旧账户与历史交易不回填无法证明的同意。

## 迁移与版本发布

后端先应用 `019_legal_consents.up.sql`，再启用新的业务实现；前后端需协调发布同一版本。后端升级后旧浏览器可能收到 `40001`，应刷新后重新阅读同意。新增 Compose 挂载仅覆盖全新数据库，既有库需执行增量迁移。生产数据库迁移和两端部署均需单独授权；回滚不得直接删除已经积累的同意记录。

正文与版本必须作为一个整体审核。已发布版本不可原地改文；下次变更前归档本版正文，并为历史版本保留可阅读入口，再提升两端版本。当前只支持首版，没有新增 CMS 或管理后台。

## 正式启用前仍需确认

- 运营主体工商登记全称，以及未登录也能使用的客服与隐私联系邮箱或电话；不能使用猜测或占位联系信息。
- 正文与实际运营、短信服务商、材料保存期限、退款和投诉处理安排一致，经过运营方确认。
- 当前 KYC 的真实性确认勾选不等于敏感个人信息处理的单独同意；不得把注册同意记录当作其证明。微信、支付或其他第三方实际启用前，需要按实际接收方补充披露及相应授权流程。

页面与记录能力完成不等于协议已获法律审核、微信审核通过或支付可用。

参考依据：[微信网站应用运营规范](https://developers.weixin.qq.com/doc/oplatform/Website_App/operation.html)、[个人信息保护法](https://www.npc.gov.cn/npc/c2/c30834/202108/t20210820_313088.html)。
