# 开发基线已修复问题

归档日期：2026-10-07。均已修复并完成相关回归，不是待实现的产品能力。

## B01：兑换码矩阵测试要求共享库为空

现象：真实三库矩阵组合执行时，兑换码测试断言库没有用户表，但其他用例已建表，造成假失败。

复现：第 1 轮最初执行 `make test-database`，MySQL/PostgreSQL 分支失败。修复：复用已有独立测试库机制，保持删除行为及原断言。证据：修复后专用矩阵零跳过通过，`go test ./controller -count=1` 完整通过；提交 `d7c2e4412`。

## B02：已过期未清理余额占住新发放上限

现象：开发中的积分包模块将全部未分配量计入钱包上限，导致一个已到期的大包阻止新发包。

复现：发放 `MaxWalletQuota`，令其到期但不运行清理，再发 1 分；原实现错误返回额度上限超出。

修复：上限只累计未到期未分配量与全部预占量；保留历史，不删除仍待结算的预占。证据：`TestCreditPackDatabaseMatrix/*/expired_money_does_not_block_new_grant` 三库通过，模型完整回归、race 检查通过；提交 `b1bd757f2`。

## B03：订阅偏好被静默改成积分付款

现象：第 3 轮初版新模式计费工厂未检查用户“只用订阅”的选择，实际消耗了积分包。

复现及修复：真实 HTTP 用例 `TestCreditBillingDatabaseMatrix/*/subscription_only_never_silently_spends_packs` 先看到上游被调用和 Key 再次扣款；工厂改为订阅接入前明确拒绝，保持积分及 Key 不变。三库专用矩阵和全量回归通过。

## B04：欠额偿付的内部重放缺少持久防重

现象：同一次发放已用于偿付后，后来有其他积分到账，再次调用内部偿付入口可能重复处理，或因预占输入变更返回冲突。

复现及修复：`TestCreditPackDatabaseMatrix/*/unpaid_bill_and_topup_repayment` 在首次偿付后新增独立资金并重放原事件，原实现失败；增加与偿付同事务提交的唯一事件，每次发放只处理一次。三库矩阵、race 及全量回归通过。

## B05：新增 HTTP 测试泄漏全局分组配置

现象：单独跑新增用例通过，完整 controller 回归中的自动分组列表缺少 vip 模型。原因是新测试修改分组倍率后未恢复。

修复：测试保存并恢复自己修改的全局分组和信任额度配置；保持原列表断言不变。`make test` 全量通过。

## B06：通用用户编辑可覆盖账户模式

第 4 轮复现：持有旧 User 对象时，主库模式已改变，`Update(false)` 把主库模式写回旧值。修复：模式与资金字段一样排除出通用编辑；`credit_account_rejects_legacy_wallet_and_key_writers` 的具体断言验证主库模式不被覆盖，旧余额不成为旁路。

## B07：新增账务管理路由漏登记访问令牌权限

全量回归检测六个方法缺少权限声明，合法管理员 PAT 无法访问。修复：明确 `option:read/write`、`billing:read/write`，保持默认拒绝；路由完整性及真实 AdminAuth/RootAuth 请求覆盖缺权限、读写分离、过期、撤销与角色降级。

## B08：兑换管理接口丢失积分期限与用途

API 行为测试先观察到创建的 123 秒/用途 3、修改后的 456 秒/用途 1 均变成 0。修复：创建透传受校验的配置，更新用可选字段保留省略值，模型更新明确保存配置；不能只靠底层直接插入测试。

## B09：已兑换的代码仍可重新启用或改写

发放后把代码改回可用并改变额度，原更新返回成功。修复：更新只允许修改未使用代码，使用状态由兑换事务单独推进，旧已用来源更新返回冲突；防止通过编辑重发或改变已记账事实。

## B10：SQLite 支付回调读后写竞争

五种支付的两条独立连接同步开始，原回调均复现 SQLITE_BUSY/BUSY_SNAPSHOT。修复：SQLite 在读取订单状态前用不改变业务值的写入取得事务；其他数据库使用行锁。同时回调、重复回调及注入 Ledger 失败均验证一次发放和整笔回滚，不依赖进程锁。

## B11：签到奖励范围缺少安全校验

倒置或超限范围可能发放异常奖励，零奖励在新模式会被当作非法发包。修复：随机计算前校验非负、有序、有界范围；零奖励只提交签到记录。失败用例保留原签到/资金原子性要求。

## B12：积分策略接口依赖旧缓存角色

以普通用户主库身份、伪造/旧 Root 上下文调用策略更新，初版返回 200。修复：策略读写再次查询主库当前 Root 及启用状态；财务操作也校验当前角色和目标资源归属。真实接口权限及版本冲突回归通过。

## B13：现金核查证据覆盖与迟到重放冲突

初版案件只保留最新证据，确认后迟到的原未知结果重放返回冲突。修复：每份不同结果追加主库证据，案件只保存当前状态；相同内容重放查历史记录返回，不能覆盖确认或重复追加；改变终局仍拒绝。核查/资金数量互不混写。

上述第 4 轮修复经 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19 专用矩阵零跳过及根模块/relaykit 全量回归；SQLite race 检查通过。完整命令与边界见开发进度，不以这些修复代表后续恢复器、迁移切换或自动现金退款已完成。

## B14：MySQL 合并大小写不同任务编号

第 5 轮提交前新增 `CaseSensitive` / `casesensitive` 两个真实任务及结算，SQLite/PostgreSQL 通过、MySQL 返回关联冲突。原因是把不透明上游 ID 按数据库默认排序规则比较。修复：保存精确字符串摘要并关联任务主键，结算锁定主键行，再按 Go 精确字符串及用户/账单归属核对；金额回写也使用主键，避免误改多行。严格三库矩阵通过。

## B15：旧 Midjourney 未写入协议类型

新账务真实入口测试发现生成 RelayInfo 时没有写 RelayFormat，工厂在发送前拒绝合法请求。修复任务/Midjourney 生成器保存协议，保留工厂拒绝未接入路径的约束；完整 Midjourney 提交、完成及重复完成用例通过。

## B16：实时用量并发修改和表达式缺少音频明细

审查发现旧实时路径两条读协程共同修改用量，结束时没有等待两者停止；预结算和最终表达式只有输入/输出总量，独立音频价未正确参与。修复为读协程只传帧、一个循环维护状态和费用，结束关闭连接并等待退出；累计用量通过现有归一化转换，完成事件按稳定 ID 去重。真实 WebSocket 的两次完成和一次重复回执得到预期 48 分，SQLite race 通过。

第 5 轮修复及资金回滚行为经严格三库矩阵、根模块/relaykit 全量回归和 SQLite 模型/控制器 race 验证；实际命令与未完成边界见开发进度。

## B17：Midjourney 即时完成重复统计

第 6 轮真实提交回归：第二笔任务直接返回 SUCCESS，包及 Key 累计扣 60 分正确，但用户已用变成 90、请求次数变成 3（应为 60、2）。新结算统计与旧提交统计同时执行。修复：新账务任务不再进入旧日志/统计分支，即时终结停止续租；保留原数量断言，三库及完整回归通过。

## B18：租约检查和续租使用等待前的时间

事务等待可能使已过期执行者在新事务继续写；续租也可能从旧时间计算，缩短续租期限。修复：持有账户锁之后读取可控服务器时钟，结算意图与资金事务分别校验，续租按锁内时间计算。目标回归在意图保存后推进时钟，旧资金事务确实拒绝；新代次只完成一次。续租失败回归曾观察到 122（应为 125），修复后通过。

