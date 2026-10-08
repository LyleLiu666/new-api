# 开发进度与验证证据

当前：第 1–6 轮已完成并提交，第 7 轮继续开发。2026-10-08 负责人确认同套餐续费顺延、已有权益快照保留、管理员手动取消且系统不退款、使用时启动周期窗口、沿用 New API 计价和路由、套餐/加油包独立核算、禁止转赠及无历史用户迁移需求；D07 改为不欠款，超出合法可支付资源的费用由平台承担，充值不追扣。产品规则见设计 §7.7、第 9 节。已校正第 3 轮旧欠款/偿付路径，继续本轮购买与权益。已有套餐版本、订单与支付事实基础保留；第 7 轮仍未验收、不提交半轮、不跳到下一轮。开发无需生产域名、支付账号或上线审批。

已提交各轮下方保留当时实现与测试证据；其中欠款、偿付及历史用户迁移描述不再代表当前目标，现行规则以产品设计为准。

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


## 第 6 轮：恢复接管、日志待办与核账

范围见[恢复契约](subscription-billing.md#49-第-6-轮恢复与核账契约)。请求取得持久化执行租约和递增代次；发送、追加、任务关联与终结事务检查当前资格。长请求续租，异步提交主动交接；到期接管后的旧执行者不能迟到扣款。已有终结意图只重试原操作，未发送可释放，已发送未知用量只转核查。失败保存次数、错误和退避，超限仍保留事实，管理员凭唯一事件和原因重试，不能通过恢复接口修改金额或把未知费用变成退款。

结算同库原子更新用户/渠道统计并创建日志待办；独立 SQL 日志库保存唯一投递回执和日志，确认响应丢失后重放仍只有一条日志。共用主库的日志也走同一防重路径。按账户核对包数量守恒、流水分量、预占分配、请求与欠额，返回可定位差异，绝不自动改余额。管理接口验证当前角色、目标归属与 billing 读写权限，内部日志载荷和执行所有者不公开。

| 命令/场景 | 实际结果 |
| --- | --- |
| `make test-database` | SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，严格矩阵零跳过通过；最终日志 `/tmp/new-api-round06-reviewed-matrix.log` |
| `make test` | 根模块与独立 relaykit 全量通过；最终日志 `/tmp/new-api-round06-reviewed-full.log` |
| `go test -race ./model ./controller -run '^(TestCreditPackDatabaseMatrix\|TestCreditBillingDatabaseMatrix)$/^sqlite$' -count=1` | 核心并发、HTTP、任务和 WebSocket 回归通过，无 race；macOS LC_DYSYMTAB 链接警告，退出 0 |
| 独立子进程退出 | 在已保存结算意图、资金事务失败后真实 `os.Exit(23)`；新连接/执行者完成原 35 分结算，包与 Key 各扣一次；三库均通过 |
| 最新发布版升级 | GitHub 最新 release `v1.0.0-rc.41`（2026-09-30 发布）的实际源码调用 `InitDB/InitLogDB` 建库，写入用户、Key、消费日志；当前代码升级并再次启动，三库均保留 73 余额、11 已用、2 次请求、Key 70/3、日志 11 和用户名唯一约束；独立 MySQL/PostgreSQL 日志库及 SQLite 共库防重表存在 |
| `git diff --check`、OpenAPI JSON 解析 | 通过 |

发布版验证使用 `/tmp/new-api-round06-released-migration/released/cmd/credit-migration-seed/main.go` 与 `/tmp/new-api-round06-released-migration/verify.go` 构建两个独立程序；每库 seed/verify 日志保存在同目录。验证只涉及可丢弃开发数据库，无生产凭证。严格矩阵另验证新增表重复迁移不产生结构修改。含欠额、旧预扣、在途任务的完整迁移、批次中断/多实例切换和容量仍归第 12 轮，未预称通过。

审查修复的实际缺陷单独归档：Midjourney 即时完成重复统计；续租使用等待前时间；共库启动漏建日志回执表；核账守恒差异只显示部分合计。先复现目标断言失败，再修复；详见 bug 归档。业务规则依然只维护在设计文档。

边界：SQL 日志库已经验证一次投递。ClickHouse 的现有 MergeTree 无事务，当前保留可见待办而不伪称自动成功；等价防重与该引擎验证仍待后续整体验收。当前核账不自动修复，也不把旧统计/Key 历史基线当作新账本生成；全量历史核账与容量在第 12 轮。字段级用量及完整消费日志明细在第 9 轮补齐。

下一轮：套餐版本、购买合同和连续 30 天权益。设计 §7.7 提出的锁价、标签、续费、窗口和支付顺序仍待回答；先推进不依赖答案的基础能力，不把建议默认写成批准。


## 需求校正轮：确认规则与不欠款结算

2026-10-08 按负责人最新答复更新产品设计、技术设计、开发计划和验收场景。此前同意的欠款方案被明确替换；此项记录为需求修订，不写成原设计的 bug。第 7 轮的购买与权益仍独立验收，此处不提交其未完成代码。

结算保留参考费用 `Actual`、真正扣款 `Charged`、平台承担的未收取量 `Uncollected`，移除欠款生成、欠款准入和充值自动偿付。追加扣款同时受有效包和有限额 Key 剩余额度约束；用户统计、任务扣款和用户消费日志使用真正扣款，参考费用另列。余额不足的新付费请求（包括零预估）拒绝，明确免费请求仍可记录；充值完整到账并可重新消费，不追扣旧差额。核账同时检查扣款分配和 `Actual = Charged + Uncollected`。

先写失败用例：余额30、预占20、最终50；Key剩25、包余额100、预占20、最终50。旧实现出现充值偿债、Key负数和日志显示未收取费用；修正后这些行为测试通过，结算重放不重复扣款。

| 验证 | 结果与证据 |
| --- | --- |
| 针对性 SQLite 行为测试 | `/tmp/new-api-no-debt-red.log` 先出现目标行为失败；`/tmp/new-api-no-debt-green.log` 修正后通过 |
| 真实数据库/Redis 矩阵 | `make test-database` 通过，零跳过；SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11；`/tmp/new-api-no-debt-matrix.log` |
| 全量后端回归 | `make test` 根模块与独立 relaykit 通过；`/tmp/new-api-no-debt-full.log` |
| 积分包/恢复 SQLite race | `go test -race ./model -run '^TestCreditPackDatabaseMatrix$/^sqlite$' -count=1` 通过；`/tmp/new-api-no-debt-race.log` |
| 静态检查 | `go vet ./model ./service ./controller` 通过；`/tmp/new-api-no-debt-vet.log` |
| 上游发布版结构升级 | 实际 `v1.0.0-rc.41` 创建三种新开发库，当前代码升级并启动两次通过，保留原用户、Key、订单、订阅、独立日志和唯一性；`/tmp/new-api-round06-released-migration/r07-no-debt-{sqlite,mysql,postgres}-{seed,verify}.log` |

结构升级验证服务于项目数据库规范；不恢复已取消的历史用户商业迁移产品范围。在途追加失败停止生成及完整费用证据仍按第 9 轮交付，不把本轮结算修正说成整个计费目标完成。

## 第 7 轮进行中：套餐版本、订单与支付事实基础

**本轮尚未完成、尚未提交。** 已实现不可变版本保存、草稿摘要、六位小数售价整数、连续 30 天记录及管理员分页接口，以及主库锁价订单和支付事实基础；发布及记录支付不改变旧订阅。设计 §7.7 已确认标签、续费与终止行为；接下来接通实际购买入口、渠道支付核验、余额购买及权益查询，才能验收第 7 轮。按原轮次顺序推进，不把基础模块通过视为整轮完成。

已经看到并修复的目标失败：版本入口缺失；不同套餐并发争用一个发布事件返回 MySQL 1062 而非版本冲突；负分页和偏移溢出返回 200；省略 expected_revision 不能与显式 0 区分；到期订单重放被时间校验拒绝；四种支付回调的签名、正文、查询串泄露及 Creem 客户信息泄露。当前保存记录不沿用旧字段去暗中批准新的溢出付款或标签策略。

| 验证 | 实际结果 |
| --- | --- |
| `make test-database` | SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11 严格矩阵零跳过通过；新增 `TestSubscriptionVersionDatabaseMatrix` 三分支已纳入必需结果，含订单/交易归属的真实并发；`/tmp/new-api-round07-purchase-current-lock-matrix.log` |
| `make test` | 根模块与独立 relaykit 全量通过，含四种支付审计回归；`/tmp/new-api-round07-purchase-final-full.log` |
| `go test -race ./model ./controller -run '^(TestSubscriptionVersionDatabaseMatrix\|TestCreditBillingDatabaseMatrix)$/^sqlite$' -count=1` | 版本、订单及真实管理接口/消费回归通过，无 race；macOS 链接器警告，退出 0；`/tmp/new-api-round07-purchase-reviewed-race.log`。支付审计另以完整顶层测试运行，全部子场景通过；`/tmp/new-api-round07-payment-audit-race.log` |
| 最新 release 直接升级 | 用 `v1.0.0-rc.41` 实际源码在三个全新开发库建表及写入旧数据；当前代码升级并重复启动通过，旧余额/Key/日志/用户名唯一约束及旧套餐订单、旧订阅用量/起止时间保留，四张新增表存在；日志 `/tmp/new-api-round06-released-migration/r07-purchase-{sqlite,mysql,postgres}-{seed,verify}.log` |
| `go vet` | 主库、控制器、路由、中间件等后端包通过；未包含需要生成前端嵌入产物的根启动包；`/tmp/new-api-round07-purchase-vet.log` |
| Python 严格矩阵门禁、OpenAPI 解析、`git diff --check` | 6 个门禁用例通过；文档及差异检查通过 |

本轮基础测试覆盖原版本不随草稿编辑变化、迟到发布重放、旧草稿冲突、两个独立连接争用同一版本、两个不同套餐争用同一全局事件、失败回滚后仍使用下一版本号、旧订阅不变、当前管理员角色、令牌读写权限、伪造操作者/路径 ID、历史分页响应、负值与省略参数。订单另验证改价/下架后原合同重放、新下单拒绝旧发布版本、期限结束后仍返回原记录、金额/币种/付款人/实际付款时刻缺失或不符进入核查、同交易多个事件和多订单冲突，以及事实插入后交易归属保存失败全部回滚。MySQL/PostgreSQL 在交易唯一约束前设置同步点，确实有两个独立事务竞争；SQLite 按实际单写者机制验证。测试夹具显式设置当前数据库类型，保证共同锁辅助函数在 SQL 数据库发出 FOR UPDATE，避免只测到 SQLite 分支。

支付日志修复与实际付费策略分开归档；未知事实不取“现在”或订单金额来补齐。版本化购买底座目前没有调用支付网关或开通权益，也没有新增客户端伪造付款的接口；仍需接通四种原订阅支付、余额购买、标签、终止和来源返还。渠道事实缺口及适配范围见计划 §4.10，业务答复始终落在设计 §7.7，不要求生产凭证作为开发前提。账户/窗口消费和商品退款还没有本轮通过证据，不在此宣称交付。

后续审查补齐 B25：同通知不同内容拒绝时仍保存关联原回执的矛盾观察与当前订单核查标记，原事实不变；同一矛盾重放只保存一份。除已存在事件分支外，还用 SQL 同步点验证不同订单并发争用同一事件，只有原订单取得交易归属。此修复属于支付证据保存，不决定续费、标签及退费政策。

本次补充实际验证：`make test-database` 严格三库/Redis 零跳过通过（`/tmp/new-api-round07-payment-conflict-matrix.log`）；`make test` 根模块和独立 relaykit 通过（`/tmp/new-api-round07-payment-conflict-full.log`）；版本/订单 SQLite race 通过（`/tmp/new-api-round07-payment-conflict-race.log`）；`go vet ./model ./controller` 通过（`/tmp/new-api-round07-payment-conflict-vet.log`）。再用实际 `v1.0.0-rc.41` 在三个全新开发库创建旧余额、Key、日志、套餐订单和订阅，当前代码升级并启动两次通过，原数据和唯一约束保留；日志 `/tmp/new-api-round06-released-migration/r07-payment-conflict-{sqlite,mysql,postgres}-{seed,verify}.log`。第 7 轮仍未验收、未提交。
