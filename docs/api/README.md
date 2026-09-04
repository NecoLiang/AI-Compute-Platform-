# 前端联调接口文档索引

**Base URL**: 开发 `http://localhost:8080/api/v1` ｜ 生产以运维提供的域名为准

## 通用约定

- **响应包裹**：所有接口统一 `{"code":0,"message":"success","data":{...},"request_id":"req_..."}`；`code!=0` 为业务错误，`message` 可直接展示
- **鉴权**：需登录的接口带 `Authorization: Bearer <access_token>`（短信验证码登录获取，见 auth-api）
- **通用错误码**：40001 参数 / 40100 未登录 / 40101 token 过期 / 40300 无权限 / 40400 不存在 / 42900 限流 / 50000 服务异常
- **金额单位一律为分**；时间为 ISO8601
- 联调遇到问题时带上响应里的 `request_id`，后端可直接定位日志

## 文档导航

| 文档 | 内容 | 角色 |
|---|---|---|
| [auth-api.md](auth-api.md) | 短信验证码注册/登录、token 刷新 | 全部 |
| [compute-api.md](compute-api.md) | 算力市场：商品列表/详情、下单、订单、交付签收、访问凭证 | 买家/供应方 |
| [agent-search-api.md](agent-search-api.md) | 市场页智能选型：算力推定 + 商品匹配 | 买家 |
| [gpu-catalog-api.md](gpu-catalog-api.md) | GPU 型号库下拉（含安可认证标记）+ 管理端维护 | 发布页/运营 |
| [scheduler-api.md](scheduler-api.md) | 供应方节点注册/心跳、商品健康度、调度建议 | 供应方/运营 |
| [blockchain-api.md](blockchain-api.md) | 文昌链存证查验（订单/交付/违规 上链验证） | 全部 |
| [payment-api.md](payment-api.md) | 支付与结算 | 买家/供应方 |
| [notification-api.md](notification-api.md) | 站内信/消息中心 | 全部 |
| [invoice-api.md](invoice-api.md) | 发票管理 | 买家 |
| [ticket-api.md](ticket-api.md) | 工单 | 买家/运营 |
| [intermediary-api.md](intermediary-api.md) | 居间/设备/质押板块 | 相关角色 |
| [admin-api.md](admin-api.md) | 运营管理后台 | 运营 |

> 文档随代码同仓维护：接口有变更时文档在同一个 commit 里更新，以 `main` 分支为准。