## B19：共库日志启动漏建防重回执

独立日志测试显式建表，未覆盖不配置 LOG_SQL_DSN 的默认共库启动。新增真实 `InitLogDB → 结算 → 投递` 用例复现“credit_log_deliveries 表不存在”。修复：共库主节点也调用完整日志迁移；共库与独立 SQL 日志、重复启动和最新 release 升级均验证通过。

## B20：核账守恒差异只显示部分合计

包原有可用 80、预占 20，注入已用 1 后，初版报告 expected=100/actual=100。原因是计算遇到不守恒分量便停止。修复：安全范围内计算全部五个分量，报告实际 101；越界分量单独列出范围差异，避免合计溢出。账本保持原样，不把核账变成自动修复。

第 6 轮上述修复经真实三库/Redis 严格矩阵、根模块/relaykit 全量回归与 SQLite race 验证；具体证据和尚未验收范围见开发进度。

## B21：不同套餐争用同一发布事件返回存储错误

第 7 轮 MySQL 并发回归让两个不同套餐在插入前到达同步点，初版有一条成功、另一条返回 1062。修复：全局事件唯一约束仲裁，冲突插入后用锁定读核实实际赢家和输入摘要；同事件不同输入返回明确版本冲突，不靠不同数据库的 RowsAffected 判断所有权。三库严格矩阵通过。

## B22：版本管理接口未约束分页与省略版本号

新增 API 回归复现负页码、page_size=-1 和偏移乘法溢出均返回 200，省略上一版本号返回 409 而非输入错误。修复：新版本接口检查正页码/页大小和安全偏移，发布 DTO 用指针区分未提供与显式 0；非法输入返回 400，合法版本冲突仍为 409。本修复覆盖新接口，旧通用分页入口仍在后续整体核查范围。

## B23：支付审计泄露签名、正文和客户信息

真实回调回归复现 Creem、Stripe、Waffo 与 Waffo Pancake 在正常/失败路径写出签名、完整支付正文或 URL 查询串；Creem 已验签的充值通知另打印客户邮箱及姓名。签名可被重放，支付链接也可能携带令牌。修复：记录处理阶段、已解析订单/事件编号、必要请求路径及字节数；去掉正文、签名、查询串、客户姓名/邮箱及支付链接；SDK 验签/正文解析失败记录阶段，不打印可能嵌入正文的错误。Creem 编解码改用项目 JSON 封装。

回归覆盖有效 HMAC/RSA/Stripe 签名的忽略事件、缺失/错误签名、格式错误、Pancake 非法环境、已验证但本地不存在的支付订单及旧测试模式分支。测试捕获真实审计输出，确认拒绝状态和必要审计仍保留、敏感值不输出。真实 RSA 测试密钥仅在测试内生成，无外部支付。安全依据见开发计划 §4.10，结果见开发进度。

## B24：过期订单重放丢失原合同

第 7 轮订单测试复现：下单成功后丢失响应，在订单期限结束后以原事件重试被前置时间校验拒绝。修复：先查原防重记录；输入一致返回原合同及原期限，输入改变仍冲突；只有创建新订单才校验期限晚于现在。重复不能续期或发放额外权益。三库回归通过。

## B25：矛盾支付通知只返回冲突，未保留核查依据

同一渠道通知编号带来不同币种或被指向另一订单时，初版直接返回冲突，当前订单没有核查标记，也没有关联原回执的矛盾记录。实际回归观察到 needs_review=false、原因为空。修复：追加一份按通知与内容摘要防重的观察，关联原事实，与当前订单核查标记同事务保存；保存后再返回冲突。原回执及其付款时间不改，重放矛盾不重复追加，正常通知不清除核查状态。

三库并发测试让两个不同订单在争用同一全局通知前到达同步点，验证一份原事实、一份矛盾观察、一份实际交易归属；不能仅依靠先查后写。SQLite 按单写者机制验证同样结果。全量、race 与最新发布版升级及重复启动通过，证据见开发进度。商品开通与退费没有在此偷偷执行。

第 7 轮最终验收及后续窗口边界见开发进度。

业务规则维护在[产品设计](../../design/README.md)，实际轮次证据见[开发进度](../../plans/development-progress.md)。本文只记录缺陷与修复。


## B26：订阅分组查询沿用全局数据库列引号

版本化取消在传入 PostgreSQL 事务时，旧分组查询仍使用由全局数据库类型生成的列片段，可能选择错误引号。改为 GORM `clause.Select` 描述 `group` 列，由实际连接选择引号。真实三库取消/分组保留回归通过，见最新目录矩阵证据。

## B27：取消已结束的旧权益污染后来新订单

新增行为测试复现：旧连续套餐已全部取消，用户另建新购买订单；以新管理事件再次取消旧记录仍成功，并把新订单标为需要核查。修复：持有账户锁后确认所选连续权益链仍有未结束的 active/scheduled 周期，没有则拒绝；原取消事件仍可防重返回。红绿日志 `/tmp/new-api-round07-history-cancel-{red,green}.log`，三库验证通过。

## B28：公开套餐目录缺少可购买版本并暴露草稿

版本管理和余额购买已实现后，公开目录仍返回可变计划，用户拿不到 `version_id`；未发布改价也直接展示。行为测试观察到版本为空。修复：新模式按当前上架状态选择每个计划的最新已发布合同；价格和标签来自该版本，隐藏渠道 SKU，未发布计划不展示。红绿日志 `/tmp/new-api-round07-catalog-{red,green}.log`，三库严格矩阵通过。


## B29：未发起收款的商品查询失败被当成支付结果不明

Creem 适配初版先写 started，再查询商品；查询失败就标核查，原事件重试永远被拒。行为测试实际观察到 needs_review=true、第二次查询恢复后仍没有创建。修复：只读商品查询先于发起状态，查询失败尚未发送创建请求，保持可重试；持久化 started 仍在真正的现金创建之前。红绿证据 `/tmp/new-api-round07-creem-lookup-red.log` 与 `/tmp/new-api-round07-cash-final-focused.log`，三库完整矩阵通过。

## B30：Creem 锁价合同允许渠道在价外加税

初版只检查币种、单次类型及商品状态；回归使用 exclusive 税费商品仍收到 200，创建次数从 3 变 4。修复：当前版本化路径仅允许 inclusive 商品，价外税费配置在创建前返回冲突；不能在用户付款后修改锁定合同来适应不同金额。红绿证据 `/tmp/new-api-round07-creem-tax-red.log` 与 `/tmp/new-api-round07-cash-final-focused.log`，三库完整矩阵通过。


## B31：Pancake 标价被误作实收款

已验签且 total 与套餐价格相同时，初版直接核准支付。新增实收缺失、实收 0、实收不同于标价的测试实际观察到错误 verified 和开通权益。当前官方协议区分 chargedAmount 与已弃用标价字段；修复为只取实收，NULL 与 0 区分，不回退到标价或 subtotal+tax。日期式 paymentDate 不补造准确时间，保持核查。真实 RSA 红绿日志 `/tmp/new-api-round07-pancake-actual-charge-{red,green}.log`，Pancake 当前三库回归通过；最终快照三库、全量和升级验证均通过，见开发进度。

## B32：旧 Pancake SDK 的对称短窗口误拒延迟重试

真实 RSA 回归观察到 20 分钟前的签名被拒，而未来 2 分钟的签名被接受。当前供应商说明重试复用首次签名；宿主保留 SDK 密码学校验，显式按过去 45 分钟、未来 1 分钟约束，数据库事件/交易防重继续生效。45 分钟外和错误签名仍拒绝；没有关闭时间检查。红绿与三库证据同 B31。

