# 开发进度与验证证据

当前：第 1–11 轮实现、review 与必要验证完成，第 12 轮整体验收待开展。2026-10-08 负责人已确认同套餐续费顺延、购买快照保留、管理员取消且系统不退款、使用时启动周期窗口、沿用 New API 计价和路由、套餐与加油包独立核算、禁止转赠、无历史用户商业迁移。最终费用超过合法可扣资源的部分由平台承担，不产生用户欠款，充值不追扣；已实现并记录于产品设计 §7.7、第 9 节。每轮完成 review、修复、必要验证后提交再进入下一轮。开发不依赖生产凭证或上线审批。

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

按[轮次表](subscription-billing.md#21-开发轮次估计)推进。对应领域入口接收明确服务器时间，并发测试使用同步屏障，随实现验证。D06 积分跨到期结算、D07 不足处置已在 2026-10-07 获用户确认并写回产品设计 §7.5；2026-10-08 的后续答复已确定首次使用启动周期窗口、套餐和加油包独立核算、平台承担超出合法额度的费用；以设计 §7.7、第 9 节为准。剩余实现按原轮次推进，不把上线条件当开发门禁。


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

## 第 7 轮：套餐版本、购买权益与渠道支付

完成不可变套餐发布版本、锁价订单、付款事实与交易归属防重、余额购套餐、四种原现金订阅渠道、连续 30 天期限及提前续费顺延、购买标签快照、管理员取消、用户订单与管理员人工付款核查 API。取消只停止权益，不退款、不发返还包。产品规则维护于设计，缺陷及红绿证据归档于 B21–B36。

余额扣包、付款事实和开通同事务；现金入口先保存本地合同再请求渠道。Stripe 核实发票付款秒数；Epay、Creem、Pancake 缺少可靠付款时间时保留 NULL、核查，管理员凭外部证据追加确认，原回执不改。Pancake 实收与标价、缺失与真实 0 分开保存。重复或未知现金创建不能发起第二笔收款；不存买家 JWT，认证刷新不创建现金订单。后到通知不能解除管理员取消边界。

管理操作检查当前数据库角色与目标归属、读写令牌范围、唯一事件和最新事实；用户不能提交成功断言、金额或其他买家身份，跨用户详情返回 404。适用安全参考为 OWASP Authentication、Session Management、Logging 及 ASVS 5.0.0 V8 对象授权；验证范围为本轮受改路径，不宣称全项目合规。测试使用真实签名及 SDK 本地网关，未进行生产商户联调。

| 最终快照验证 | 实际结果与证据 |
| --- | --- |
| `make test-database` | SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11；严格矩阵零跳过通过，含真实独立 SQL 事务争用发布、交易、结账发送资格；`/tmp/new-api-round07-final-matrix.log` |
| `make test` | 根 Go 模块与独立 relaykit 全量通过；`/tmp/new-api-round07-final-full.log` |
| `go test -race ./model ./controller -run '^(TestSubscriptionVersionDatabaseMatrix\|TestCreditBillingDatabaseMatrix\|TestPaymentWebhookAudit)' -count=1` | SQLite 版本、付款、管理/真实消费链路与支付审计通过，无 race；此命令未配置外部 DSN，其可选分支不作为三库证据；`/tmp/new-api-round07-final-race.log` |
| `go vet ./model ./controller ./service ./router ./middleware` | 通过；`/tmp/new-api-round07-final-vet.log` |
| 最新发布版升级与重复启动 | 实际 `v1.0.0-rc.41` 源码 seed 三种全新开发库，当前代码升级并启动两次通过；旧余额73/已用11/请求2、Key70/3、订单10.5、订阅100/7及原起止、日志11、唯一索引均保留；新标签、现金响应和取消边界第二次启动保留。`go build -o /tmp/new-api-round07-final-migration/verify /tmp/new-api-round07-final-migration/verify.go` 后运行 `python3 /tmp/new-api-round07-final-migration/run.py`；汇总及六份日志在该目录 |
| 三库新建库与两次启动 | `go build -o /tmp/new-api-round07-final-migration/fresh /tmp/new-api-round07-final-migration/fresh.go` 后运行 `python3 /tmp/new-api-round07-final-migration/fresh-run.py` 全部通过；分别独立 MySQL/PostgreSQL 日志库及 SQLite 共库，新增表、取消列、日志防重和新模式保存；`fresh-result.log` |
| Python 门禁、OpenAPI JSON、`git diff --check` | 6 个严格门禁测试通过，JSON 和差异检查通过 |

升级验证履行结构兼容要求，不恢复已取消的历史用户商业迁移范围。第 8 轮负责 5 小时/周/期限累计窗口、提交上游时确认启动、多 Key 共享、旧代次结算、未来续费期生效和支付来源编排；当前购买与查询不能替代窗口消费验收。第 9–12 轮继续用量证据、路由、页面及整体故障/容量验收，整个 goal 仍在执行。


## 第 8 轮：多个额度窗口与来源编排

已完成期限累计与最多 8 个短周期窗口、首次提交确认计时、多 Key 共享、过期代次保留、窗口/Key 原子追加与结算、提前续费期限切换、四种来源偏好及用户窗口查询。规则和响应契约见[开发计划 §4.15](subscription-billing.md#415-第-8-轮窗口消费契约)及 OpenAPI。窗口和积分包整笔选来源，不能重复扣款或借用新代次；最终无法覆盖的量记录为平台未收取，不记用户欠款。未启动的窗口查询不创建代次。

TDD 覆盖：订阅不依赖钱包即可真实请求；5 小时不足整笔回滚；两个 SQL 事务并发首次使用；另一请求成功后失败请求不得重置窗口；窗口到期后的迟到结算；Key 不足追加回滚；终结事务失败保留意图与所有预占、重试只扣一次；执行接管拒绝旧代次提交；免费不启动、付费零估算不绕过耗尽 Key；未生效续费不增当前额度，新期按自己的快照生效；取消后原预占可结算、追加被拒。套餐和钱包独立核账，套餐累计只计算一次；核账能定位人为制造的累计量差异。

审查修复见 B37–B40。查询/消费按当前数据库权益分组，旧 Redis 分组不能延长已过期权限；仍保留已有身份版本 fence。适用参考：[OWASP Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)、[Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html) 与既有 ASVS 5.0.0 V8 授权边界。测试包含当前用户禁用、Key 禁用/到期、缓存过期分组和待提交限制性身份更新；只说明受改路径验证，不宣称全项目合规。

| 验证 | 实际结果与证据 |
| --- | --- |
| `make test-database` | SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11；严格矩阵零跳过通过，新增必需 window_consumption 分支不得遗漏；`/tmp/new-api-round08-reviewed-matrix.log` |
| `make test` | 根 Go 模块及独立 relaykit 全量通过；`/tmp/new-api-round08-final-full.log` |
| `go test -race ./model ./controller ./middleware -run '^(TestSubscriptionVersionDatabaseMatrix\|TestCreditBillingDatabaseMatrix\|TestVersionedRelayGroupUsesCurrentRightsAndAuthFence\|TestAccessTokenIdentityAndSingleLookup)' -count=1` | SQLite 模型/真实 HTTP/身份缓存并发回归通过，无 race；可选外部 DSN 跳过不作三库证据；`/tmp/new-api-round08-final-race.log` |
| `go vet ./model ./controller ./service ./router ./middleware` | 通过；`/tmp/new-api-round08-final-vet.log` |
| 发布版升级及两次启动 | 再确认最新 release 为 v1.0.0-rc.41；用该实际源码 seed、当前代码 InitDB/InitLogDB 升级并重复启动，三库均通过；原用户/Key/订单/订阅/日志及唯一约束保留；新增 window_rules、窗口计数和分配保留、同规则同代次唯一约束有效。构建 `/tmp/new-api-round08-final-migration/verify.go` 后运行同目录 `run.py`，六份日志与 result.log 保存于该目录 |
| 新库及两次启动 | 构建同目录 fresh.go 后运行 fresh-run.py，真实三库均通过；窗口表、来源/分配列、新账户模式及独立 SQL 日志防重结构存在；fresh-result.log |
| Python、JSON、差异 | `python3 -m unittest discover -s bin -p test_database_matrix_test.py` 6 个门禁用例通过；OpenAPI JSON 解析及 git diff --check 通过 |

数据库矩阵最初发现测试 Key 与已有小写 Key 在 MySQL 默认排序规则下重复；新测试改用符合既有认证格式的独立字母数字 Key，完整三库重新通过。旧 fixture 补齐依赖的订阅表并在清理时删除窗口表，不放宽业务断言。

第 9 轮继续字段级用量证据、估算/价格版本、断流与尝试费用、原分配修正及平台未收取费用；当前窗口核心不能替代这些验收，整个 goal 仍执行。

## 第 9 轮：用量证据与账单修正（开发中，未提交）

当前已实现：

- 追加式字段证据、事件/序号防重和尝试隔离；明确零与 unknown 分开，来源区分 upstream、estimate、adaptor。原事件不可变，断流估算以部分计数为下限，完整终局不被估算覆盖。
- 最终明细在金融意图之前持久化，金额与证据绑定；证据提交后、意图前退出仍可接管恢复完整消费日志。未知费用保留占用并核查。
- 原字段采集覆盖 OpenAI Chat JSON/SSE、Realtime、Claude JSON/SSE 及转 Responses、Responses JSON/SSE/WebSocket/压缩及转换、Gemini 原生/Chat/Responses JSON/SSE、OpenAI 转写 JSON/SSE、Images JSON/JSON 转 SSE/真实 SSE。转写秒数保持秒单位，缺失 token 不冒充零；TTS SSE 和兼容数量响应头保留原量，时长/字节估算记录算法；SSE 音频按解码后字节估算，完整零不覆盖。语音合成/转写显式终局与 EOF 分开，二进制读取失败保留可靠回执及不完整状态。图片内容计数标 adaptor；断流保留请求数量时标 estimate。
- 异步及即时完成任务、Midjourney 的终局参考费用在资金意图前保存；没有 token 回执明确记 unknown。资金失败和意图前退出后复用原证据恢复，未完成任务仍等待轮询。表达式价格快照冻结数值单位，原提交估算与完成提取分开；合法供应商积分小数和 Go 整数均保留。完成提取标 adaptor，不冒充原始供应商字段；成功状态和数量已提交、最终证据尚未保存的任务，用原冻结价格恢复费用；即时成功超出余额仍结算平台未收取部分，金额饱和保留管理员审计。Midjourney 已保存成功及按次费用、尚未保存终局证据时也可恢复。任务零费用按实际计价分支的完成量或冻结零价证明，缺失终局数量的估算零转核查；未使用的数量不阻止明确免费分支。
- 新账本音频、实时和文本结算及日志使用捕获的积分换算；工具单价从同一个价格索引捕获并保存于请求，包含明确零及模型前缀匹配；Gemini 输入音频单价亦保存于请求。执行期间修改工具单价不改变本请求收费。其他价格及尝试归属继续审查。

具体契约以[开发计划 §4.16](subscription-billing.md#416-第-9-轮用量与账单修正契约开发中)为准。`Uncollected` 表示未收取的参考收费，不证明真实供应商支出；不记用户欠款、不追扣后续充值。

### 最新阶段验证

以下均为实际运行结果，**不代表第 9 轮完成**。

| 验证 | 命令、结果与证据 |
| --- | --- |
| 严格真实矩阵 | `make test-database` 零跳过通过；SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11；`/tmp/new-api-round09-task-zero-final-matrix.log`。门禁要求每库完成证据恢复、序号冲突、零/未知、音频价格冻结、转写 JSON/SSE、语音合成 SSE/数量响应头/明确零、图片明细及工具价格冻结（10、0 两种初价）；部分 adaptor 计数的估算下限、异步任务费用超额后的实扣投影和重放、任务意图前保存终局证据后的恢复、即时完成证据和 Midjourney 超额/重新读取/意图写入失败重试也通过三库验证；资源积分 3.5、冻结单位、轮询成功和数量同存、原生 Go 整数量、成功状态保存后终局证据写入失败的自动恢复（后来改价不影响原收费）、即时完成超额和金额饱和审计、Midjourney 终局证据前退出恢复、明确零/缺失量、常量免费/条件免费及未知量保留原占用也必须每库完成 |
| 完整后端 | `make test` 根模块及独立 relaykit 通过；`/tmp/new-api-round09-task-zero-final-full.log` |
| 并发 | `go test -race ./model ./controller ./service ./pkg/billingexpr ./relay ./relay/channel/task/jsplugin ./setting/operation_setting ./relay/channel/openai ./relay/channel/claude ./relay/channel/gemini -run '^(TestCreditPackDatabaseMatrix\|TestCreditBillingDatabaseMatrix\|TestCreditTextQuota.*\|TestCalculateText.*\|TestTool.*\|TestGetTool.*\|TestCreditOpenai.*\|TestCreditResponses.*\|TestCreditClaude.*\|TestCreditGemini.*\|TestCreditTranscription.*\|TestCreditSpeech.*\|TestCreditImage.*\|Test.*Image.*\|TestSecurityAccountDeletion.*\|Test.*Task.*\|Test.*Usage.*)$' -count=1` 通过，无 race；可选外部 DSN 跳过不作三库证据；`/tmp/new-api-round09-task-zero-final-race.log` |
| 静态及门禁 | `go vet ./logger ./model ./controller ./service ./pkg/billingexpr ./setting/operation_setting ./relay/...`、`python3 bin/test_database_matrix_test.py`（6 个测试）、`git diff --check` 通过。`task-zero-final-vet.log`、`task-zero-gate-{red,green}.log`；短文件名前缀为 `/tmp/new-api-round09-` |
| 新建及真实发布版升级 | 确认当前发布版仍为 v1.0.0-rc.41（2026-09-30 发布）；以该版实际源码构建 seed。`python3 /tmp/new-api-round09-task-metadata-migration/run.py` 和 `fresh-run.py` 在 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19 全部通过，每库均两次 `InitDB` / `InitLogDB`。MySQL/PostgreSQL 使用独立日志库；账户/Key/订阅/订单/日志、窗口、证据原文及价格快照保留，最终证据关联、明确零/unknown、小数秒、防重重放和两种唯一约束均验证；新增任务快照的提交估算 4、冻结 credit 单位、完成提取 3.5 在重启后保持；证据在`/tmp/new-api-round09-task-metadata-migration` 的 `upgrade-result.log`、`fresh-result.log` 及各库 seed/verify/fresh 日志 |

升级验证履行结构兼容要求，不恢复已取消的历史用户商业迁移范围。以后本轮新增结构仍须再次验证，当前结构证据不能覆盖尚未实现的修正表。

### 缺口复现与审查修复

功能缺口的红绿证据包括 `usage-log`、`presence`、`stream-presence`、`evidence-recovery`、`sequence`、`media-log`、`responses-fields`、`compact-fields`、`gemini-fields`、`claude-fields`、`transcription`、`transcription-outcome`、`image-fields`、`speech`、`speech-headers`、`task-evidence`、`task-recovery-preintent`、`task-fields`、`task-polled`、`task-terminal-gap`、`mj-terminal-gap`，前缀 `/tmp/new-api-round09-`，具体协议边界维护于开发计划。已确认实现缺陷的复现、修复与回归单独记录于[Bug 归档](../archive/bugs/accounting-development-baseline.md)：B41/B42 音频倍率及金额绑定，B43 账户删除，B44/B45 缓存及零费用，B46 部分计数投影，B47 无图片内容的完成事件，B48 工具价格与文本换算，B49 新语音回执的分量和读取失败，B50 途中适配器合计的下限，B51 异步任务内存投影错用参考费用，B52 Midjourney 超额重放，B53 新证据写入失败后的旧执行资格，B54 日志/测试共享状态竞争，B55 未启用 Redis 仍启动返还缓存任务，B56 计量拒绝原生整数，B57 即时成功费用超过余额时丢弃结算，B58 把缺失完成量的任务估算零当作真实免费。新增功能不作为既有 Bug。

原失败的 `protocol-fields-green.log` 和误改共享 SQLite 夹具后中断的 `audio-reviewed-full.log` 不计为成功；B43 修正夹具范围并补出身份消失的确定性失败后，三个数据库的针对性删除测试、完整回归及 race 均通过。早期 `primary-cache-zero-red.log` 的合成矛盾用例预期已纠正，不作为有效 Bug 证据。扩大并发范围首次发现日志计数、测试共享状态竞争，随后发现未启用 Redis 的空后台任务；`task-fields-race.log` 和 `task-fields-reviewed-race.log` 均失败，不作通过证据。修复后的 `task-numeric-final-race.log` 通过。阶段性缺 import 或调用方式错误的日志也不作为通过结果。本次最早的 task-evidence-green 误筛选显示 no tests to run，随后覆盖实际任务子测试的 SQLite 完整分支、严格三库和完整回归均通过；空筛选不计作验证。

### 已验证阶段：受控账单修正（第 9 轮仍在开发）

已实现不可变财务修正和管理员证据、原 FEFO 分配返还（含结算补占）、到期/撤销归属、原多窗口代次更新、Key/用户统计一次返还、并发版本冲突及完整写入失败回滚。调高费用只增加平台未收取量，不再扣款。原账单、终局凭证、原分配和消费日志均保留。核账额外验证操作、凭证指纹、原分配与返还流水；破坏凭证或移除返还关联会报告差异。

用户/管理员 API、scope、归属和字段隔离见开发计划 §4.16。新账本的任务费用由资金事务维护，任务重放读取净扣款，旧内存对象的保存/CAS/计费状态回写不能覆盖修正。独立 SQL 修正日志支持提交成功但主库确认失败的重放；有限重试达到上限进入核查，管理员恢复后仍只写一条。

先失败再通过的日志：`bill-adjustment-{red,green}`、`bill-adjustment-review-red` / `bill-adjustment-reviewed-green`、`adjustment-delivery-{red,green}`、`adjustment-task-replay-behavior-red` / `adjustment-task-replay-green`、`adjustment-stale-task-{red,green}`、`adjustment-api-{red,green}`、`adjustment-worker-{red,green}`、`adjustment-audit-red` / `adjustment-audit-reviewed-green`；统一前缀 `/tmp/new-api-round09-`。第一次任务修正测试的 `adjustment-task-replay-red.log` 是错误包别名导致的编译失败，不作为行为红证据；首次审计实现 `adjustment-audit-green.log` 因错误假设分配表存在 user_id 而失败，修正为原操作/分配/积分包归属校验后重新通过。并发夹具读取最新记录前必须清空旧主键，恢复被改坏的凭证前单独保存原指纹。

| 实际命令 | 结果与证据 |
| --- | --- |
| `make test-database` | SQLite 3.50.4 / MySQL 8.4.11 / PostgreSQL 15.19 / Redis 7.4.11，严格零跳过；`/tmp/new-api-round09-adjustment-final-matrix.log` |
| `make test` | 根模块及独立 relaykit 完整通过；`adjustment-final-full.log` |
| `go test -race ./model ./controller ./service ./pkg/billingexpr ./middleware ./relay ./relay/channel/task/jsplugin ./setting/operation_setting ./relay/channel/openai ./relay/channel/claude ./relay/channel/gemini -run '^(TestCreditPackDatabaseMatrix|TestCreditBillingDatabaseMatrix|TestCreditTextQuota.*|TestCalculateText.*|TestTool.*|TestGetTool.*|TestCreditOpenai.*|TestCreditResponses.*|TestCreditClaude.*|TestCreditGemini.*|TestCreditTranscription.*|TestCreditSpeech.*|TestCreditImage.*|Test.*Image.*|TestSecurityAccountDeletion.*|Test.*Task.*|Test.*Usage.*|Test.*AccessToken.*)$' -count=1` | 扩展竞争回归通过；`adjustment-final-race.log`，未配置外部 DSN，不代替三库证据 |
| `go vet ./logger ./model ./controller ./service ./middleware ./pkg/billingexpr ./setting/operation_setting ./relay/... ./router` | 通过；`adjustment-final-vet.log` |
| `python3 -m unittest discover -s bin -p test_database_matrix_test.py` | 6 个自测通过，遗漏三库修正/投递/API/任务契约均拒绝；`adjustment-gate-{red,green}.log` |

阶段验证日志统一前缀 `/tmp/new-api-round09-`。新增 `CreditBillAdjustment` 已验证三库迁移：`/tmp/new-api-round09-adjustment-migration/run.py` 用实际发布版 v1.0.0-rc.41 的 seed，再运行当前 `verify.go`；`fresh-run.py` 运行 `fresh.go`，两者均加入 `adjustment-check.go`。构建命令为 `go build -o /tmp/new-api-round09-adjustment-migration/verify /tmp/new-api-round09-adjustment-migration/verify.go /tmp/new-api-round09-adjustment-migration/adjustment-check.go`，fresh 对应替换名称；随后分别执行两个 Python 入口。三个数据库的新建和发布版升级均执行两次 InitDB/InitLogDB，验证已发布用户/Key/订阅/订单/日志保持、新修正及管理员证据/原价格/过期返还/日志确认保持、防重重放和三个独立唯一约束。SQL 独立日志库同样验证；结果在同目录 `upgrade-result.log`、`fresh-result.log` 及各库日志。

此阶段通过不是第 9 轮完成；ClickHouse 契约、多实例完整故障演练和容量验证仍按第 12 轮执行。新增未知账单人工终结、其余协议等仍列在下方；不提前提交本轮或进入第 10 轮。

### 审查修复：删除 Key 后的在途结算

原请求已准入，Key 随后软删除；原结算按正常 Key 查询找不到记录，停在待结算。修复仅让已准入账务更新历史 Key 计数；新请求仍按删除标记拒绝，凭证不会恢复。修正返还同样更新历史计数。已验证原预占 40、最终实扣 25、Key 75/25、包 75/25/0，以及删除后新请求拒绝、原来源被冻结后的返还归属。

真实失败 `deleted-key-red.log`；第一次实现的 `deleted-key-green.log` 因复用 GORM 查询对象生成歧义更新失败，使用独立会话后 `deleted-key-reviewed-green.log` 通过。最新源代码的严格三库矩阵、完整根模块/独立 relaykit、上述扩展 race 及 vet 均通过，证据 `/tmp/new-api-round09-deleted-key-final-{matrix,full,race,vet}.log`，数据库版本同上。门禁遗漏这个契约时先失败，再通过 6 个自测：`deleted-key-gate-{red,green}.log`。此修复没有新增或改变表结构，先前修正表迁移结果继续适用。

### 已验证阶段：未知账单人工确认（第 9 轮仍在开发）

实现和权限契约见开发计划 §4.16。确认与追加凭证、恢复唤醒同事务，原未知终局不改写。管理员需明确原凭证关联、外部参考、原因及新计量；未完成任务和有效执行租约拒绝确认。审批不是已完成扣款；后续使用原来源和无欠款规则结算。旧执行代次、自动任务失败、迟到计量和原释放入口不能撤销已确认结果。费用 25、明确免费 0 和费用 120/实扣 100/平台未收取 20 均验证；并发的不同确认仅一个成功，相同事件重放保持原结果。

故障验证：确认写入失败不留下新凭证或操作；确认提交后可自动恢复；资金事务失败保留 40 原占用，金额可查询，后续只扣一次。核账及账单查询检查新凭证指纹、操作、原凭证、事件、价格及金融意图关联，破坏凭证不能被正确总金额掩盖。SQL outbox 使用新计量生成唯一日志，管理员参考不进入用户日志。

真实红/绿证据前缀 `/tmp/new-api-round09-`：`bill-review-{red,green}`、`bill-review-audit-behavior-red` / `bill-review-audit-green`、`bill-review-api-red` / `bill-review-api-green`、`bill-review-task-red`、`bill-review-late-failure-{red,green}`、`bill-review-fence-{red,green}`、`bill-review-payload-red`（通用 payload/fence 修复后一并通过）。第一次生成审查用例的脚本因匹配到两处而未写文件，随后运行的 `bill-review-audit-red.log` 为既有用例通过，不作为行为红证据；重新加入用例后才产生 `audit-behavior-red`。

最新源代码 `make test-database`（SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11，零跳过）、`make test`（根模块及独立 relaykit）、前表完整扩展 race 命令、`go vet ./logger ./model ./controller ./service ./middleware ./pkg/billingexpr ./setting/operation_setting ./relay/... ./router` 均实际通过，日志 `/tmp/new-api-round09-bill-review-final-{matrix,full,race,vet}.log`。门禁强制每库完成人工核查核心及完整 API/任务/Midjourney 子矩阵；遗漏自测先失败两项，再通过全部 6 个自测：`bill-review-gate-{red,green}.log`。

新增 `ReviewEvidenceID` 的三库迁移验证 `/tmp/new-api-round09-bill-review-migration/`：构建 `go build -o …/verify …/verify.go …/adjustment-check.go …/review-check.go`（路径展开为该目录），fresh 对应替换名称；实际执行 `python3 …/run.py` 和 `python3 …/fresh-run.py`，日志 `upgrade-result.log` / `fresh-result.log` 全部通过。真实发布版 v1.0.0-rc.41 seed 及新建各执行两次 InitDB/InitLogDB，保留已发布数据、先前元数据/修正约束，验证确认提交后重启时尚未扣款、原凭证与确认分别保持、操作人降权不阻止已合法确认的恢复、有限来源结算、独立 SQL 日志重放只一条；已降权操作人不能重复确认，恢复授权后原事件防重结果不变。

进一步审查确认入口，发现 estimate 数量也被当作已核实依据。先以费用 0、25、120 三种情况复现失败，再收紧来源校验；仅 estimate 不可确认，完整 upstream/adaptor 保持可用。真实红证据 `/tmp/new-api-round09-bill-review-source-red.log`；修复后上述严格三库、完整后端、扩展 race 和 vet 均通过，最新源代码日志 `/tmp/new-api-round09-bill-review-source-final-{matrix,full,race,vet}.log`。本次没有改变结构，人工确认阶段的三库新建/发布版升级及重复启动结果继续适用。缺陷单独归档 B64。

本阶段没有进入第 10 轮，也未提交不完整的第 9 轮。

### 已验证阶段：文本估算版本与计数依据（第 9 轮仍在开发）

输入估算在实际计数时捕获原模型、开关、分词器或启发式权重、消息/工具格式附加量及媒体合计，随原价格快照保存；终局使用原捕获，不用结束时的新配置补造历史依据。输出按实际映射后模型计数，保留多段分别舍入和工具附加量。分词器依赖版本来自构建信息，无法取得则明确 unknown。完整供应商零值不附本地估算描述；本地数量小于部分回执时，保留回执下限及独立本地估算值。描述和金额都不保存提示词或生成内容。跨增量合计不冒充某个单次计数器，原不可变事件保持各自依据。

真实缺口红证据 `/tmp/new-api-round09-estimator-behavior-red.log` 的 enabled_true 分支缺少价格快照计数依据；该次 enabled_false 受前一失败用例未清理已关闭上游影响，不计作有效业务红证据。最初 `estimator-red.log` 夹具未发包得到额度不足，后补齐真实发包；第一 `estimator-green.log` 误修改了媒体辅助函数中的开关引用，造成编译失败，修正为捕获开关传入该计数入口后重新验证。`estimator-validation-red.log` 明确复现 upstream 也能附估算描述；后续未被拒绝的同序号输入产生级联冲突，不把冲突作独立缺口证据。新校验拒绝非法来源、缺少版本、负数量/参数；合法描述防重重放、变更描述冲突和原计数依据保持均通过。计数单测补齐真实 ChannelMeta；早期编译/空筛选和未初始化夹具的错误均不是行为通过证据。

最新源代码 `make test-database` 在 SQLite 3.50.4 / MySQL 8.4.11 / PostgreSQL 15.19 / Redis 7.4.11 严格零跳过通过；`make test` 根模块和独立 relaykit 全量通过；前表的扩展 race 命令加入 `TestCreditTokenEstimatorProvenance` 后通过；同范围 vet 通过。日志 `/tmp/new-api-round09-estimator-final-{matrix,full,race,vet}.log`。三库门禁要求两个计数开关分支及元数据模型验证，遗漏时先失败三项、更新后 6 个自测通过：`estimator-gate-{red,green}.log`。

估算 JSON 契约的三库 fresh/release 验证在 `/tmp/new-api-round09-estimator-migration/`：实际构建 verify/fresh，均加入原 adjustment-check.go、review-check.go 及 estimator-check.go；实际运行 `python3 …/run.py`、`python3 …/fresh-run.py`，三个数据库均两次 InitDB / InitLogDB 通过。使用真实 v1.0.0-rc.41 seed，既有数据和原约束保留。新增检查输入 Claude 权重 1.13、输出 gpt-4o 的 o200k_base / 构建依赖 v0.6.2 在重启后仍保持；最终证据提交、金融意图前的恢复不改原凭证，独立 SQL 日志保持原计量且重放只一条。`upgrade-result.log`、`fresh-result.log` 和各库日志为实际结果。本阶段没有新增表列，但 JSON 兼容仍履行三库要求。媒体逐项参数、其他音频/实时估算和完整尝试价格归属继续执行。

### 仍需完成

其余适配器及异步图片的原字段与部分回执、二进制音频流的输出阶段记录、完整尝试/输出阶段及供应商成本依据、执行中预算停止、插件原字段来源、估算模型和完整价格版本归属，以及本轮最终结构和完整 review/回归/提交。完成前保持第 9 轮执行中，不进入第 10 轮；整个 goal 仍在执行。


### 开发阶段：OpenAI Chat 流式预算（第 9 轮仍在开发）

新增监测及处理契约见设计 §7、开发计划 §4.16，未宣称其余协议已覆盖。余额不足的真实 HTTP 失败 `stream-budget-red.log`：继续转发了触发不足内容和尾段，并发送正常 DONE；实现后 reported/estimated 两条通过。按次和足额 token 收费继续正常完成；最终依据保存预算停止原因，途中输出回执不能因扫描器先看到 DONE 而升级为完整量。存储故障停止并核查，原账单尚未收费，错误不泄露内部详情。

`stream-budget-storage-red.log` 另复现裸 JSON 破坏 SSE，针对性 green 已通过，B65 尚待最终完整回归。`stream-budget-contracts-green.log` 是错误包别名导致的编译失败；修正后 `stream-budget-contracts-reviewed-green.log` 五条真实 HTTP 契约通过，不能把编译失败当作行为红证据。门禁遗漏自测 `stream-budget-gate-red.log` 五个遗漏未被拒绝；新增每库要求后 `stream-budget-gate-green.log` 的 6 个测试通过。

首次完整三库和根模块/独立 relaykit 回归通过（`stream-budget-final-matrix.log`、`stream-budget-final-full.log`），vet 通过。扩展竞争检查 `stream-budget-final-race.log` **失败**：监测复制整个 RelayInfo，与扫描器写入接收计数竞争。已改为只投影由流处理者持有的计量、价格和资金字段，完整复验仍执行。首个窄筛选 `stream-budget-race-green.log` 没有执行 controller 用例，不计作该问题通过证据；随后使用正确顶层筛选重新验证。所有短文件名前缀为 `/tmp/new-api-round09-`。


复验完成：`stream-budget-race-reviewed-green.log` 正确执行 controller 与扫描器/计价竞争回归，退出 0；随后 `make test-database`、`make test`、前述扩展 `go test -race`（本次另包含 `./relay/helper` 与 `TestStreamScanner.*`）、前述 `go vet` 均退出 0，证据前缀 `/tmp/new-api-round09-stream-budget-reviewed-final-`。严格三库使用 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19 和 Redis 7.4.11，零跳过。B65/B66 已归档。

新建及真实 v1.0.0-rc.41 升级后的两次 `InitDB` / `InitLogDB` 通过：在 `/tmp/new-api-round09-stream-budget-migration/` 构建 verify/fresh（各联合 adjustment-check.go、review-check.go、estimator-check.go、budget-check.go），运行 `python3 …/run.py` 和 `python3 …/fresh-run.py`，三库退出 0；upgrade-result.log / fresh-result.log 及各库日志保留。新增重启样本预占 40、总合法积分 50、参考费用 60，恢复实扣 50、平台未收取 10，无用户欠款；原终局不改写，停止原因随独立日志保存，重复投递仍一条且核账无差异。本阶段未增加 schema，结构、证据及预算停止元信息均验证。第 9 轮仍需其余原生流协议、完整尝试/价格依据及其他已列未完成项，不提交半轮或进入第 10 轮。


### 开发阶段：Responses、Claude、Gemini 流式预算（第 9 轮仍在开发）

新增原生 Responses SSE 预算投影，早期估算不改写原事实、不提前 Finish；临时图片计数隔离于原工具对象。预算错误使用 Responses 的 error 事件，存储失败在已发送 response.created 后仍返回流式错误并保留核查；足额 token 与按次价格正常完成。真实失败 `responses-budget-red.log`，实现后 green、扩展不足/存储/正常/按次的 reviewed-green 均通过。门禁 4 个遗漏先未被拒绝，gate-red 失败；新增要求后 gate-green 的 6 个测试通过。

Claude 原生 Messages、Chat 转换、Responses 转换共用原缓存/工具计量和最终估算；在转发当前事件前检查预算。Gemini 原生、Chat 转换、Responses 转换保留分量与合计来源，预算停止后不执行成功收尾。三入口分别先失败 `claude-budget-red.log`、`gemini-budget-red.log`；claude-budget-green、native-budget-green 针对性回归通过。Gemini 合计不附候选输出计数器描述，候选量自己的描述仍保留。六个分支的门禁遗漏先失败 native-budget-gate-red，随后 native-budget-gate-green 6 个测试通过。

审查另用持续打开的真实 HTTP 流复现 B67：Gemini 转 Responses 的预算错误之后又发送 server_error 和 response.failed，把本地额度不足当作供应商失败。测试通过上游请求取消同步确定停止，没有等待时长或吞吐假测试。真实失败 gemini-budget-outcome-red；已将预算停止置于通用失败转换之前，完整复验仍执行。新增原生协议预算属于功能契约，B67 单独记录，未据局部通过关闭第 9 轮。上述短文件名前缀 `/tmp/new-api-round09-`。


native-budget-reviewed-green 的 HTTP/原生适配器/计价针对性回归退出 0；native-budget-final-full（根模块及独立 relaykit）、native-budget-final-race（含扫描器的扩展范围）、native-budget-final-vet 均退出 0。native-budget-final-matrix **失败**：新增长用例名称被夹具放进 varchar(32) 的 AffCode，MySQL 1406/PostgreSQL 22001 拒绝；SQLite 没有限长校验而通过。这是测试数据缺陷，未改宽生产字段或放松业务断言。已给新用例显式使用独立、短的合法用户/推荐码，严格三库重新执行，结果待记录。正常慢测试与共享机器负载不当作开发门禁，不中止无关项目进程。


夹具修正后的 `make test-database` 退出 0：SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19 和 Redis 7.4.11，严格零跳过，包含每库 15 个流式预算分支；`/tmp/new-api-round09-native-budget-reviewed-final-matrix.log`。生产源代码与之前退出 0 的 native-budget-final-full/race/vet 相同，仅修正测试用户/推荐码长度，未继续重复无变化的全量检查。B67 完成回归并归档。未新增数据库结构，此前 stream-budget-migration 的真实新建/发布版升级及两次启动证据继续适用；新协议的资金行为由本次真实三库验证，不用旧阶段结果代替。第 9 轮继续价格/尝试及已列未完成项。

### 已验证阶段：重试实际价格与费用归属（第 9 轮仍在开发）

`CreditEvidenceInput` 的可选 `attempt_price` / `attempt_price_evidence_id` 不改变历史凭证的 JSON 指纹。每次发送以独立 attempt 阶段保存渠道、分组、计费名、映射模型名、协议及实际价格 JSON/摘要，与提交标记同事务；重复标记只一条，改变价格复用同次编号拒绝。最终消费自动关联对应价格；异步终局关联最新提交尝试。初始 PriceSnapshot 保持不变，换算和工具/音频单价采用已捕获值。无供应商采购价时继续未知，未增加伪造成本。

针对性用例沿真实积分、Key、原生阶梯结算和修正链验证：首组倍率 1、第二组倍率 2；运行配置换算改为 5000000 后仍使用本请求 500000；p12/c8 实扣 56；前一渠道统计不增加，第二渠道记 56；修正至 40 只返还 16 且调整第二渠道。故障注入使提交标记、价格记录一起回滚；损坏价格后账单查询拒绝、核账报告凭证关联差异。初次缺口失败 attempt-price-behavior-red（两次记录为零）、attempt-routing-red（费用归错渠道）、attempt-price-proof-red（普通账单未验凭证）；修复后 SQLite 全部信用账务与控制器分支通过 attempt-price-sqlite。首个 attempt-price-red 是测试字段编译错误，gate-red 是错误的 Python 导入命令，均不是行为证据；gate-behavior-red 才复现漏跑仍通过，修复后 Python 6 个门禁测试通过。

`make test-database`、`make test`、既有扩展 `go test -race`（含扫描器、model/controller/service/relay 和三个原生协议）、`go vet ./logger ./model ./controller ./service ./middleware ./pkg/billingexpr ./setting/operation_setting ./relay/... ./router` 全部退出 0，日志前缀 `/tmp/new-api-round09-attempt-price-final-`。真实 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，严格零跳过，新增每库重试价格契约门禁。改动期间未边运行验证边改生产源文件。

新建/发布版升级及两次启动复验保留于 `/tmp/new-api-round09-attempt-price-migration/`：verify/fresh 联合 adjustment-check、review-check、estimator-check、budget-check、attempt-check 编译；upgrade 从实际 v1.0.0-rc.41 生成的代表库开始，原用户余额/Key/任务及唯一约束保持。新增样本在第二次发送价与最终计量提交后、资金意图前重启，恢复按原 56 结算，两条尝试不变，日志归实际渠道/分组且独立投递一次，核账无差异。迁移执行结果在各库日志及 upgrade-result/fresh-result 中记录；未增加表结构。

尚须完成其余转换链及音频/实时预算、完整发送/回执/客户端输出阶段、其他协议和任务原量来源、媒体估算版本，随后整轮最终 review/验证/提交。保持第 9 轮执行中。

### 开发阶段：Responses WebSocket 预算与关闭边界

同一连接每次 create 独立建账的原契约保持。当前响应事件原量及累计数量先采集，再复用 Responses 预算检查；不足时不转发触发帧和正常终局，按观察数量及原合法资金来源结算，发送带 create 事件 ID 的原生错误并关闭连接。求值/存储失败走核查，不保存收费意图。既有不同 stream_id、迟到旧响应和控制错误隔离保持。

失败 `/tmp/new-api-round09-ws-budget-red.log` 复现超额输出与完成仍到达客户端，存储故障也在完成后才处理。四类 targeted/proof-green（不足、存储失败、充足、按次固定价）通过；gate-red 四个漏跑都未被拒绝，补入三库门禁后 Python 6 项通过。首个 ws-budget-green 是编译字段错误，不是通过证据。

完整 ws-budget-final-matrix/full/race 失败，vet 通过：B70 显示错误后仍可能收到缓冲后续事件。读协程在 done/current 收尾间隙直转后续帧；取消连接在放开准入之前完成，通用写入在写锁内检查取消，避免迟到内容绕过预算。正在跑修复后原生 WebSocket 和账务回归，随后重跑完整验证。上一次 final 日志均不作为成功证据，第 9 轮未提交。

修复后 `make test-database`、`make test`、扩大到 WS 原生单测/端到端的扩展 race、上述 vet 全部退出 0，前缀 `/tmp/new-api-round09-ws-budget-reviewed-final-`。SQLite 3.50.4 / MySQL 8.4.11 / PostgreSQL 15.19 / Redis 7.4.11，三库严格包含四个 WS 预算分支，未跳过。close-green 执行了 controller 账务及原有 WebSocket 回归，但 relay 包筛选为空，不算该包测试；完整和扩大后的 race 才覆盖其单测。B70 归档。本次只改 WS 控制流与已验证的预算调用，没有新增数据库结构或更改 SQL/金额模型；前一实际价格阶段 fresh/released 两次启动证据继续适用，新增传输资金行为由本次真实三库验证。第 9 轮继续开发，不提交半轮。

下一审查项：异步任务恢复仍从准入价格重算，需用失败行为验证分组重试或选择条件改变后是否与实际提交价格一致；已确认错误才进入 Bug 目录，不把待查项当作问题。

### 已验证阶段：异步任务实际尝试价格与重启恢复

B71 的 task-attempt-price-red 在已有插件轮询夹具内复现：3.5 供应商积分、第二次倍率 2/0，应为 35/0，恢复都收 18。用例模拟已记录的第二次提交检查点；不声称该夹具做了实际网络重试。收费分支追加到原生预占目标 40，零倍率保留原占用；成功状态和数量先落库，在最终证据写入处失败，再改变管理价格，验证恢复使用实际尝试而非当前设置或初始倍率。最终第二次尝试关联、数量单位、零价依据、实扣及重放均正确。

`creditSubmittedPrice` 为异步最终明细及恢复提供同一不可变价格依据，任务完成原量不被替换；历史内部请求无尝试证据时保持原契约。原准入 PriceSnapshot 不修改。收费组 35、免费组 0 均通过，后续追加和账单修正仍由原资金来源及原实际渠道核算。

完整 task-attempt-price-final-matrix/full/race/vet 全部退出 0（命令与 WS-reviewed 阶段相同），SQLite 3.50.4 / MySQL 8.4.11 / PostgreSQL 15.19 / Redis 7.4.11 严格零跳过，新增两条任务恢复门禁。gate-red 复现两个漏跑未拒绝，gate-green 的 Python 6 项通过；已有所有任务分支 targeted task-attempt-price-green 通过。日志前缀 `/tmp/new-api-round09-`。

实际新建及 v1.0.0-rc.41 代表库升级、每库两次 InitDB/InitLogDB 退出 0，目录 `/tmp/new-api-round09-task-attempt-price-migration/`。verify/fresh 联合先前五个检查 helper 和 task-attempt-check.go 构建，运行 run.py、fresh-run.py，upgrade-result/fresh-result 全通过。第二次实际价格与任务 SUCCESS/3.5 数量已提交、终局证据尚未创建的检查点，重启恢复实扣 35/0，两个尝试保留、最终关联第二个版本、免费依据不丢失、核账无差异；先前原数据、约束及独立日志样本继续通过。B71 归档，第 9 轮未完成或提交。

### 开发阶段：Chat / Responses 双向转换的用量与预算

六条真实 HTTP 场景（双方向各不足、足额、存储失败）首次失败 `/tmp/new-api-round09-conversion-budget-red.log`：超额片段和正常完成仍转发，存储失败到最后才处理。转换前原量作为费用来源，Responses 复用原累加器，Chat 保留明确字段并按原文本/工具规则补缺；只在通过预算后转发。正常 Chat 的途中 30 输出不是完整回执：长文本估算较大时保留下限，不能为了预期 60 把途中数量当作最终量；足额测试改为明确终局附 30 数量，验证应收 60。两个方向的存储失败 HTTP 状态按是否已输出分别 200/500，错误仍为 SSE，不放宽费用/内容断言。六条 targeted reviewed-green 退出 0。

客户端 Responses → 上游 Chat 非流式请求的 reported / missing / cacheunknown / cachezero 四条扩展既有真实数据库表；临时撤去该 JSON 修复后，conversion-json-red 明确复现 reported 和 cachezero 错入核查，再恢复修复。SQLite 完整 controller 账务回归 conversion-sqlite-green 退出 0，覆盖六条预算与四条 JSON 场景。conversion-budget-green 的 controller 和 conversion-proof-green 均因错误的带斜线括号筛选未执行测试，不计作行为通过证据；同文件中原协议单测按实际执行结果保留。门禁遗漏十条契约时 conversion-gate-red 十项失败；新增各库执行要求后 conversion-gate-green 全部 6 个门禁自测通过。

B72 独立记录于 Bug 目录，完整严格三库、根模块/独立 relaykit、扩展 race 和 vet 正在执行，未关闭第 9 轮或提交半轮。所有短日志名前缀 `/tmp/new-api-round09-`。本次只改转换的计量/控制流，没有新增表结构；前一 task-attempt-price-migration 的实际 fresh/released 双启动结构与持久化 JSON 兼容结果继续适用，新增计费行为以本次三库结果为准。

转换修复最终验证：`make test-database`、`make test`、原扩展 race 命令增加 `./relay/channel/advancedcustom` 及 `TestOai.*|Test.*AdvancedCustom.*|Test.*ResponseModel.*`、既有范围 vet 全部退出 0，证据 `/tmp/new-api-round09-conversion-final-{matrix,full,race,vet}.log`。SQLite 3.50.4 / MySQL 8.4.11 / PostgreSQL 15.19 / Redis 7.4.11 严格零跳过，每库六条转换预算和四条 JSON 契约均执行。随后仅增强测试断言，确认正常转换终局的数量来源为完整 upstream、没有本地估算描述并关联本次价格；新的 `make test-database` 和 `go test -race ./controller -run '^TestCreditBillingDatabaseMatrix$' -count=1` 均退出 0，证据 conversion-proof-final-matrix/race。生产源代码与此前完整回归/扩展 race/vet 相同，未重复无变化的全量。B72 已移至归档。继续语音流预算、缓冲 SSE 转 JSON、完整阶段记录及已列未完成项，不进入第 10 轮。

### 开发阶段：转写及语音合成 SSE 预算

扩展原 HTTP/真实数据库行为表，每类语音各不足、存储失败、正常完成三个分支。转写夹具上传真实一秒 WAV 并调用 multipart 入口；合成夹具提供公开兼容 SSE 的 base64 字节流，不声称供应商实际调用。audio-budget-red 明确复现超额内容、终局仍到达，以及存储故障最后才处理。监测先累积观察量，在独立投影中使用原缺失/部分估算及正确分类价格，再发送当前片段；不足不发送语音终局，原量结算封顶；未知故障保留核查。合成已观察 30001 字节，按原 ceil(byte/1000) 得 31，参考费 62、实扣 50、平台未收取 12。完整 31 回执也正常收费 62。转写输出按原文本估算，不伪造音频 token。

第一次 audio-budget-green 因 root types 与 relaykit types 别名混淆编译失败，不是行为证据；修正后 audio-budget-reviewed-green 只剩存储故障 HTTP 状态预期错误：该表达式初始占用为 0，首个片段的增量就被故障拒绝，尚未写出客户端内容，因此为 500 SSE。改成与实际发送阶段一致的预期，没有放松资金/内容断言。audio-budget-sqlite-green 完整 controller 信用账务回归退出 0。门禁遗漏六条用例时 audio-budget-gate-red 失败，补齐每库要求后 gate-green 的 6 个自测通过。B73 独立待关闭，严格三库、完整根模块/独立 relaykit、扩展 race/vet 正在运行。日志前缀 `/tmp/new-api-round09-`。

无新增结构或 JSON 字段，既有 fresh/released 两次启动的持久化兼容证明继续适用；语音的新增资金行为由本次三库验证。第 9 轮仍需实时语音、缓冲 SSE 转 JSON、完整阶段记录和其他已列事项，不提交半轮。

首批 audio-budget-final-matrix/full/race/vet 均退出 0。审查再增强持续上游取消验证和旧音频倍率冻结：实际有两个 SSE 场景只发送前两帧后等待请求取消，停止时确实关闭上游；新增 ratio 价格原 audio=2、audio_completion=3，生成期间运行配置改为 20/30，仍按原 31*6=186 参考费用，实扣 50，平台未收取 136。临时去掉监测的价格冻结，audio-budget-frozen-behavior-red 复现错误地在首帧提前停止并只记录 6；恢复后 close-frozen-green 的完整 SQLite controller 回归通过。首个 frozen-red 是测试未声明 legacy 字段的编译失败，不作为行为红证据；新增门禁分支 frozen-gate-red 先失败，补齐后 6 项 frozen-gate-green 通过。

最新仅测试增强后的 audio-budget-proof-final-race 退出 0；audio-budget-proof-final-matrix **失败**：本任务专用 PostgreSQL tmpfs 存满 7.8 GiB，SQLSTATE 53100，建表之前失败，未作为账务通过。确认 compose 明确为 disposable 测试库、标签归属于本仓库后，只重启 new-api-accounting-test-postgres-1 清空其临时测试数据；新健康实例 PostgreSQL 15.19、tmpfs 68.6 MiB，独立日志库查询成功。原迁移原始日志及 seed/verify 程序保留。严格全矩阵正在重跑 audio-budget-reviewed-proof-final-matrix；没有更改业务代码、放松门禁或干预其他项目服务。

环境恢复后的 `make test-database` 实际退出 0，audio-budget-reviewed-proof-final-matrix：SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19 和 Redis 7.4.11，严格零跳过，包含每库七个语音预算分支和持续上游取消。生产源代码与已通过 audio-budget-final-full/race/vet 相同，只增强测试；最新 controller race 也通过。B73 已归档；空间耗尽那次失败日志保留，不计作通过。下一项实时语音预算与估算依据仍需实现。

### 开发阶段：原生 Realtime 预算（阶段验证完成）

一条连接的已完成回执与未完成文本/音频观察量分开累加，使用冻结表达式或原音频倍率在转发前检查预算；完整回执替代本次估算，重复事件不重复收费。不足关闭上游并返回原生错误，观察参考量按合法额度封顶；求值/存储故障保留 review/no intent。新增客户端输入在发送前检查，未发送的拒绝输入不收费；服务器的输入确认不重复作为输出。

既有 controller 表七个真实 WS 场景：文本/PCM16 不足、存储失败、足额、已完成回执叠加当前输出、拒绝输入、足额输入确认。realtime-budget-red 实际退出 1，前两条仍转发越界输出，存储失败错误保存 settle 意图。初始四条实现后通过；新增拒绝输入边界第一次失败（boundaries-green 实际退出 1），修复客户端 create 计数后六条通过；ack-red 复现同一输入被确认回执重复计为输出、参考 12/实扣 10，修复后足额完成参考/实扣 8。日志文件名不代表结果。

完整根模块/独立 relaykit、严格三库/Redis、扩大含原生 Realtime 的竞态检查及 vet 全部退出 0，前缀 `/tmp/new-api-round09-realtime-budget-final-`。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，七分支每库零跳过；门禁七个遗漏先失败，补齐后 6 项自测通过。B74 已归档；本阶段未改变结构，已验证的新建/发布版升级结构沿用，不冒充整轮完成或提交。

### 开发阶段：缓冲 Responses 预算（阶段验证完成）

非流式客户端遇到上游 SSE 时，缓冲转换与原生流共用计量累加器，在各事件观察后检查同一资金来源和冻结价格。不足立即关闭上游，返回一个 JSON 错误，没有部分内容或正常完成；已观察参考费仍按合法资源封顶，未收取部分由平台承担。未知求值/存储故障保留核查，正常及固定价格保留完整 JSON 和原来源。

既有 controller 文件中的四条真实 HTTP 场景验证不足、存储故障、足额、固定收费，以及两条未结束上游的取消。buffered-budget-behavior-red 实际退出 1：不足返回完整成功 JSON，存储失败返回成功且 pending/settle。buffered-budget-red 只记录测试 int/int64 编译错误，不作为行为红；修复后 green 与 close-green 实际通过。门禁四个遗漏先失败，补齐后 6 项自测通过。

make test-database、make test、含 buffered/OpenAI/转换/Realtime/WS 的扩展 race、vet 全部退出 0，前缀 `/tmp/new-api-round09-buffered-budget-final-`。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，四分支每库零跳过。未改数据库结构；B75 已归档。本轮仍需要完成阶段证据和估算依据等剩余项，再做整轮 review 与提交。

### 开发阶段：语音与实时估算依据（阶段验证完成）

合成 SSE 的字节估算和二进制 PCM 时长估算已经保存实际映射模型、版本、计数方法和参数，已核实回执不附本地描述。audio-estimator-red 在既有 SQLite controller 表实际复现缺少描述；green、严格三库/Redis、controller/OpenAI race、vet 退出 0，前缀 `/tmp/new-api-round09-audio-estimator-`。未改表结构或价格；这是本轮新增设计能力，不作为既有计费错误归档。

Realtime 每个实际计数事件单独保留描述，最终连接合计与单个分词结果分开。输入/输出音频保留原算式及截断、同次解码的采样参数，文本保留该次实际配置；未知格式明确本地回退。拒绝未发出输入与确认回执不另记录计数，未知写入失败停止并核查。realtime-estimator-red 的六个收费场景实际复现无原计数凭证；实现后的 green 和 reviewed-green 通过。第八条故障场景在估算凭证 Create 注入失败，返回隐藏内部细节的错误且 review/no intent；新增严格门禁遗漏先失败、补齐后 6 项自测通过。

realtime-estimator-final 的严格三库/Redis、完整根模块/relaykit、定向竞态检查和 vet 全部实际退出 0。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，第八个凭证写入故障场景每库零跳过。未改表结构，新增 JSON 描述沿既有有界验证；整轮尚未提交。

### 开发阶段：请求与输出阶段证据（阶段验证完成）

在既有 controller 三数据库流预算表，给估算不足、足额 token 和固定价格三个场景加入阶段凭证断言。relay-phases-red 实际退出 1：都没有 upstream_response、client_write_possible、client_write_accepted 记录；其他既有预算场景仍通过。此项是开发计划中的新证据能力，不把尚未实现的设计当作新 Bug。客户端真正读取无法从服务器写成功得知，设计已明确网关写入接受的证据边界。

阶段证据实现与验证：每次尝试最多保存首次上游响应/错误、写出可能和正字节写入接受，关联原实际价格凭证；SDK 不可观察的 HTTP 状态为空。Gin 写入包装保留原接口，保活不建输出记录；WebSocket 只记录当前响应自己的数据，不把上游输入和无关 stream 归入账单。已交出异步任务后旧执行者不写证据；结算后延迟终局只保留预先的 possible，accepted 未知。元数据写入失败停止并核查，内存防线也阻止再提交和金融意图，故障不会伪装成免费。

原三条失败阶段用例 relay-phases-red 到 green 通过。新增六条边界用例覆盖正常/保活、零字节失败、3 字节部分写入及三个证据存储故障；relay-phases-faults 的 SQLite controller 全表与原 WS 通过。扩大 race 含 controller/relay/OpenAI/helper 扫描器全部退出 0（service 筛选无测试，不作为该包覆盖）。首个 boundaries 因新包装在 Midjourney 租约交接后写入失败，修复并保留原代次防线后通过。Python 第一次 gate-red 退出 0，没有验证新增遗漏；补全遗漏断言后 gate-behavior-red 六项实际失败、恢复完整门禁后 6 个自测通过。

严格三数据库及 Redis、完整根模块/relaykit、全范围 vet 实际退出 0，证据 /tmp/new-api-round09-relay-phases-final-{matrix,full,vet}.log；race 证据 /tmp/new-api-round09-relay-phases-race.log。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，新增六分支各库零跳过。没有新增表结构；Observation 是 omitempty 可选 JSON，旧凭证指纹保持。完整第 9 轮仍需媒体逐项估算、其余来源边界审查及最终 review/迁移验证后提交。

### 开发阶段：输入媒体逐项估算（阶段验证完成）

原生图片估算保留实际尺寸、低细节/分块/分片计数和倍率，固定图像/音频/视频/文件默认值分别记录；上传音频保存每个文件的原时长及独立秒数/token 取整，合计不改变原规则。细节最多 64 项，更多项仍计入总量并明确遗漏数；不保存标识、地址、文件名或内容。跨文件和图片合计使用既有饱和转换，不能整数累加成负费用。

media-estimator-red 实际复现混合媒体与 patch 图片缺少逐项描述；green 通过，增强模型有界验证及两个 0.1 秒 WAV 独立计数 17+17=34、65 项合计和 64 项细节边界后的 reviewed 通过。原 model 估算凭证表新增非法种类/顺序/数值/过多项拒绝、不可变重放及原记录读取，测试未新增文件。

make test-database、make test、controller/model/service 与原生三协议定向 race、全范围 vet 全部退出 0，证据 /tmp/new-api-round09-media-estimator-final-{matrix,full,race,vet}.log；SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11 严格零跳过。新增可选 JSON components/omitted_components，没有新表结构，旧凭证 JSON 保持。第 9 轮还需来源完整性与最终 review/迁移验证，尚未提交。

### 开发阶段：其他适配器计量来源（阶段验证完成）

adaptor-facts-red 在既有重试价格/修正场景复现缓存、音频、图片与思考等 DTO 数量未进入最终凭证。通用结算现在保留全部计量分类，明确标为 adaptor/new-api-adaptor-normalized-v1；未知标量零不声称已核实，存在的缓存 modality 指针零单独保存，原 upstream/estimate 事实优先。参考费用仍 56、后续修正 40，渠道/分组、Key/包均保持原行为。

green 及 adaptor-facts-final-matrix/full/race/vet 全部退出 0。真实三库 3.50.4/8.4.11/15.19、Redis 7.4.11，严格零跳过；扩展竞态覆盖 controller/model/service/relay/helper 和原生三协议。没有新结构。后续审查发现阶段错误仍允许缓冲输出，relay-fault-stop-red 三个故障分支实际失败；修复后 relay-fault-stop-green 的相关五包与 controller 全表通过，正在补真实 HTTP 和整体验证，尚未关闭本缺陷或第 9 轮。

### 最终审查：阶段写入故障与持久化兼容

relay-fault-stop-red 三个写入故障实际失败，最初直接边界修复通过后，真实 HTTP 的 relay-fault-http 又发现缓冲错误重分类及 SSE 分隔损坏。修复后凭证错误停止生成，普通客户端断开仍按原 New API 计费；错误渲染只发一个终局、帧均可解析。中间 typed-nil 回归的失败保留，显式 nil 修复后通过，不把文件名为 green 的失败当作证据。B77、B78 已归档。

最新 relay-fault-final-matrix/full/race/vet 全部实际退出 0，严格真实三库/Redis 零跳过，扩展 race 包括 model/controller/service、原生三协议、转换、音频、图像、Realtime、WS、扫描器及任务相关行为。新建及实际发布版 v1.0.0-rc.41 代表库升级、每库两次 InitDB/InitLogDB 均退出 0；/tmp/new-api-round09-relay-metadata-migration/{upgrade-result,fresh-result}.log 及各库日志保存结果。新版凭证同时包含真实输入图片 components、实际尝试价格及三个 relay 阶段，第二次启动后的不可变重放、原计数/分词器依据及恢复结算通过。原用户/Key/任务/订单/价格及唯一约束保留；没有历史商业用户迁移需求。

### 最终审查：图片流预算

image-budget-red 实际复现数量或 token 超额仍发送载荷和 DONE、存储故障保存资金意图。每个真实 SSE 事件先采原量、按已有图片规则更新观察数量/token 费用，再检查冻结价格与原资金来源。未完成流不得降低请求数量；超过请求的实际载荷可提高参考数量。不足不转发当前载荷及正常完成、封顶结算；未知故障保留核查。JSON 转 SSE 在第一张转发前按完整原量检查；图片 base64 不用于 token 估算。预算投影复制私有倍率映射，不能改写原价格。

第一次 image-budget-green 是接口参数编译错误；reviewed-green 实际复现提前看到 DONE 的终局错误，增加独立预算停止防线后 done-green 通过。旧按次价格和两条持续上游取消的增强后 close-green 通过。既有 controller 文件共七个场景，gate-red 七个遗漏实际失败，补齐后 Python 6 个门禁自测通过。正在执行 image-budget-final-matrix/full/race/vet，整轮保持未提交。

图片预算首批最终验证：严格三库/Redis、make test、vet 退出 0，image-budget-final-matrix/full/vet。image-budget-final-race **退出 1**，复现 B80：阶段故障工作器调用正常停止并恢复 c.Request，与主扫描器读取竞争。修复后故障中止仅取消上下文、正常收尾仍保留原行为，同一个 once 保护后续收尾；正在跑 relay-abort-race-green。首批失败竞态不是通过证据，整轮仍未提交。

relay-abort-race-green 与 reviewed-race-green 都退出 1：已不再出现数据竞争，但立即取消使 flush 先返回通用 context canceled，掩盖原凭证故障；不是通过证据。生成入口和 flush 先返回凭证故障，之后才检查普通取消；relay-abort-cause-race-green 的 controller 全表、service 计数/图像、helper 扫描器及 OpenAI 图像竞态实际退出 0。正在执行整轮 reviewed-final-matrix/full/race/vet，尚未提交。

## 第 9 轮最终交付

字段来源、缺失/明确零、部分/完整和累计/增量证据已接入全部既有新账本结算入口。原始协议量、适配器归一化、插件提取及本地估算分别留痕；价格/算法版本、音频/图片媒体参数、请求阶段均有可追溯依据。已实现预算停止、平台承担合法实收之外的差额、不可变原账单修正、受控核查确认及持久恢复。无采购成本或可查询用量的供应商保持未知，不虚构精确采购费用或自动补查。

最后一轮审查修复 B79 图片预算及 B80 故障中止竞争，原 New API 收费语义、正常终局和普通断开保持，未知存储错误不写资金意图。所有已确认缺陷 B65–B80 已归档；其他缺陷及其修复证据亦在原归档文件。没有前端改动，未部署或推送。

| 交付验证 | 实际结果与证据 |
| --- | --- |
| `make test-database` | reviewed-final-matrix 退出 0；SQLite 3.50.4 / MySQL 8.4.11 / PostgreSQL 15.19 / Redis 7.4.11，所有必需契约零跳过 |
| `make test` | reviewed-final-full 退出 0；根模块与 GOWORK=off 独立 relaykit 完整回归 |
| 扩展 `go test -race` | reviewed-final-race 退出 0；model/controller/service、middleware、表达式、任务、scanner、原生/转换协议及音频/实时预算与阶段故障 |
| `go vet ./logger ./model ./controller ./service ./middleware ./pkg/billingexpr ./setting/operation_setting ./relay/... ./router` | reviewed-final-vet 退出 0 |
| fresh / 实际 v1.0.0-rc.41 升级，主库和独立日志库各初始化两次 | relay-metadata-migration 的 upgrade-result / fresh-result 均退出 0；三库原数据、唯一约束、可选 JSON、不可变重放及已知计量恢复保持 |
| `python3 bin/test_database_matrix_test.py`、`git diff --check` | 6 个门禁自测通过，新增七个图像契约漏跑均会拒绝；差异检查通过 |

完整日志在 /tmp/new-api-round09-reviewed-final-*.log，迁移辅助程序和结果在 /tmp/new-api-round09-relay-metadata-migration/。普通 make test 的可选外部环境跳过不替代严格矩阵证明。最后的中止修复与图片监测未改变数据库结构/序列化合同，先前最终 JSON 迁移证据继续有效；其事务行为由最新三库验证。

第 10 轮边界：账号稳定标识、负载均衡/严格固定/会话优先、跨用户和多 Key 会话隔离、凭证轮换、429/故障策略，以及输出后禁止整次重放、提交结果未知的实际控制。复用第 9 轮价格与阶段凭证；新增路由行为先写失败测试，再 review/修复、三库与必要回归、提交后进入第 11 轮。UI/多语言/真实浏览器仍属于第 11 轮；ClickHouse、完整多实例/故障/容量证明属于第 12 轮。

第 9 轮最终验证的完整命令（各项实际退出 0）：

```sh
make test-database
make test
go test -race ./model ./controller ./service ./pkg/billingexpr ./middleware ./relay ./relay/helper ./relay/channel/task/jsplugin ./setting/operation_setting ./relay/channel/openai ./relay/channel/claude ./relay/channel/gemini ./relay/channel/advancedcustom -run '^(TestCreditPackDatabaseMatrix|TestCreditBillingDatabaseMatrix|Test.*ResponsesWS.*|TestResponsesWebSocket.*|TestResponsesInterruptedStreamHealth|TestStreamScanner.*|TestCreditTokenEstimatorProvenance|TestCreditTextQuota.*|TestCalculateText.*|TestTool.*|TestGetTool.*|TestCreditOpenai.*|TestCreditResponses.*|TestCreditClaude.*|TestCreditGemini.*|TestCreditTranscription.*|TestCreditSpeech.*|TestCreditImage.*|Test.*Image.*|TestSecurityAccountDeletion.*|Test.*Task.*|Test.*Usage.*|Test.*AccessToken.*|TestOai.*|Test.*AdvancedCustom.*|Test.*ResponseModel.*|Test.*Realtime.*)$' -count=1
go vet ./logger ./model ./controller ./service ./middleware ./pkg/billingexpr ./setting/operation_setting ./relay/... ./router
```


## 第 10 轮：账号路由与重试

实现范围：保持 New API 的渠道优先级、权重、随机/轮询与会话规则，在主库加入稳定账号和用户隔离的会话绑定。严格模式在网络发送前原子认领账号，重试选择仍保留账号；prefer/off 按原策略选择。凭据轮换保持账号 ID 和递增版本，普通替换及删除退役旧身份，重排保留禁用原因及时间。健康状态写入与选择、轮换、删除通过同一渠道行锁协调。

异步任务提交时持久化账号/版本，查询、下载与继续任务使用该账号当前凭据；批量查询按账号分组，不推进轮询游标。每次尝试的不可变价格凭证及管理路由事件记录实际账号和版本。明确 429 且未输出时可以按配置换账号重试，一个逻辑账单只结算一次；提交结果未知、开始输出、取消、核查、已结算或已释放会话禁止重新生成。平台承担规则保持，不产生用户欠款或充值追扣。

设计依据见产品设计 §4.3、技术设计 §9.1、开发计划 §4.17。新增功能缺口与 B81–B87 的已复现缺陷分开记录。权限验证覆盖普通用户、最新数据库降权、只读及过期 PAT、版本冲突与秘密不泄漏；采用 OWASP ASVS 5.0.0 V8 适用访问控制要求，不声称全项目合规。

### TDD 与 review

- 初始失败复现严格请求 A→B、同名会话串用户、删除再添加复活身份及任务缺少提交账号，日志 `/tmp/new-api-round10-affinity-red.log`、retirement-red、channel-retirement-red、task-identity-red。
- B81 输出/终局会话重试、B82 重排丢失健康、B83 私有字段 Value 丢弃只有账号的任务、B84 删除渠道误判供应商失败、B85 条件删除误删重新启用渠道，均先复现后修复；对应原始日志在 Bug 归档。
- 完整回归揭示新增分发器检查误阻止无需选渠道的任务 GET（B86）；原路由行为测试实际返回 500，修复后保持认证及所有权要求。B87 严格绑定的第二次选择仍轮询，strict-retry-red 实际 A→B，修复后不变。
- 最初 final-full / final-race 的 WebSocket 测试夹具缺少账号表，请求已被拒绝而测试无限等候；SIGQUIT 获取栈后两次进程非零退出，不能当作通过。补齐表并给实际目标接收设有界超时，不降低请求结果断言。
- reviewed-final-matrix 因 PostgreSQL 夹具显式 ID 与自动生成 ID 冲突退出非零；reviewed-final-race 捕获原 Kling 夹具的异步性能统计读取 Redis 配置与 cleanup 写入竞争。统一固定夹具 ID、关闭该用例无关的异步性能统计，postgres-fixture-reviewed-green 与 native-fixture-reviewed-race-green 实际退出 0。首次关闭统计的错误赋值只造成编译失败，不属于行为证据。

### 最终验证

修复后的最终完整验证全部实际退出 0，日志 `/tmp/new-api-round10-final-approved-{matrix,full,race,vet}.log`：

| 命令 | 结果 |
| --- | --- |
| `make test-database` | SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11；必需账号、HTTP 重试及已有账务契约零跳过 |
| `make test` | 根模块与独立 relaykit 全量回归通过；普通测试的可选外部环境跳过不作为矩阵证据 |
| 下方扩展 `go test -race` | 所有选定用例通过，无数据竞争报告 |
| `go vet ./logger ./model ./controller ./service ./middleware ./pkg/billingexpr ./setting/operation_setting ./relay/... ./router` | 通过 |
| `python3 bin/test_database_matrix_test.py`、`git diff --check` | 6 个严格门禁自测及差异检查通过 |

```sh
go test -race ./model ./controller ./service ./middleware ./relay ./relay/helper ./relay/channel/task/jsplugin ./setting/operation_setting -run '^(TestAccountAffinityDatabaseMatrix|TestCreditPackDatabaseMatrix|TestCreditBillingDatabaseMatrix|Test.*Channel.*|Test.*Affinity.*|Test.*Polling.*|Test.*Retry.*|Test.*ResponsesWS.*|TestResponsesWebSocket.*|TestResponsesInterruptedStreamHealth|TestResolveOriginTask.*|TestRespondTaskSubmissionErrorWithoutCause|TestKlingNativeRouteSubmitPollSettleAndQuery)$' -count=1 -timeout 5m
```

前一组 reviewed-final-matrix/race 非零退出保留为失败证据；只有上述最后结果用于完成判断。B81–B87 均已归档。

新建及实际 v1.0.0-rc.41 开发库升级后，主库及独立日志库各执行两次 InitDB/InitLogDB，核对原数据、索引、唯一约束、历史可选 JSON、防重、恢复及新增稳定绑定。辅助程序已以最后的生产代码重新构建，`/tmp/new-api-round10-account-migration/reviewed-final-upgrade.log`、reviewed-final-fresh.log 均实际退出 0，真实 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19。账号轮换后重启保持 ID/当前版本 2，任务提交版本仍为 1；绑定隔离、唯一性、凭据摘要隐藏及陈旧版本拒绝均通过。

第 11 轮边界：管理与用户页面、积分包有效期和分量、订阅多窗口、账单参考价/实收与证据、自定义权益标签失效、账号轮换入口、多语言及真实浏览器链路。第 12 轮仍负责 ClickHouse 非事务日志、完整多实例/中断恢复与容量证明及 A01–A24 / I01–I14 收口。本轮不部署或推送。


## 第 11 轮：管理与用户页面（已验收）

已接入积分包与账单的有界查询、套餐窗口和标签表单、发布版本、用户窗口/权益与账单证据、管理员发放/核查/修正/恢复以及账号轮换入口。现有钱包改读新订阅结构，版本购买携带购买意图及版本，使用订阅用途的包余额；金额保留六位小数。前端复用 Dialog、ConfirmDialog、StaticDataTable、业务状态组件及既有表单/金额格式器。

TDD 实际缺口：wallet-api-red 的 404；subscription-ui-reviewed-red 的三个行为失败；bill-evidence-red 的数量依据缺失；catalog-methods-red 的公开支付方式缺失；purchase-funds-price-red 的合法购买被旧余额阻止及微价格显示为零。前两组初期 green 亦有失败：API 未声明 PAT 只读范围、UI 夹具缺 QueryClient；这些不是通过证据。

review 实际复现凭据在 Mutation 变量和 Axios 错误配置中残留；credential-cache-red 失败，改为本地读取凭据及安全错误投影，green 实际退出 0。管理员查看已禁用用户积分误被拒绝，disabled-admin-read-red 复现；修复后 green 的 SQLite 管理契约退出 0。设计规则见开发计划第 11 轮契约；缺陷将按最终回归证据单独归档。

最终交付包括公开版本目录与购买订单、合法用途余额、限时积分和明细、原始/当前账单数量证据、服务端时钟下的窗口与标签、管理员发放/来源策略/核查/账单修正/恢复/取消及账号管理。用户列表只读投影账务模式，不开放写入该字段。Stripe 版本套餐的内联价格不依赖旧充值 SKU；支付配置与套餐合同可用方式分别验证。响应不明保留稳定意图，已付款订单能通过所属事件恢复购买结果。

181 条新增或补齐文案在 en、zh、zh-TW、fr、ja、ru、vi 中均完整，插值字段一致。七语言切换、八种界面语言代码和最小积分金额均有行为测试。取消后的历史只保留原用量，不宣称仍有可用额度，也不把原期限误标为取消时刻。已复现缺陷及失败到通过证据分别归档为 B88–B98，设计保持确定规则。

| 最终验证 | 实际结果和证据 |
| --- | --- |
| `make test-database` | `/tmp/new-api-round11-approved-matrix.log` 退出 0；SQLite 3.50.4 / MySQL 8.4.11 / PostgreSQL 15.19 / Redis 7.4.11，必需契约零跳过 |
| `make test` | approved-go-full 退出 0；根模块与独立 relaykit 全量 |
| `go test -race ./model ./controller ./middleware -run '^(TestCreditPackDatabaseMatrix|TestCreditBillingDatabaseMatrix|TestChannel.*|TestAccessToken.*)$/^sqlite$' -count=1` | approved-race 退出 0；三数据库行为另由严格矩阵验证 |
| `go vet ./controller ./model ./middleware ./router`、`go build -o /tmp/new-api-round11-browser-api .` | approved-vet / approved-build 均退出 0 |
| `cd web && bun run test` | delivery-frontend 退出 0：176 文件、2196 个测试 |
| `cd web && bun run build:check` | delivery-build-reviewed 退出 0，包含 TypeScript 检查和生产构建；第一次因新增测试漏传必需 topupInfo 退出 1，修正夹具后通过 |
| 本轮 TS/TSX 的 oxlint、保护头格式检查、七语言插值/完整性检查 | delivery-lint 和最终范围检查退出 0；47 个本轮 TS/TSX 文件保护头保持，54 个文件无格式差异 |
| 全仓 `bun run copyright:check` | 退出 1：23 个既有文件的头不符合脚本要求；逐个核对与 HEAD 完全相同，均不在本轮修改范围，没有把此项声称为全仓通过 |
| 实际浏览器 | 本地余额购买、模拟上游真实 HTTP 消费、首次使用窗口、账单证据、管理员取消、发布版本 2 后旧订单保持版本 1、稳定账号列表、390px 手机宽度通过；页面宽度 390px，无全页横向溢出 |

Go 日志前缀 `/tmp/new-api-round11-approved-`，最终前端日志前缀 `/tmp/new-api-round11-delivery-`。小额/币种、凭据缓存、取消、服务端时间、发布冲突及不明响应均有先失败后通过的独立原始日志。没有本轮数据库结构变更，前一轮 fresh/实际 rc.41 两次初始化证据继续适用，最后一轮将重新完成整体启动演练。

浏览器使用隔离的 SQLite 开发库和模拟上游，截图在 `/tmp/new-api-round11-browser/`：wallet-consumed.png、admin-cancelled.png、plan-published.png、channel-accounts.png、overview-credit-balance.png、wallet-cancelled-mobile.png、wallet-cancelled-desktop.png。没有真实现金支付或实际凭据轮换。原本机磁盘占用超过默认 95% 阈值，仅测试库把阈值设为 100% 以验证转发；生产默认没有改变。管理员发包由真实三库 API 与 RTL 确认，浏览器的原生日期控件自动填值未成功提交，不列为浏览器成功发包。

第 12 轮边界：整体现有契约逐项核对、真实进程中断与多实例接管、真实三库新建/rc.41 升级和重复启动、非事务 ClickHouse 保留可见待办的契约、明确开发负载下的容量/恢复验证。生产容量承诺、域名、证书、真实供应商支付凭据和上线审批不作为开发门禁；不部署或推送。
