# OmniS 问题追踪 — 2026-09-08

本表将 2026-09-08 的源码、接口与运行状态核对结果转为 GitHub Issue。历史文档仅作背景；Issue 的验收标准、关联 PR 和后续验证记录是进度依据。前后端保持独立仓库。

## 修复与合并规则

1. 修复前创建或复用目标 Issue，写明问题、证据和验收标准。
2. 在独立分支修复并留下对应回归。PR 使用 `Closes #编号` 关闭本仓库已完成目标；跨端依赖和部分完成项用 `Refs`。
3. 检查通过且验收条件满足后 merge；提交、合并、部署、生产迁移和真实渠道验收分别记录。不得将后续欠项顺带关闭。
4. 本批后端需迁移 020，合并 main 会自动部署；生产迁移须先单独授权并核对备份和库存，不把合并授权扩大到数据库。

## 后端

| Issue | 本轮安排 |
|---|---|
| [[P0] 交易开关必须在创建订单时实际生效](https://github.com/NecoLiang/AI-Compute-Platform-/issues/2) | 本批修复，合并后按验收关闭 |
| [[P0] 新订单使用动态费率并保持历史费用快照](https://github.com/NecoLiang/AI-Compute-Platform-/issues/3) | 本批修复，合并后按验收关闭 |
| [[P0] 风控告警冻结必须改变关联订单或账户](https://github.com/NecoLiang/AI-Compute-Platform-/issues/4) | 本批修复，合并后按验收关闭 |
| [[P0] 按真实库存占用释放，暂停不完整续租](https://github.com/NecoLiang/AI-Compute-Platform-/issues/5) | 本批修复，合并后按验收关闭 |
| [[P0] 消除个人资料更新假成功](https://github.com/NecoLiang/AI-Compute-Platform-/issues/6) | 本批修复，合并后按验收关闭 |
| [[P1] 公共商品响应保留 health 契约](https://github.com/NecoLiang/AI-Compute-Platform-/issues/7) | 本批修复，合并后按验收关闭 |
| [[P1] 补齐超时签收与机房侧凭证失效闭环](https://github.com/NecoLiang/AI-Compute-Platform-/issues/8) | 后续待办，保持开放 |
| [[P1] 实现关联原订单的完整续租生命周期](https://github.com/NecoLiang/AI-Compute-Platform-/issues/9) | 后续待办，保持开放 |
| [[P2] 实现可验证的账户资料编辑](https://github.com/NecoLiang/AI-Compute-Platform-/issues/10) | 后续待办，保持开放 |
| [[P1] 将试点自动通过 KYC 替换为真实核验](https://github.com/NecoLiang/AI-Compute-Platform-/issues/11) | 后续待办，保持开放 |
| [[P1] 完成易宝支付、分账、退款与渠道对账闭环](https://github.com/NecoLiang/AI-Compute-Platform-/issues/12) | 后续待办，保持开放 |
| [[P1] 实现资质到期前 30 天预警](https://github.com/NecoLiang/AI-Compute-Platform-/issues/13) | 后续待办，保持开放 |
| [[P1] 接通自动风控、刷单检测与信用更新](https://github.com/NecoLiang/AI-Compute-Platform-/issues/14) | 后续待办，保持开放 |
| [[P2] 实现订单电子合同与多方签署](https://github.com/NecoLiang/AI-Compute-Platform-/issues/15) | 后续待办，保持开放 |
| [[P2] 补通知模板、短信和邮件业务通知](https://github.com/NecoLiang/AI-Compute-Platform-/issues/16) | 后续待办，保持开放 |
| [[P2] 补历史趋势、运营统计与报表导出](https://github.com/NecoLiang/AI-Compute-Platform-/issues/17) | 后续待办，保持开放 |
| [[P1] 完成微信生产开通与绑定验收](https://github.com/NecoLiang/AI-Compute-Platform-/issues/18) | 后续待办，保持开放 |
| [[P2] 明确资方业务与 Token 工厂集成边界并交付后端能力](https://github.com/NecoLiang/AI-Compute-Platform-/issues/19) | 后续待办，保持开放 |
| [[P1] 迁移 020、存量库存核对与生产业务验收](https://github.com/NecoLiang/AI-Compute-Platform-/issues/20) | 后续待办，保持开放 |
| [[P1] 校正进度文档并建立 Issue → PR → merge 交付记录](https://github.com/NecoLiang/AI-Compute-Platform-/issues/21) | 本批修复，合并后按验收关闭 |

## 前端

| Issue | 本轮安排 |
|---|---|
| [[P0] 结算读取交易开关和动态费率](https://github.com/Dylan-Nihilo/compute-exchange/issues/16) | 本批修复，合并后按验收关闭 |
| [[P0] 风控冻结确认、真实结果与失败重试](https://github.com/Dylan-Nihilo/compute-exchange/issues/17) | 本批修复，合并后按验收关闭 |
| [[P1] 贯通市场、详情和结算健康度](https://github.com/Dylan-Nihilo/compute-exchange/issues/18) | 本批修复，合并后按验收关闭 |
| [[P1] 校正前端接入清单并按目标 Issue 合并修复](https://github.com/Dylan-Nihilo/compute-exchange/issues/19) | 本批修复，合并后按验收关闭 |
| [[P1] 完成支付状态反馈、退款与渠道对账交互](https://github.com/Dylan-Nihilo/compute-exchange/issues/20) | 后续待办，保持开放 |
| [[P1] 在后端续期闭环完成后开放买家续租](https://github.com/Dylan-Nihilo/compute-exchange/issues/21) | 后续待办，保持开放 |
| [[P2] 对接真实账户资料编辑和敏感变更验证](https://github.com/Dylan-Nihilo/compute-exchange/issues/22) | 后续待办，保持开放 |
| [[P1] 补协议运营信息并验收真实 KYC 与同意留痕](https://github.com/Dylan-Nihilo/compute-exchange/issues/23) | 后续待办，保持开放 |
| [[P2] 接入智能选型页面](https://github.com/Dylan-Nihilo/compute-exchange/issues/24) | 后续待办，保持开放 |
| [[P2] 接入供应方节点管理及双方调度建议](https://github.com/Dylan-Nihilo/compute-exchange/issues/25) | 后续待办，保持开放 |
| [[P2] 实现存证查验、订单时间线与运营补推入口](https://github.com/Dylan-Nihilo/compute-exchange/issues/26) | 后续待办，保持开放 |
| [[P2] 提供用户可查看的授权同意记录页面](https://github.com/Dylan-Nihilo/compute-exchange/issues/27) | 后续待办，保持开放 |
| [[P2] 接入运营 GPU 型号维护](https://github.com/Dylan-Nihilo/compute-exchange/issues/28) | 后续待办，保持开放 |
| [[P1] 补运营工单详情回复和订单处置工作流](https://github.com/Dylan-Nihilo/compute-exchange/issues/29) | 后续待办，保持开放 |
| [[P2] 完成 CRM 成交流程与厂商工作台接入](https://github.com/Dylan-Nihilo/compute-exchange/issues/30) | 后续待办，保持开放 |
| [[P2] 交付资方工作台与 Token 工厂实际入口](https://github.com/Dylan-Nihilo/compute-exchange/issues/31) | 后续待办，保持开放 |
| [[P2] 补历史趋势、报表导出和公告用户触达](https://github.com/Dylan-Nihilo/compute-exchange/issues/32) | 后续待办，保持开放 |

当前源码已有的账单、发票、供应方聚合、消息、节点 API 和存证基础能力不重复建成“完全未实现”任务；缺失页面、外部渠道开通和生产验收分别跟踪。