## B33：后到事实可以覆盖“已取消”的显示原因

核查原因字段用于展示新收到的缺失或冲突，不能同时承担永久取消边界。review 增加独立 rights_cancelled_at；取消保存边界，后到矛盾回执、人工核查、购买重放均不能恢复取消权益。测试明确构造“取消 → 后到金额错误回执 → 管理员核查”的顺序，保持原事实并拒绝重新开通；最终快照三库、全量和升级验证均通过，见开发进度。

## B34：管理草稿把人民币合同强制改成美元

管理 API 实际接受 CNY 后仍返回并保存 USD，使新 Epay 人民币合同无法正常发布。红测试复现创建、修改两处币种被覆盖；修复为保留规范化的请求币种，空值才默认 USD，发布与草稿复用同一校验。证据 `/tmp/new-api-round07-review-api-currency-{red,green}.log`，最终矩阵结果见开发进度。

## B35：旧订阅入口绕过新账本购买与窗口

实际红测试复现新模式用户通过旧套餐创建入口获得旧权益，并通过旧增减接口直接改动已购买版本的用量。修复：旧创建和预扣拒绝新模式账户，旧增减/重置/删除拒绝版本化权益，保留旧模式契约；新订阅消费由第 8 轮窗口入口负责。红绿证据 `/tmp/new-api-round07-legacy-subscription-bypass-{red,green}.log`。

## B36：买家令牌持久化、失效缓存与支付链接故障泄露

实际红测试分别复现购买响应把 JWT 写入数据库、ready 重试在认证失败时仍返回旧令牌、DEBUG SQL 写入失败输出可用付款链接；随后本地 SDK 网关复现 Auth 固定键取得过期缓存令牌、过期令牌仍返回 200，以及网络认证期间账户被停用却仍返回令牌。修复：只持久化现金会话，不存 JWT；重试独立获取认证，保持一笔现金创建；仅移除 Auth 端点的缓存键，验证令牌期限并在网络返回后复查资格。敏感响应写入禁用该语句的 SQL 内容追踪，审计仍保存失败阶段。红绿日志 `/tmp/new-api-round07-checkout-token-storage-{red,green}.log`、`/tmp/new-api-round07-checkout-sql-privacy-{red,green}.log`、`/tmp/new-api-round07-buyer-token-cache-{red,green}.log`。


## B37：免费与零估算绕过当前使用资格

窗口接入初版付费零估算允许已耗尽 Key；免费分支没有校验当前 Key 禁用和过期。回归先观察失败，再将主库 Key 资格与额度检查分开；免费可以不占金额，但不能免除使用资格。新请求也检查当前用户启用状态，旧合法预占结算不因后来禁用而消失。红绿日志为 `/tmp/new-api-round08-window-lifecycle-{red,green}.log`、`/tmp/new-api-round08-rule-key-{red,green}.log`、`/tmp/new-api-round08-current-status-red.log`；完整验收见第 8 轮进度。

## B38：到期分组已提交但缓存仍授予旧权限

实际回归构造数据库已降回 default、Redis 仍是 pro，初版 relay 读出 pro。修复：新账务账户推进购买期限后从主库读取当前身份/分组，保留原认证版本 fence；待提交限制性身份更新仍拒绝。红绿证据为 `/tmp/new-api-round08-relay-group-red.log` 及本轮完整/race 回归。旧管理令牌查询保持原单次身份查询契约。

## B39：窗口来源被钱包核账误报且累计量未核对

初版核账将没有钱包分配的窗口请求判为资金缺失，且不能发现套餐累计值偏差。新增窗口分配、各代预占/合法计量/参考量与逐请求关联核对，再独立核对套餐累计只计一次。人工加 1 的累计值产生可定位差异，不自动修余额。红绿证据为 `/tmp/new-api-round08-window-lifecycle-red.log`、`/tmp/new-api-round08-growth-reconcile-red.log` 及 `/tmp/new-api-round08-broadened-green.log`，最终完整矩阵通过。

## B40：窗口规则依赖数据库大小写排序且旧重置配置被忽略

回归发现 Week、带重音或空格的标识可被发布，不同数据库可能产生规则碰撞；旧单计数器 daily 重置也可发布但新窗口不执行。修复：规则使用明确的小写 ASCII 协议标识，保留 term 并拒绝重复；版本化合同拒绝非 never 的旧重置配置，要求明确 window_rules。保留旧模式既有重置，新后台重置查询限定 plan_version_id=0，不能清空新窗口。红绿证据为 `/tmp/new-api-round08-rule-key-{red,green}.log` 与 `/tmp/new-api-round08-reset-errors-{red,green}.log`。

## B41：音频结算读取执行期间新改的倍率

真实 HTTP 回归在发送前保存音频倍率 2、输出倍率 3，上游处理时管理员改成 20、30。原新账本路径保存了旧价格，但结算函数重新读取全局倍率，17 个估算音频 tokens 的参考金额从 102 变成 10200，实扣耗尽 1000 并出现 9200 平台未收取量。修复新模式的音频/实时追加、结算及日志统一使用捕获倍率，按次换算使用会话捕获的 QuotaPerUnit；旧模式保留原契约。红测试 `/tmp/new-api-round09-audio-price-red.log`、绿测试 `audio-price-green.log`（相同前缀）、严格三库 `evidence-snapshot-matrix.log` 及全量/race 回归通过。价格其他路径及本轮结构升级仍列为第 9 轮未完成工作，不据此宣称全部价格审查完成。

## B42：金融意图可偏离已保存的最终计量

第 9 轮初版证据保存 25，但执行者调用结算 26 仍成功，原日志和可恢复依据失去一致性。回归同时观察错误金额提交及无法再接管原 25。修复在保存意图前验证最终证据的归属、指纹、价格摘要与金额；不同金额拒绝，资金和意图均不改。恢复继续使用原证据的 25，只扣一次并保留输入 12、输出明确 0、未知缓存字段和权限分层日志。红绿 `/tmp/new-api-round09-evidence-amount-{red,green}.log`、严格三库及全量/race 快照验证通过。业务修正使用后续追加事件，不通过覆盖原证据实现。

## B43：账户删除并发身份消失与测试事务配置

最初全量测试观察 SQLite busy、两个并发删除请求均未成功，单独重跑不能消除这份失败。核查发现安全测试使用 SQLite 默认延迟事务，与应用默认 WAL、busy timeout、immediate 写事务配置不同；受控双事务演示稳定复现延迟事务读后写升级失败，而应用配置下两笔写入成功。修正仅让安全测试复用应用连接选项，数据库文件仍隔离，不改变刻意测试延迟事务冲突的其他夹具。此证据解释配置造成的锁冲突机制，不声称取得了最初失败的完整调用栈。

进一步加强断言后 PostgreSQL 揭示另一个实际缺陷：唯一删除请求成功后，竞争请求偶尔得到 `AUTH_INTERNAL_ERROR`。确定性回归在会话查询与账户查询之间模拟已提交的账户删除，得到 HTTP 500，事务复核返回裸 `record not found`，预期应为身份失效的拒绝。修正会话、验证要求及事务复核只将记录不存在归类为身份失效；真正存储错误仍是 HTTP 500，原验证作用域、期限、一次性消费及会话撤销规则不变。

