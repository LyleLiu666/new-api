# 开发进度与验证证据

当前：第 1–3 轮完成，进入第 4 轮。整体目标仍在执行；积分消费核心已接通，订阅和完整发放入口仍在开发。每轮按实现、审查、修复、验证、提交顺序完成。开发无需生产域名、支付账号或上线审批。

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

按[轮次表](subscription-billing.md#21-开发轮次估计)推进。对应领域入口接收明确服务器时间，并发测试使用同步屏障，随实现验证。D06 积分跨到期结算、D07 不足处置已在 2026-10-07 获用户确认并写回产品设计 §7.5；窗口和途中追加的其余细节仍待确认。其他待商榷规则到依赖阶段再确认，不作为基础开发启动门禁。


## 第 2 轮：积分包与分配核心

范围：新增账户串行写入点、积分包、操作防重结果、来源分配及不可变数量流水；只新增主库表，不转换旧余额，不启用用户新账务模式。

契约见[核心接口](subscription-billing.md#45-已实现的积分包核心契约)。发放和预占在短事务内共同提交流水、来源及结果；支付入口后续可调用 Tx 版本把来源事件一起提交。

已验证 S02、S03、S06 与 S01 的预占部分，以及 I02–I05、I11 的积分基础部分。每种数据库包括新增表、已有 User/余额的增量升级、重复迁移无结构变更、完整来源重放、冲突拒绝、用途/时间过滤、同到期稳定顺序、回滚、独立连接并发与数量流水核对。最近发布版本的完整迁移和真实请求结算仍由第 3、12 轮验证，不在此预称通过。

审查修复：已到期但未清理的大包原来占用钱包上限，造成新发放被拒。新增失败用例后改为只统计未到期未分配量与全部预占量；过期历史不被删，预占仍占额度。使用摘要索引保持来源 ID 大小写和精确字符串语义，避免 MySQL 默认排序规则误合并订单。

| 命令 | 实际结果 |
| --- | --- |
| `make test-database` | SQLite 3.50.4 / MySQL 8.4.11 / PostgreSQL 15.19；新增核心六组行为场景全部通过；整个专用矩阵零跳过 |
| `go test ./model -count=1` | 模型包完整回归通过 |
| `go test -race ./model -run '^TestCreditPackDatabaseMatrix$' -count=1` | SQLite 核心并发与 race 检查通过；此命令未配置外部 DSN，因此不作为三库证据 |
| Python 门禁 6 用例、`git diff --check` | 通过 |

第 3 轮补齐持久化请求、互斥结算/释放、Key 原子变更和真实文本接入。D06、D07 的积分规则现已确认，具体见产品设计；不会用已提交的预占模块声称完整账务生命周期已完成。

## 第 3 轮：持久化请求与文本消费

范围及限制见[请求结算契约](subscription-billing.md#46-已实现的请求结算契约)。新增请求、欠额和持久化偿付事件；预占、分包结算与 Key 额度在主库原子处理。关闭新模式的大余额信任旁路及旧总余额/Key 写入路径。用户确认的积分跨到期结算和欠额规则已写入[产品设计 §7.5](../design/README.md#75-已确认的跨到期结算与未付差额)；缺陷单独记录在 bug 归档。

已验证：真实认证、渠道选择、HTTP 模拟上游、现有价格表达式、FEFO、请求结算及 Key 用量的完整 Chat Completions 链路；无限额 Key 不能用已过期包或大旧余额付款；未接入路径和“只用订阅”在发送前拒绝。核心覆盖原预占跨期结算/失效释放、部分补扣和欠额、充值偿付、防重冲突、独立连接并发结算/释放、Key 写入失败的资金回滚及意图保留、丢失内存会话后的重放、旧 User 表升级与旧余额保留。

| 命令 | 实际结果 |
| --- | --- |
| `make test-database` | SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19 和 Redis 7.4.11；含新增模型及 HTTP 消费契约，整个专用矩阵零跳过通过 |
| `make test` | 根 Go 模块及独立 relaykit 全量通过；普通任务的可选外部环境跳过不作为三库证据 |
| `go test -race ./model -run '^TestCreditPackDatabaseMatrix/sqlite$' -count=1` | SQLite 核心并发及 race 检查通过 |
| Python 矩阵门禁 6 用例、`git diff --check` | 通过 |

审查发现并修复：订阅偏好被静默覆盖；偿付内部入口重放未持久防重；新 HTTP 测试泄漏全局分组配置。先看到相关失败，再修复并通过上述回归。数据库迁移只新增表和默认 0 的模式列；完整历史迁移、第 6 轮恢复器、第 9 轮证据和全部协议尚未完成，不作为切换生产账务的依据。

第 4 轮接通充值、兑换、签到、注册/邀请、管理员发放及撤销记录；发包有效期和用途按商品/活动配置，不把生产支付账号设为启动条件。
