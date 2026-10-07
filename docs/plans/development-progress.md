# 开发进度与验证证据

当前：第 1–5 轮完成，进入第 6 轮。整体目标仍在执行；积分来源发放、现有多协议消费已接通，持久化恢复、证据完善及订阅继续开发。每轮按实现、审查、修复、验证、提交顺序完成。开发无需生产域名、支付账号或上线审批。

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

## 第 4 轮：来源发放、策略快照与人工核查

范围见[发放与核查契约](subscription-billing.md#47-第-4-轮发放与退款核查契约)。接入五种现有支付完成、人工补单、兑换、签到、注册/邀请和管理员发包；主库保存来源策略、订单快照、操作者原因、冻结案件及追加式现金证据。已确认 D06/D07 保持在设计 §7.5，所提 D03/D09 默认规则与 D12 未确认分支在设计 §7.6，未作为默认配置。

新模式不提供旧汇总余额覆盖或按未知历史续期的邀请转入；旧资金迁移在第 12 轮。购买撤销的现金与权益终局按 D12 待定规则处理，当前交付的是人工核查/冻结/证据接口，不声称自动退款已完成。旧账户发放保留既有行为，新模式仍未对生产账户自动启用。

| 命令 | 实际结果 |
| --- | --- |
| `make test-database` | SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，专用矩阵零跳过通过；五种支付重复/同时回调、写入故障回滚、锁定数量及期限、兑换/签到/注册原子发放、退款冻结及现金证据重放、管理 API 与权限拒绝均通过 |
| `make test` | 根模块与独立 relaykit 全量通过，普通测试的外部可选环境跳过不当作三库证据 |
| `go test -race ./model -run '^TestCreditPackDatabaseMatrix$/^sqlite$' -count=1` | SQLite 来源与账务并发、race 检查通过；链接器给出 macOS LC_DYSYMTAB 警告，测试退出 0，无 race 报告 |
| `python3 -m unittest discover -s bin -p test_database_matrix_test.py` | 6 个严格矩阵门禁用例通过 |
| `git diff --check`、OpenAPI JSON 解析 | 通过 |

迁移验证：本轮新增表重复迁移不改结构；含代表旧结构的 User、充值订单、兑换码列升级，保留旧余额、订单/代码数值与状态，不自动补造期限或启用模式。测试使用与正式启动一致的 MySQL/PostgreSQL 迁移方言包装；完整最新发布版本升级、中断续跑及模式切换仍在第 12 轮，当前不预称通过。

review 修复：旧用户编辑覆盖模式、新管理路由漏登记令牌权限、兑换管理接口丢失期限/用途、已兑换代码可被重启、SQLite 回调读后写竞争、签到范围未校验、策略接口依赖旧缓存角色，以及核查证据被覆盖/迟到重放冲突。缺陷详见 bug 归档；修复均保留原目标断言。

下一轮：图片、音频、实时和异步任务的现有收费入口，追加预占与分步失败。沿已有协议计量接入统一持久化请求，不以生产上游或支付凭证作为开发条件。


## 第 5 轮：多协议消费与追加预占

范围见[多协议契约](subscription-billing.md#48-第-5-轮追加预占与多协议契约)。图片参数覆盖后的追加预占、音频、embedding、实时 WebSocket、同连接多次 Responses 请求、任务插件和旧 Midjourney 已接入持久化请求。HTTP 各适配器在本地转换之后、发送之前标记提交，含绕过公共 HTTP 工具的 SDK 路径。未知失败保留预占待核查，不回落到旧钱包；已知任务成功可以结算真实零费用。

覆盖包括：追加重放及并发、追加时原包已到期、Key/包不足整笔回滚、分次预占按原来源结算；图片分裂载荷只计一张；音频实际格式；实时重复回执及分开的文本/音频表达式；任务金额回写故障后的意图保留与原子回滚；异步完成重复结算；Midjourney 旧余额为零仍可用有效包付款；大小写不同任务 ID 在三数据库均独立结算。

| 命令 | 实际结果 |
| --- | --- |
| `make test-database` | SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11；专用矩阵零跳过通过 |
| `make test` | 根模块与独立 relaykit 全量回归通过 |
| `go test -race ./model ./controller -run '^(TestCreditPackDatabaseMatrix\|TestCreditBillingDatabaseMatrix)$/^sqlite$' -count=1` | 模型并发、真实 HTTP/任务/两种 WebSocket 链路通过，无 race；macOS 链接器 LC_DYSYMTAB 警告，退出 0 |
| `git diff --check` | 通过 |

TDD 先观察到追加接口缺失、图片/音频/实时/任务/Midjourney 未接入的目标失败，再实现。提交前新增大小写编号回归确实在 MySQL 失败，修复后严格三库矩阵通过。完整回归还发现旧 Kling 路径多了一次预占调用，已将新增预占限定到新账务模式，保留旧断言。

商业问题仍维护于设计 §8.1：D05/D10 尚未回答，当前未知费用只是保留资金和核查状态，未替用户决定中断、失败或违规费责任。违规附加费不能通过旧后扣路径绕过新账本，该分支需在第 9 轮按确认政策提供独立证据与费用项。第 6 轮补齐异步消费的统计、独立日志待办和崩溃恢复；第 9 轮补齐字段级证据、真实零值、估算和调整。这些后续能力不预称本轮已经交付。

下一轮：用持久化租约和递增代次控制恢复接管，阻止旧执行者迟到写入；同库统计和跨库日志各有防重结果，账务核对返回可定位差异。