验证：`go test -v ./controller -run '^TestSecurityAccountDeletion' -count=1` 配合真实 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19 全部通过，包含仅一个胜者、全会话失效、消失身份及写入失败。红证据 `/tmp/new-api-round09-deleted-identity-red.log`；绿证据 `deleted-identity-green.log`、`deletion-mysql-final.log`、`deletion-postgres-final.log`（短名前缀均为 `/tmp/new-api-round09-`）。随后 `make test` 根模块和独立 relaykit、`make test-database` 严格三库及 Redis 7.4.11 零跳过、相关 controller race 和 `go vet ./model ./controller ./service ./relay/...` 均通过，证据前缀 `/tmp/new-api-round09-auth-fixed-`。

安全依据为 [ASVS 5.0.0 V7](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x16-V7-Session-Management.md) 的 7.2.1、7.4.1、7.4.2、7.5.3，以及 [Authentication](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html) / [Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html) Cheat Sheet；验证范围是本次身份失效分类及已有证明/撤销回归，不宣称整个认证系统已通过 ASVS。初次错误地扩大 SQLite 夹具配置导致旧延迟事务屏障测试阻塞，该次 `audio-reviewed-full.log` 中断且未通过；修正夹具范围后完整回归通过，不能把中断日志计作成功。

## B44：缓存原量与兼容结果混用，累计别名被误判冲突

第 9 轮审查发现标准缓存报告 0、供应商别名报告 8 时，兼容计费用 8，但字段证据仍将 0 标成最终核实量；仅有别名的累计流从 8 更新到 12，又被当作与旧标准值冲突，停留在 8。修复将原标准量、供应商别名和兼容结果分开保存：矛盾保留原量并标 adaptor/冲突算法，正常累计别名更新不产生冲突。

先失败：`/tmp/new-api-round09-cache-alias-evidence-red.log`、`cache-alias-cumulative-red.log`；修正后 `provenance-reviewed-green.log` 通过。严格真实三库、全量根模块/独立 relaykit、相关 race 和 vet 的协议阶段快照均通过，前缀 `/tmp/new-api-round09-protocol-snapshot-`。未声称合成矛盾回执是真实供应商行为。

## B45：零费用依据漏掉原生归一化分类

Claude 表达式 p 会合并未独立定价的缓存读取，之前只检查输入/输出就可能把未知缓存当作可靠零费用。另一方面，Gemini 合计虽然由完整原始字段确定，却被一概判成无依据，真实零进入核查。修正按原生表达式语义检查 Claude 缓存；Gemini 只在原始分量均为完整 upstream、合计严格一致且算法明确时确认依据，其他 adaptor 结果不自动获得该资格。

先失败：`/tmp/new-api-round09-native-zero-proof-red.log`（Claude 未知应 review 却 settled，Gemini 已报告零应 settled 却 review）；修正后 `native-zero-proof-green.log` 和协议阶段严格三库矩阵通过。规则仍维护在设计/开发计划，bug 归档只记录实现与既定依据不一致的缺陷。

## B46：证据投影丢弃断流估算，保留早期部分计数

早期报告输出 3、最终根据已收到内容估算为 5，消费快照使用 5，而证据查询仍返回 3/upstream/partial；早期报告 8、估算 5 时，查询也未表达最终有下限的估算。修复累计投影：只保护完整 upstream 回执；部分回执可由估算补全，数量不低于已报告部分计数。后续完整零回执仍覆盖估算，之后的估算不能改它。原事件始终不可变。

红绿 `/tmp/new-api-round09-partial-projection-{red,green}.log`，严格三库、完整后端、相关 race 和 vet 均通过，最新证据前缀 `/tmp/new-api-round09-image-snapshot-`。

## B47：没有图片内容的 completed 事件仍减少计费张数

请求 3 张图片，SSE 仅返回一个 completed 事件和 revised_prompt、没有 URL/base64 图片内容，原实现把数量改成 1，违反已有图片计数规则。回归期望保留 3、原实际为 1。修复只统计带非空 URL/base64 的 completed 事件；正常结束、客户端中断及已完成数量超过请求数量的原规则保留。

先失败 `/tmp/new-api-round09-image-empty-completion-red.log`，修正后 `image-reviewed-green.log`、严格三库、完整后端、相关 race 和 vet 通过，后者前缀 `/tmp/new-api-round09-image-snapshot-`。此缺陷记录不替代新增字段级图片用量功能的开发计划。

## B48：请求结束时读取新工具单价与文本积分换算

真实 HTTP 回归在发送前设置工具单价 10，上游处理期间改成 20，原结算实扣 10020，预期按捕获价格实扣 5020；初价明确为 0 时原实扣仍为 10020，预期只有基础费 20。原请求价格记录也没有工具价格。文本计算回归捕获换算 500000、运行时改成 5000000，原金额 50000，预期为 5000。

修复工具价格从一个不可变索引按现有模型前缀规则捕获，连同积分换算与 Gemini 输入音频单价写入请求；文本结算和日志使用捕获值。明确零保留，不因配置后改而收费；旧账户保持原运行时查询规则。

红证据 `/tmp/new-api-round09-tool-price-red.log`、`text-price-red.log`，针对性绿证据 `tool-price-green.log`、`text-price-green.log`。完整根模块/独立 relaykit、真实 SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 严格零跳过矩阵、相关 race 及 vet 通过，证据前缀 `/tmp/new-api-round09-price-snapshot-`。严格门禁专门验证三库的初价 10、0 分支，遗漏自测先失败再修复：`price-gate-{red,green}.log`。初次混用顶层及子测试筛选时 controller 显示 no tests to run，单独重新运行子测试观察实际失败；该次空筛选不作为覆盖证据。

## B49：新语音回执处理重复计算参考音频，读取失败丢失已报告数量

第 9 轮新增语音数量响应头处理时，输入 14、其中音频 4、缺少文本分类，初版又把全部 14 算作文本，正确剩余应为 10。数量头已报告输入 14、输出 31，但读取音频失败时提前返回旧预估 40/0，账单计量与保存证据不一致。

修复缺失文本分类从输入扣除已知音频，仍明确标为估算；读取失败保留先前可靠数量，记录不完整结果。回执不因媒体读取失败变成未知或零，重放和平台不欠款规则保持。此条记录新实现的缺陷，新增 SSE/响应头能力的范围仍维护在开发计划。

先失败 `/tmp/new-api-round09-speech-review-red.log`，针对性通过 `speech-reviewed-green.log`。完整根模块/独立 relaykit、SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 严格零跳过矩阵、相关 race 和 vet 均通过，最新阶段前缀 `/tmp/new-api-round09-speech-final-`。矩阵强制每库完成 SSE、非流式数量头和明确零分支，遗漏自测先失败再通过 `speech-gate-{red,green}.log`。零用量夹具无音频内容，不把“有输出音频却报告零 codec tokens”强行设为正常供应商语义；这些是公开协议的合成测试，不是实际供应商调用。

## B50：途中适配器合计未作为断流估算下限

途中合计来源为 adaptor、数量 8，最后估算 5，内存结算保护下限 8，但证据投影显示 5。根因是投影把“来源”和“是否完整”混用，只保护 upstream 的部分计数。修复让任何有数量的部分事实都提供估算下限；完整 upstream 仍保护，正常估算增加到 5、后续完整明确零及估算不能覆盖完整零的原规则不变。只统一现有投影分支，没有增加抽象层。

红绿 `/tmp/new-api-round09-adaptor-partial-{red,green}.log`；最新 `speech-final-matrix.log` 的真实三库序号/投影分支、完整回归、相关 race 和 vet 均通过，短名前缀 `/tmp/new-api-round09-`。原不可变事件没有被修改。

