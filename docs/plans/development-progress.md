# 开发进度与验证证据

当前：第 1 轮完成，进入第 2 轮。整体目标仍在执行；此结果不代表积分包和订阅已实现。每轮按实现、审查、修复、验证、提交顺序完成。开发无需生产域名、支付账号或上线审批。

## 第 1 轮：开发基线

### 收支与旁路清单

| 能力 | 当前入口与行为 | 接入轮次 |
| --- | --- | --- |
| 充值、兑换 | `model/topup.go` 的 `creditTopUpQuota`；各支付完成和 `model/redemption.go` 复用事务内总余额增加 | 4：订单/兑换事件发包 |
| 签到 | `model/checkin.go` 事务直接增余额，另有 IncreaseUserQuota 路径 | 4：签到事件发限时包 |
| 注册、邀请、转入 | `model/user.go` 两种 Insert、FinishInsert、邀请奖励、TransferAffQuotaToQuota；待领取邀请奖励与钱包不同 | 4：来源防重 |
| 管理员 | `model/user_quota_adjustment.go` 增减/覆盖；`controller/user.go` 通用更新也需收口 | 4：禁直接覆盖余额 |
| 文本、资金来源 | `relay/request_billing.go` → `service/billing_session.go`、`funding_source.go`；`model/quota_reserve.go` 依赖总余额 | 3：请求与来源持久化 |
| 信任及回退 | 大余额可免预扣；无限额 Key 仅解除 Key 上限；`service/billing.go` 无 session 还有 PostConsume 回退 | 3：新模式禁旁路与回退 |
| 图像、音频、实时 | `service/image_billing.go`、`quota.go`；实时有 PreWssConsume | 5：保留协议计量 |
| 异步、违规费 | `service/task_billing.go`、`midjourney.go`、`violation_fee.go`，含直接增余额及后扣 | 5：防重与恢复 |
| 订阅 | `model/subscription.go` 余额购买、单周期预扣、退款、七天清理、硬删除 | 7–8：窗口、版本及历史 |
| 延迟写入 | `model/utils.go` → updateUserQuotaUsedQuotaAndRequestCount 合并余额与统计 | 4、6：隔离新模式写入 |
| 统计及 Key | `model/usedata.go` quota、渠道 used_quota 为统计；Token 额度是独立限制 | 3、6：分别核账 |
| 路由 | `service/channel_select.go`、`model/channel.go` 多 Key 与亲和 | 10：稳定账号与重试 |

首批文本目标：Chat Completions、Responses、Claude Messages 的流式及非流式。图像、音频、实时、视频/异步任务等已有收费路径随后接入；新模式未验证的路径必须消费前拒绝。此表记录归属，不声称当前接通。

### 实际结果

代码起点 `5382135c6`，上游代码基线 `78bd5b1cb`。环境：Go 1.25.3、Bun 1.3.12；SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，均为实际运行版本。

| 命令 | 结果 |
| --- | --- |
| `make test` | 根模块与 relaykit 全量通过，52 个包；普通任务允许已有可选环境跳过，不声称零跳过 |
| `cd web && bun install --frozen-lockfile && bun run test` | 173 个文件、2161 个用例通过 |
| `cd web && bun run typecheck` | 通过 |
| `make test-db-up && make test-database` | 四个包的选定三数据库/Redis 契约全部通过，零跳过 |
| `go test ./controller -count=1` | 隔离修复后整个包通过 |
| `python3 -m unittest discover -s bin -p test_database_matrix_test.py` | 6 个失败门禁用例通过 |
| Compose 校验、CI YAML 解析、`git diff --check` | 通过；远程 Actions 尚未运行 |

审查修复：兑换码测试复用共享库且要求空库，与其他矩阵用例冲突；改用已有独立库机制。Runner 配置 audit/fixed-price DSN 别名，缺配置、跳过、漏掉必需分支均失败。本地健康检查确认日志库初始化完成。Compose 使用独立 tmpfs，不使用业务数据库卷。

复现：`make test-db-up` 后执行 `make test-database`；结束用 `make test-db-down`。配置和密码仅属于可丢弃开发测试服务。

## 后续边界

按[轮次表](subscription-billing.md#21-开发轮次估计)推进。对应领域入口接收明确服务器时间，并发测试使用同步屏障，随实现验证。D06、D07 已询问用户，尚待确认；第 2 轮先做不依赖它们的发放、有效期筛选与分配基础，结算分支保留待完成。其他待商榷规则到依赖阶段再确认，不作为基础开发启动门禁。