## B51：异步任务内存投影和重放将未收取费用算进用户扣款

参考费用 150、积分包及 Key 合法可扣 100，资金事务正确实扣 100、平台未收取 50，但 `RecalculateTaskQuota` 把内存中的 task.Quota 写成参考费用 150；结算重放再次写成 150，与已经保存的任务实扣 100 不一致。后续展示或保存该对象会带回错误费用。

修复新模式的结算及匹配原参考金额的重放使用 Charged；即时成功提交也同步任务的实扣值。Actual 和 Uncollected 保持独立，不产生负余额、欠款或充值追扣。原模式保留原逻辑。

先失败 `/tmp/new-api-round09-task-collected-red.log`，正常、写入失败回滚、超预算及重放针对性通过 `task-collected-green.log`。完整根模块/独立 relaykit、真实 SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 严格零跳过矩阵、相关 race 和 vet 均通过，前缀 `/tmp/new-api-round09-task-collected-snapshot-`；每库必须执行 pending_over_budget，门禁缺失自测红绿为 `task-gate-{red,green}.log`（前缀 `/tmp/new-api-round09-`）。这只验证任务费用投影，任务字段证据和完整尝试成本仍属第 9 轮未完项。


## B52：Midjourney 超额结算的内存费用及重新读取重放错误

剩余可扣 30、参考费用 50，资金事务正确实扣 30，但内存任务仍显示 50；重新读取实扣 30 后，重放拿它与参考费用 50 比较并报冲突。修复内存投影统一 Charged，已结算任务允许原参考金额或持久化实扣值重放，并恢复为实扣值。账单参考金额和平台未收取 20 保持不变。

先失败 `/tmp/new-api-round09-task-recovery-red.log`，修复后 `task-terminal-green.log` 的实际 SQLite 分支通过。完整根模块/独立 relaykit、SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 严格零跳过矩阵、相关 race 及 vet 全部通过，最新阶段前缀 `/tmp/new-api-round09-task-terminal-`。这不替代新增任务终局证据及插件计量的设计契约。

## B53：Midjourney 交接执行资格后，失败重试仍使用旧资格

新增终局证据保存先成功、资金意图写入失败并交还资格；同一个内存任务再次结算仍使用旧执行资格，收到 lease lost。修复交还资格同时清除仅存在内存中的旧资格，后续重试重新取得资格，继续复用原证据。

先失败 `/tmp/new-api-round09-mj-lease-red.log`，严格三库验证意图写入失败、原证据不变、重试完成及重新读取重放；完整回归、相关 race 和 vet 均通过，前缀 `/tmp/new-api-round09-task-terminal-`。该阶段三库门禁新增即时任务、任务意图前恢复和 Midjourney 分支，遗漏自测先失败再通过 `task-terminal-gate-{red,green}.log`（6 个测试）。


## B54：扩大并发回归暴露日志和测试共享状态竞争

并发任务轮询同时修改全局日志计数及轮换标志，race 检查确认冲突；另两处测试并发修改 Gin 全局模式，或在任务更新时读取同一任务对象的 ID。日志计数和轮换准入改用原子操作，文件写入的既有锁保留；测试不再并发改全局模式，异步启动前保存不可变 ID。没有用串行化轮询来掩盖竞争。

红证据 `/tmp/new-api-round09-task-fields-race.log`，针对性 `task-fields-race-fix.log` 通过；扩大范围最终 `task-numeric-final-race.log` 通过。严格三库、完整回归和 vet 通过，前缀 `/tmp/new-api-round09-task-numeric-final-`。根因是共享状态没有正确的所有权和同步；修复限制在原子状态和夹具值，没有新增并发框架。

## B55：未启用 Redis 仍创建返还缓存后台任务

即时任务的旧钱包返还在 Redis 未启用时仍创建协程；其迟到读取与测试结束恢复配置竞争。修复 `IncreaseUserQuota` 仅在 Redis 已启用时排队缓存更新；数据库和批量入账逻辑不变，未启用 Redis 不执行无效后台工作。

红证据 `/tmp/new-api-round09-task-fields-reviewed-race.log`，针对性 `task-integer-cache-green.log` 通过。扩大的任务/用量 race、完整根模块/独立 relaykit、真实 SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 严格矩阵及 vet 均通过，前缀 `/tmp/new-api-round09-task-numeric-final-`。此条只确认并修复额度返还的未启用 Redis 分支，不宣称所有旧缓存任务均已审计。

## B56：新增任务计量证据只接受 float64，拒绝合法 Go 整数

任务完成数量为合法 int64(6)，新证据保存强制断言 float64 而失败，资金意图不能提交。原插件协议允许所有数值类型，新增计量不能缩窄该契约。修复经项目 JSON 编解码规范化数字，拒绝 null、字符串及非数字，有限、非负和数量上限检查仍由证据边界执行；供应商积分小数不被舍入成整数。

红证据 `/tmp/new-api-round09-task-integer-red.log`，针对性 `task-integer-cache-green.log` 通过；三库必须验证整数计量的失败回滚、原证据保留、结算和重放。严格矩阵、完整回归、扩展 race 和 vet 通过，前缀 `/tmp/new-api-round09-task-numeric-final-`；遗漏门禁自测为 `task-integer-gate-{red,green}.log`（6 个测试）。


## B57：即时成功任务的最终费用超过余额时提前拒绝结算

任务提交前已预占 20，上游即时完成后的参考费用为 150，用户合法可扣为 100。原控制器再次预占 150，收到余额不足即返回，成功任务没有保存及结算，平台承担的 50 也没有记录。这与已确认的无用户欠款规则冲突；大金额饱和为 MaxQuota 的成功任务同样触发。

修复仅针对新账本已即时成功的任务，进入持久化及结算，收取合法可扣上限并保存平台未收取费用。提交前预占与未完成任务的调整预占保留；即时、轮询及恢复结算补齐数值饱和审计。

先失败 `/tmp/new-api-round09-task-immediate-cap-red.log`；SQLite 任务完整子矩阵 `task-cap-green.log` 已通过，含参考 150 / 实扣 100 / 平台未收取 50、金额饱和以及恢复后不重复扣款。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11 的严格零跳过矩阵、完整根模块/独立 relaykit、扩展 race 和 vet 均通过，前缀 `/tmp/new-api-round09-task-cap-final-`。矩阵要求每库完成恢复、超额和饱和分支，遗漏门禁自测先失败再通过 `task-cap-gate-{red,green}.log`。新增终局证据前崩溃恢复的能力维护在开发计划，不作为既有 Bug。


## B58：任务成功后把没有完成数量的估算零认作免费

提交估算数量为 0，成功回包没有完成数量，原证据仍把 ZeroChargeEstablished 设为 true 并完成零结算。完成状态不证明数量已报告，更不能证明真实零费用。

修复只用本次实际执行的计价分支判断零费用；缺失数值完成量的估算零转核查，原预占保留，资金意图不提交。常量免费、由已冻结请求条件选中的免费分支、明确完成数量 0 以及已冻结的零价按次任务保持正常零结算；未使用的计量缺失不构成门禁。表达式语义及价格不变，最终资金入口另检查证据的零费用证明。

真实失败 `/tmp/new-api-round09-task-zero-red.log`，SQLite 完整任务子矩阵 `task-zero-final-green.log` 已通过。补充核查持有原 20 预占；Key 的 UsedQuota 沿用现有预占计数，不能误断言为 0。初次扩展检查 `task-zero-reviewed-green.log` 因该夹具预期错误失败，不计作通过，修正预期后重新通过。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11 的严格零跳过矩阵、完整根模块/独立 relaykit、扩展 race 和 vet 均通过，前缀 `/tmp/new-api-round09-task-zero-final-`。门禁强制每库执行明确零、缺失量、常量免费、条件免费和保留占用；遗漏自测 `task-zero-gate-{red,green}.log` 先失败再通过（6 个测试）。


## B59：费用修正后窗口核账仍只累加原账单

第 9 轮新增修正的审查测试发现：原请求扣 60，返还 40；旧短窗口应为 20，新代次另有 10。旧核账仍累加原请求的 60，报告假的差异，且调高参考费用后仍遗漏最新参考量。修复按原账单与连续修正合并净费用，同时保留原分配的持有/结算核对，不把返还记入新窗口。

真实失败 `bill-adjustment-review-red.log`，修正后 `bill-adjustment-reviewed-green.log` 通过。首个失败日志还包含并发夹具沿用旧主键查不到最新记录的测试问题，已单独修正。严格三库矩阵、完整回归、扩展 race、vet 及新建/发布版升级后两次启动通过，证据前缀 `/tmp/new-api-round09-adjustment-final-`，迁移 `/tmp/new-api-round09-adjustment-migration/`。

## B60：异步任务迟到回写覆盖已生效的账单更正

账单修正已把任务实扣降低至 10，旧终局回执重放或旧内存任务的保存/CAS/计费状态回写却恢复了原来的 30；Midjourney 重新读取修正后的费用再重放还会被拒绝。资金虽然未重复扣，用户看到的费用和主账本不一致。

修复终局重放读取修正后的净扣款；新账本实扣只由资金事务写入，通用任务回写保留该字段。原费用及修正保持不可变，原模式继续沿用现有写入。先失败 `adjustment-task-replay-behavior-red.log`、`adjustment-stale-task-red.log`，对应 green 通过；严格三库、完整回归、扩展 race 和 vet 均通过，证据前缀 `/tmp/new-api-round09-adjustment-final-`。核账凭证/分配关联和独立日志恢复属于新增修正能力，其测试证据维护在开发计划，不混作旧缺陷。

## B61：删除 API Key 后原请求无法完成结算

请求已预占 40 并提交，随后删除 Key；最终费用为 25，原结算查询排除软删除记录而失败，资金占用不能关闭。修正返还也跳过已删除 Key，使历史 Key 用量与账户净费用不符。

修复只对已准入账务读取及更新历史 Key，保留删除标记；新请求仍拒绝，不能恢复凭证。真实失败 `deleted-key-red.log`，修正后的 `deleted-key-reviewed-green.log` 通过。首次 green 是 GORM 查询复用导致的歧义 SQL 失败，不作为通过证据。严格 SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 矩阵、完整根模块/独立 relaykit、扩展 race 和 vet 均通过，证据前缀 `/tmp/new-api-round09-deleted-key-final-`。遗漏自测先失败再通过：`deleted-key-gate-{red,green}.log`。

## B62：待结算账单查询把尚未扣款误判为金额不一致

金融意图已保存为 25，后续资金事务失败；实际扣款和平台未收取量都尚未确定，应为 0。原查询却对待结算账单使用已结算的金额恒等式，返回 invariant 错误，管理员无法查看这个真实待办。

修复只对已结算账单要求参考费用等于实扣与未收取量之和；待结算返回已知金额及阶段，实扣和未收取量保持 0。真实失败 `bill-review-audit-behavior-red.log`，`bill-review-audit-green.log` 通过，且原占用保持、后续恢复只扣一次。严格三库、完整根模块/独立 relaykit、扩展 race、vet 和新建/真实发布版升级后重复启动通过，前缀 `/tmp/new-api-round09-bill-review-final-`，迁移 `/tmp/new-api-round09-bill-review-migration/`。

## B63：自动异步失败覆盖已人工确认的未知账单

第 9 轮人工确认后，迟到的 Task/Midjourney 自动计量写入新的原终局关联，破坏确认的原依据；自动失败又取得新租约，把已确认请求改回核查，恢复不再扫描它。原释放入口同样会撤销确认，费用不能完成结算。

修复资金入口保护已确认结果，原证据不再允许自动生产者回写；自动计量/失败在确认后不接管请求，已结算重放读取当前净扣款。处理中的任务仍拒绝人工确认，不能借保护规则提前假定终局。真实失败 `bill-review-task-red.log`、`bill-review-late-failure-red.log`、`bill-review-fence-red.log`；对应修正后的 API/late-failure/fence green 通过。严格三库、完整回归、扩展 race、vet 和新建/真实发布版升级后重复启动通过，证据及迁移目录同 B62。

## B64：人工确认把估算数量当作已核实依据

原校验接受一切非 unknown 的完整数量，来源为 estimate 的数量也能使未知账单离开核查。这与凭新核实依据确认的契约不符。修复仅允许完整 upstream/adaptor 数量作确认依据；估算、未知或部分数量不能单独确认，原未知记录继续保留。

费用 0、25、120 的真实失败证据 `/tmp/new-api-round09-bill-review-source-red.log`；修复后的严格 SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 矩阵、完整根模块/独立 relaykit、扩展 race 和 vet 全部通过，前缀 `/tmp/new-api-round09-bill-review-source-final-`。未改变数据库结构，先前人工确认 fresh/release 迁移证据继续适用。


## B65：本地错误把裸 JSON 拼入 SSE

已设置 text/event-stream 的请求在预算校验失败后，通用结束处理仍输出 JSON HTTP 错误体；客户端无法按事件流读取错误。真实失败 `/tmp/new-api-round09-stream-budget-storage-red.log`，改为对应流式错误事件后 storage-green 和五条 contracts-reviewed-green 通过。尚未发送第一段内容时也保持已声明的 SSE 类型，并使用错误状态；已经发送后保留原 HTTP 状态，追加错误事件，不发送正常 DONE，不暴露内部存储错误。

严格 SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 矩阵、完整根模块/独立 relaykit、扩展 race（含扫描器）和 vet 全部通过，前缀 `/tmp/new-api-round09-stream-budget-reviewed-final-`；结构/元信息重启及发布版升级通过，目录 `/tmp/new-api-round09-stream-budget-migration/`。新增预算功能本身维护于设计/计划，不能混作原缺陷。

## B66：流式预算临时投影复制扫描器共享计数

新增监测复制整个 RelayInfo，与扫描器更新 ReceivedResponseCount 竞争。真实竞争失败 `/tmp/new-api-round09-stream-budget-final-race.log` 指向 OpenAI 处理回调和扫描器写入；金额业务测试通过不能证明该并发路径安全。

修复只投影由流处理者持有的计量、价格和资金字段，不读取扫描器接收计数或首响应时间，不把临时估算写回原证据。正确顶层筛选 `stream-budget-race-reviewed-green.log` 通过，随后 B65 所列完整复验全部通过。首个窄筛选 race-green 未执行 controller，不作问题修复证据。


## B67：Gemini 预算停止又被转换成供应商失败

Gemini 转 Responses 在本地预算错误已发送后，通用异常收尾继续发送 server_error 和 response.failed，造成两个错误且错误归责。真实 HTTP 上游只发送前两段并保持连接，等待网关取消；确定性失败 `/tmp/new-api-round09-gemini-budget-outcome-red.log` 显示两个 error 事件及伪造的供应商失败。

修复在通用异常和成功收尾之前处理本地预算停止；只发送一个预算错误，关闭上游并按已观察用量结算。native-budget-reviewed-green 通过；根模块/独立 relaykit 全量、含扫描器的扩展 race、vet 退出 0，前缀 `/tmp/new-api-round09-native-budget-final-`。初次三库矩阵因新测试推荐码过长失败，不计通过；夹具改为合法短推荐码后严格 SQLite 3.50.4/MySQL 8.4.11/PostgreSQL 15.19/Redis 7.4.11 零跳过通过，`native-budget-reviewed-final-matrix.log`。未改宽生产字段或放松账务断言。

## B68：新账本重试成功的费用归到首选渠道

账本日志和渠道统计直接使用初始请求的 ChannelID/Group；第二个渠道成功后仍把参考费用加给第一次渠道，后续修正也调整错误渠道。真实失败 `/tmp/new-api-round09-attempt-routing-red.log` 显示初渠道从 35 增为 91，实际成功渠道为 0，日志保留旧组。

消费和修正由不可变终局关联的实际尝试取得渠道及分组，初始请求合同不覆盖。测试成功收费 56、修正后 40，首渠道统计保持不变。SQLite 全信用用例、严格三数据库/Redis、完整根模块与独立 relaykit、扩展 race、vet 全部通过，前缀 `/tmp/new-api-round09-attempt-price-final-`；发布版升级及重启的实际尝试归属另见开发进度的 attempt-price-migration。

## B69：普通账单查询与核账漏验计量凭证

查询和核账只验证人工确认凭证，普通已结算账单损坏计量/实际价格关联仍可当作正确账单。真实失败 `/tmp/new-api-round09-attempt-price-proof-red.log`：破坏实际价格记录后查询未返回约束错误。

所有有终局或人工确认凭证的账单验证原始指纹、价格摘要及对应尝试关联；损坏时查询拒绝，核账保留具体关联差异，不把余额守恒当作凭证正确。测试恢复原记录后余额、修正和核账仍正确。复验命令、三库版本和退出 0 结果与 B68 相同。新增价格功能维护在设计/计划，这两个已复现错误单独归档。

## B70：WebSocket 预算错误后仍转发缓冲事件

预算 worker 已发送错误并释放 current / done 后，读取协程可能把已缓冲的后续事件当作连接空闲事件转发，客户端在错误后仍收到内容或完成。真实完整失败 matrix/full/race 日志前缀 `/tmp/new-api-round09-ws-budget-final-`；业务 targeted 通过未覆盖所有收尾交错，不能据此关闭。

错误终局先写一次，然后在释放准入和唤醒读协程之前取消连接上下文；通用客户端写入在写锁内检查取消，下一次准入也拒绝取消连接，避免读协程 done 分支绕过预算。Controller 账务和原有 WebSocket 行为用例通过 close-green；该命令在 relay 包未匹配用例，因此不作为 relay 单测证据。后续完整根模块/独立 relaykit、严格三库和 Redis、扩大到 `Test.*ResponsesWS.*` / `TestResponsesWebSocket.*` 的 race、vet 全部退出 0，前缀 `/tmp/new-api-round09-ws-budget-reviewed-final-`。测试确认错误后无额外内容，且网关先关闭上游，测试再关闭客户端。原取消控制、连接复用、跨响应隔离及旧模式回归继续通过。

## B71：异步终局恢复退回初始分组价格

第二次尝试已经持久化实际提交价格，但成功状态/完成数量落库、最终计量尚未写入时，恢复仍从初始 PriceSnapshot 重算。真实失败 `/tmp/new-api-round09-task-attempt-price-red.log`：数量 3.5、倍率 2 应为 35，恢复为 18；第二次倍率 0 应免费，也被扣 18 且没有零收费依据。

异步证据及恢复共用实际提交价格读取，有尝试凭证时采用最新不可变版本，无该凭证的历史内部请求才用准入价格。保留持久化完成数量、单位及原始合同，重启不读取当前管理价格。恢复日志分组倍率同时更新；收费和零价分支最终都关联第二次尝试。测试覆盖预占追加、真实插件轮询、在计量写入处失败、管理价格后来变更、恢复重放。`make test-database`、`make test`、含原生 WS 的扩展 race、vet 全部退出 0，前缀 `/tmp/new-api-round09-task-attempt-price-final-`。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，严格两种新恢复分支零跳过；Python 门禁从漏跑仍通过的两条失败到 6 项通过。新建及实际发布版升级后两次 InitDB/InitLogDB、第二次提交价格及小数数量的重启恢复均通过，目录 `/tmp/new-api-round09-task-attempt-price-migration/`，upgrade-result/fresh-result 退出 0。未增加表结构。

## B72：转换协议绕过流式预算并丢失零回执来源

现象：Chat / Responses 双向流转换在原资金预算不足或账务存储故障时，仍把超额生成内容和正常结束事件发给客户端；客户端 Responses 经上游 Chat 的非流式请求也没有保留原字段，供应商明确零和缓存零会错误进入核查。

修复：转换前保存原协议字段，用原计量和冻结价格检查预算后再转发；Responses 来源使用原累加器，Chat 来源按原字段和文本/工具估算补缺。不足只结算合法额度，未知故障保留核查，不发送正常完成。明确零与缺失分开，转换不另计费用。

验证：扩展既有 controller 真实 HTTP/三数据库表六条预算和四条 JSON 契约。conversion-budget-red 复现输出越界，conversion-json-red（临时撤去 JSON 修复）复现明确零错误核查；门禁十个遗漏先失败、补齐后 6 个自测通过。完整根模块/独立 relaykit、严格三库/Redis、扩展 race 和 vet 退出 0，`/tmp/new-api-round09-conversion-final-{matrix,full,race,vet}.log`；加强正常终局来源与价格关联断言后 conversion-proof-final-matrix/race 退出 0。数据库 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19，Redis 7.4.11，零跳过。两个包含错误筛选的 green 文件未执行 controller，用于交付的证明是实际完整/三库执行。

状态：已修复并验证。新转换预算是开发能力，此条记录已确认的实际错误；第 9 轮的其他未完成项仍在计划，不据此关闭整轮。

## B73：语音流没有在生成期间检查额度

现象：转写和合成 SSE 将超额片段、完整终局转发后才处理最终费用；存储故障也在输出完成后才暴露。原平台未收取金额规则保持，但应及时停止继续生成。

修复：观察并保存原量、累加转写文本或 decoded 音频字节，在独立投影中补缺并检查预算后才转发。真实音频分类使用结算同一冻结倍率/表达式；其余转写输出沿原文本规则。不足按观察量封顶结算、不发送正常语音终局，存储/求值失败保留核查。表达式错误不改用其他价格继续输出。本地字节估算仍明确 estimate。

验证：既有 controller 表扩展七个真实 HTTP/三数据库分支，两类语音各不足/故障/正常，加原生音频倍率冻结；转写上传真实 WAV，两条持续打开流确认请求已取消。参考费 62/实扣 50/平台未收取 12；ratio 的原 audio=2/audio_completion=3，在上游处理中改为 20/30 仍参考 186/实扣 50/平台未收取 136。audio-budget-red 复现输出越界及迟到存储故障；frozen-behavior-red 是故意临时撤去新增监测冻结的回归验证，不声称此前生产实现存在这个改价缺陷。

完整根模块/独立 relaykit、首批严格三库/Redis、扩展 race/vet 全部退出 0，`/tmp/new-api-round09-audio-budget-final-{matrix,full,race,vet}.log`。仅增强测试后的 controller race 退出 0；首个 proof-final-matrix 因本任务可丢弃 PostgreSQL tmpfs 存满失败，恢复该专用容器后 reviewed-proof-final-matrix 严格零跳过退出 0。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11。编译失败和未写出内容时 HTTP 状态预期错误均在开发进展中记录，未当作通过证明。

状态：已修复并验证。实时语音、二进制音频输出阶段及逐项估算参数仍在第 9 轮计划，不据此关闭整轮。

## B74：实时语音预算检查迟到及输入确认重复计量

原生 Realtime 直到 response.done 才检查额度，超额片段和尾段已经转发，存储故障还错误产生 settle 意图。realtime-budget-red 实际失败，足额分支正常。新增实现的边界测试又发现客户端 conversation.item.create 未计量，修复后确认 conversation.item.created 被再次作为输出计量，ack-red 复现足额 8 被参考 12/实扣 10 并停止。

修复：单个持有用量的处理循环用已完成回执加当前观察量检查冻结价格；触发不足输出不转发，按合法资源封顶，原生错误发出后关闭连接与上游。存储/求值失败核查且不写收费意图。客户端创建在发送前计量，拒绝且未发送的输入不收费，服务器确认不重复计量；原回执留存，当前观察估算不冒充上游终局。

验证：七条真实 WebSocket 链路覆盖文本、PCM16、未知存储故障、正常回执、前一响应叠加、拒绝和足额输入确认；检查输出边界、上游关闭、无重试、来源凭证、Key/包守恒及核账。make test-database、make test、扩大含 Realtime 的 race、vet 全部退出 0，`/tmp/new-api-round09-realtime-budget-final-{matrix,full,race,vet}.log`。三库版本 3.50.4/8.4.11/15.19，Redis 7.4.11，严格零跳过。boundaries-green 名称的失败没有作为通过证明。

状态：已修复并验证。估算参数及其余实时协议、阶段证据仍按开发计划完成，整轮尚未提交。

## B75：缓冲 Responses 生成没有过程预算检查

客户端要求一次性 JSON，上游以 SSE 返回时，网关原来缓冲到终局才计费。实际不足仍返回成功及所有内容，存储故障后仍写 settle 意图。buffered-budget-behavior-red 实际退出 1，正常及按次分支通过；首个 red 的 int/int64 编译错误不作为行为证据。

修复：转换累加器与计量累加器分别管理呈现和原费用；每帧观察后先检查冻结价格。不足返回一个 JSON 错误，按已观察量封顶结算，不把错误转成成功内容；未知故障保留核查、没有收费意图。停止主动关闭上游；正常和固定价格保留原完整返回。

四条真实 HTTP/三数据库场景检查 JSON 契约、上游取消、无重试、金额与来源、Key/包守恒和核账。make test-database、make test、扩展 race、vet 全部退出 0，`/tmp/new-api-round09-buffered-budget-final-{matrix,full,race,vet}.log`。SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，零跳过。状态：已修复并验证；未据此关闭第 9 轮。

## B76：新阶段记录在异步交接后使用旧执行资格

新增写入观察包装使 Midjourney 已持久化任务、交出租约后的提交回执失败，原成功 200 变为 copy_response_body_failed/400；relay-phases-boundaries 实际退出 1。交接成功后关闭本会话证据生产资格，回执保留原返回；不弱化数据库代次校验，不伪造缺失 accepted。已有异步任务行为用例保留并通过。

状态：已修复并验证。relay-phases-faults、relay-phases-race、relay-phases-final-matrix/full/vet 全部退出 0，三库版本 3.50.4/8.4.11/15.19、Redis 7.4.11，零跳过。设计阶段能力未完成不算 Bug，此条只记录已复现的新控制流回归。

## B77：通用适配器丢失计量分类凭证

通用最终证据仅保存输入/输出合计，适配器已返回的缓存、音频、图片和思考数量未保存。`adaptor-facts-red` 在既有实际尝试价格场景复现；通用入口现保存全部分类，来源为 adaptor，不能冒充供应商原字段。原字段事实优先，无法证明字段存在的零记 unknown，缓存模态指针的显式零保留。参考费用 56、修正后净扣款 40 及渠道/Key/包保持原规则。

状态：已修复并验证。`/tmp/new-api-round09-adaptor-facts-final-{matrix,full,race,vet}.log` 全部退出 0；SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，严格矩阵零跳过。未新增表结构。

## B78：输出阶段凭证失败后继续缓冲转发

新增阶段写入失败后，Chat 流仍可能发送已经缓冲的后续内容；真实 HTTP 又复现错误被重分类为预算错误，以及写后故障使 SSE 错误与上一帧粘连。`relay-fault-stop-red`、`relay-fault-http` 均实际失败。

修复：阶段错误持久转核查并保留内存防线，包装器拒绝后续生成写入；协议专用错误入口只发送一个隐藏内部详情的终局错误。Chat 缓冲路径仅将凭证错误作为致命错误，普通客户端断开沿原规则处理；写后故障先结束上一帧，再写错误。审查中 getter 把 nil 指针装入 error，误判健康请求；`relay-fault-client-semantics` 复现，显式 nil 判断修复后通过。

状态：已修复并验证。`/tmp/new-api-round09-relay-fault-final-{matrix,full,race,vet}.log` 全部退出 0，三库版本 3.50.4/8.4.11/15.19、Redis 7.4.11。六种写入/存储边界及两个真实 HTTP 故障各库零跳过，逐帧 JSON 可解析、无重试、无收费意图、原占用及核账一致。新建与真实 v1.0.0-rc.41 升级、两次初始化通过，新增可选 Observation/媒体 components 的不可变重放保持，证据目录 `/tmp/new-api-round09-relay-metadata-migration/`。

## B79：图片流超额后继续输出

已确认：请求 n=1，上游返回更多图片，或图片 token 回执超过可用额度时，超额载荷及 DONE 仍转发；账务存储失败也直到最终结算才处理。真实失败 image-budget-red。现已在载荷转发前检查原规则下的图片数量/token 费用，已确认不足按合法额度结算并停止；未知存储故障保留核查。正常数量/计价、JSON 转 SSE 和旧按次价格均有回归。第一次实现又复现扫描器提前看到 DONE 后错误发送完成，已按预算停止标记修复。已通过严格三库、全量、竞态及 vet，整轮最终证明见下文。


## B80：凭证故障中止与请求读取竞争

扩展 race 的 image-budget-final-race 实际失败，phase_possible / phase_accepted 的流处理协程修改 c.Request，主扫描协程同时读取该指针。原停止方法为正常收尾恢复请求对象，新故障路径在流工作器中调用了它。

修复保留正常收尾，新增故障中止只取消已建立的上下文，不改写共享 Gin 请求指针；同一个 once 防止随后收尾重新赋值。原公共错误和占用核查行为保持。生成/flush 的凭证错误优先于由它引发的取消，保留原错误分类。最终完整验证已通过。


B79、B80 最终证据：`make test-database`、`make test`、扩展 `go test -race` 和全范围 `go vet` 全部实际退出 0，日志 `/tmp/new-api-round09-reviewed-final-{matrix,full,race,vet}.log`。三库版本 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19、Redis 7.4.11，严格零跳过；图像七个分支及两个持续上游取消、六个写入故障边界/两个 HTTP 阶段故障全部执行。B80 两次修复后的中间 race 仍因取消掩盖错误失败，记录于开发进度；只有最后 cause-race-green 与 reviewed-final-race 属于通过证据。
