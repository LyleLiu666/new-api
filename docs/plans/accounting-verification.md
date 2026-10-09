# 账务验收与故障场景

状态：本次开发范围的 S01–S18、F01–F10 与 A01–A24/I01–I14 已通过，具体证据见第 6 节。本文给出输入、故障断点和必须观察的结果；实际通过范围以开发进度的证据为准。第 10 轮已验证 S18 的账号绑定、用户隔离、轮换、重排和故障模式，以及 S08 的已输出/未知提交重试边界；第 11 轮已验证权益失效、窗口/账单/购买/管理页面、七语言及真实浏览器，第 12 轮已补齐真实中断、多进程、Redis、日志清理、启动与容量证据。

产品预期以[产品设计 A01–A24](../design/README.md#11-验收标准)为准，正确性以[账户设计 I01–I14](../design/accounting.md#3-必须保持的正确性)为准，实施顺序见[开发计划](subscription-billing.md)。用例中的积分和时间均是测试数据，不代表商品售价或额度。

## 1. 测试方式

时间采用可控服务端时钟，不等候现实时间过去。并发通过明确的同步点控制，确保请求确实在竞争同一余额或窗口；使用独立数据库连接，不能用单一串行连接假装验证并发。MySQL/PostgreSQL 使用真实实例，SQLite 验证实际部署可采用的连接与事务方式。

用例先断言 API 或业务操作结果，再查询已提交数据核对来源分配、包守恒、窗口代次、Key 额度及请求账单。重复与失败场景不能只看返回值。恢复测试从数据库重新加载对象，并至少包含一次真实子进程退出/重启验收，防止内存状态掩盖持久化缺口。

外部上游和支付网络使用可控模拟服务器；不能 mock 被测账务入口。金额及窗口预期直接给定，不在测试里复制生产算法算预期。业务规则未确定的案例先保留场景与依赖，规则确认后补准确断言；不以永久跳过的用例作为验收证据。

## 2. 积分、窗口和订单场景

| 场景 | 输入与触发 | 必须观察的结果 | 关联验收 |
| --- | --- | --- | --- |
| S01 跨包分配与结算 | A 剩 30 分早到期，B 剩 100 分晚到期；预占 50，实际 35 | 预占 A30/B20；结算 A30/B5，释放 B15；A 净消耗30，B 可用95，无重复费用 | A07、A11 |
| S02 并发争抢 | 仅有 50 分，两个请求同时预占 40 分 | 恰好一个成功，另一个不足；可用10、预占40；不能靠负余额让两者成功 | A11 |
| S03 原子失败与防重 | 分配两包过程中注入写入失败；随后重试同操作；重复同 ID 不同金额 | 失败不留半份分配；成功重放不再扣款；不同金额冲突拒绝 | A07、A11 |
| S04 到期与释放 | A30 在 T+1 到期，B100 在 T+10 到期；T 预占50，T+2实际20 | D06 已确认：A消耗20、未用预占10失效、B预占20全部释放；A 不恢复可用 | A08、A12 |
| S05 已结算返还 | S01 已结算35，之后调整为20；此时 A 已到期 | 按原 FEFO 分配顺序重算净扣款，净结算变为A20/B0，A返还10计失效、B返还5可用；调整重放无变化，不发新积分 | A13、A18 |
| S06 生效与用途 | 一个包未来生效，一个已到期，一个用途不匹配，一个合法可用 | 消费只使用合法包；无合法包即拒绝，清理任务未执行也一样；同到期顺序稳定 | A08、A16 |
| S07 多窗口共同约束 | 5小时上限60，周上限200，套餐总上限500；已用分别55、100、100；预占10 | 5小时不足，三个窗口及Key均无新增预占；5小时新代次开启后可按其他窗口余量使用 | A03、A05 |
| S08 时间边界及并发启动 | 付款 T，首次请求 T+2天；两个Key并发首次使用，首请求失败而另请求成功 | 套餐 T+30天到期；短窗口由 D01 规定时点开始且只开一代；失败不能撤销成功请求使用的代次；到期时刻起拒绝新使用 | A04、A12、A15 |
| S09 旧窗口完成与补差 | 旧5小时代次预占30；新代次已用10；旧请求后来实际35 | 原预占与实际计量属于旧代次；新代次已用仍为10。额外5的支付与是否允许超限按D06/D07处理，不能偷偷扣入新窗口 | A12、A24 |
| S10 旁路、免费与无限额Key | 总余额大于信任阈值但可用包已到期；无限额Key对应已耗尽窗口；免费模型调用 | 前两者仍拒绝；免费调用按D01/D02检查是否启动和计量窗口，有请求记录，不凭价格零免除所有规则 | A15 |
| S11 余额购买套餐 | 套餐需40分；允许用途的包A30/B100，重复购买通知；创建订单或订阅时失败 | 正常扣A30/B10并开通一次；事务失败不留下扣款；赠送包是否参与按D03；购买成功不自动退款；管理员取消停止新权益，保留原扣款来源 | A16、A17 |
| S12 锁价与延迟到账 | 下单锁版本V1，随后改为V2或下架；付款T，通知T+2天；再收到重复/金额不符通知 | 按D04订单合同开通，有效期以核实付款T起算；重复不开通，金额不符不盲发；币种和舍入有固定预期 | A04、A17、A23 |
| S13 商品退款与撤销 | 已发包100，其中已用20、预占30、未用50；重复支付撤销，管理员手动取消 | 各分量都可查，不把100无条件再扣或返还；停止新使用，按D12处理在途及已用；系统不执行现金退款；已用、预占及取消操作可查询，不自动返购买款 | A17、A20 |

S05 的费用修正按原 FEFO 分配重算，保持原来源、有效期和守恒；这是请求费用纠正，不是商品购买退款。

## 3. 用量与路由场景

| 场景 | 输入与触发 | 必须观察的结果 | 关联验收 |
| --- | --- | --- | --- |
| S14 回执归并 | 重复和乱序累计回执、一次增量回执、协议转换前后的同一事实 | 依据协议累计/增量语义归一化；重复事实不重复加量，早期回执不覆盖后期完整值；矛盾数据可查 | A09、A10 |
| S15 中断与未知 | 仅有输入/缓存回执后断流；仅输出增量后断流；无任何证据；明确零用量 | 已知输入与缓存保留，估算输出单列；未知与真实零不同；按D05/D10结算，依据与覆盖范围可查 | A09、A10 |
| S16 计价与调整 | 请求开始后管理员改价；原账单后续调高/调低；小数临界值、非法和超大数量 | 原请求沿价格快照；按既有安全换算得到固定预期；同调整只生效一次；正常费用不变成入账；上游成本与用户费用分别记录 | A09、A13、A23、A24 |
| S17 重试许可 | 上游未提交、提交后结果未知、已向客户端输出后连接中断 | 按D11逐阶段判断；输出后不从头重放拼接；未知执行保留尝试记录；网关防重不宣称上游只生成一次 | A10、A19 |
| S18 账号及权益隔离 | 同名会话来自两用户；固定多Key渠道后凭证数组重排/轮换/禁用；标签已缓存时订阅被终止 | 会话不串用户，绑定不因位置变动变成别账号；故障按路由模式处理；失效标签不继续授予权益，公开查询无成本和凭证 | A01–A03、A06、A21 |

首批协议分别覆盖 OpenAI Chat Completions、Responses、Claude Messages 的流式/非流式；缓存读取、写入及工具量按各协议独立给定输入和预期。图片、音频、实时与异步任务只在本身已支持的计量基础上验证账本接入，不复制文本缺回执估算。

## 4. 故障断点与恢复

| 断点 | 注入方式 | 重启、接管或重试后的必要结果 | 关联验收 |
| --- | --- | --- | --- |
| F01 预占事务提交前 | 中止事务或杀掉进程 | 无已提交分配；仅重放操作时才可创建一次预占 | A11、A14 |
| F02 已提交但返回丢失 | 提交成功后丢弃响应 | 通过稳定操作ID查回结果，不重复分配；与真正未提交可区分 | A11、A14 |
| F03 预占后、上游提交前后 | 分别在已记录尝试、网络提交和收到回执前退出 | 根据持久化阶段与新证据恢复；提交后不确定不能仅凭超时全退或无条件再调用 | A14、A19 |
| F04 结算/释放竞争 | 两执行者同时执行互斥终结操作 | 只有合法状态转换生效，终结结果可查；不能既扣款又全退 | A11、A14 |
| F05 资金完成、其他步骤失败 | Key更新或独立日志库失败 | 同主库部分按事务约束；异库待办可重放，资金不再重复变化 | A11、A14 |
| F06 接管后原执行者迟到 | 明确让租约失效，新进程接管，旧进程随后提交 | 旧代次写入拒绝，新代次完成一次；两个接管者也只能一个取得资格 | A14、A20 |
| F07 Redis过期/故障 | 断连、旧快照、失效通知丢失、多实例读到不同缓存 | 数据库有效包和窗口仍约束消费，不因缓存重建恢复已占额度；权益到期及时失效 | A08、A11、A21 |
| F08 费用调整结果未知 | 账本提交后响应丢失 | 查防重记录；不能新增第二次扣款或返还；系统不执行现金退款 | A13、A17、A20 |
| F09 归档后旧事件重放 | 归档明细，再重放支付/结算通知 | 必要唯一键及结果摘要仍拒绝重复；账本守恒，不以记录已删除为再次入账依据 | A20 |
| F10 新部署与模式写入隔离 | 新建、重复启动后恢复开发请求，旧总余额入口尝试修改新账户 | 账本和预占保持；旧写拒绝，全部 Key 使用同一用户账务模式 | A22 |

首次验证至少覆盖账本、窗口、Key和业务结果；“独立日志库不可用但费用已完成”可以保留可重放待办，但不能向用户虚报资金未扣而诱发重新消费。

## 5. 迁移与证据模板

无历史用户商业迁移。结构验证使用最近上游发布版本创建的代表性开发库，核对用户、Key、旧订单、索引和唯一性；新账本样本覆盖发包、在途预占、已结算、已过期和付款未开通订单。重复启动和中断恢复不得重发或丢失来源。

新增确定案例：预占20、合法余额共30、最终费用50，应扣30、平台未收取20，包和有限额Key非负；之后充值10仍完整可用，不追扣旧20。同套餐付款续费时在原到期日加30天、不提前重置窗口；商品改价/改标签不影响旧权益；管理员取消不自动退钱。

每次验证记录：场景/断点编号、关联A和I、依赖的D决定、代码提交、数据库及Redis版本、配置与命令、失败到通过的证据、提交后数据、失败或跳过原因。不得在文档记录凭证或完整敏感请求。

发布前按[开发计划 P6](subscription-billing.md#p6迁移发布与完整验收)核查全部关联场景，逐项标记通过、失败或未执行。待商榷的规则或必要验证缺失时，对应功能仍为未完成；测试数量和文档完整度不能代替正确性证据。

## 6. 最终验收映射

2026-10-09：下表所有开发契约通过。`make test-database` 的最终日志为 `/tmp/new-api-round12-root-reviewed-matrix.log`，对真实 SQLite 3.50.4、MySQL 8.4.11、PostgreSQL 15.19 执行相同业务断言，必要分支不允许跳过；Redis 7.4.11 与 ClickHouse 25.8.33.6 为真实实例。测试文件中查询已提交包、分配、窗口、Key、意图、证据及账单，不只断言 HTTP 成功。

简称：CP = [TestCreditPackDatabaseMatrix](../../model/credit_pack_test.go)；CB = [TestCreditBillingDatabaseMatrix](../../controller/credit_billing_test.go)；SV = [TestSubscriptionVersionDatabaseMatrix](../../model/subscription_reset_test.go)；AA = [TestAccountAffinityDatabaseMatrix](../../controller/channel_pin_retry_test.go)。具体断言位于下列场景；受影响模块的全量/race及版本启动证据见开发进度第 12 轮。

| 验收 | 可观察结果与代码/测试证据 |
| --- | --- |
| A01 | AA/off_balances_and_prefer_falls_back、strict_pins_actual_account；配置允许集合分配与实际账号记录 |
| A02 | AA/strict_pins_actual_account、concurrent_strict_claim_and_expiry；严格报错保留原账号，prefer允许切换并记录 |
| A03 | SV/window_consumption、CB/subscription_windows_share_keys_and_independent_wallet、AA/same_session_isolated_by_user；共享Key窗口及用户归属 |
| A04 | SV/locked orders and verified payment facts、window_consumption；付款锁定连续30天、提交使用时启动短周期 |
| A05 | SV/window_consumption；5小时耗尽即拒绝，不部分改变周/总窗口或第二个Key |
| A06 | SV/renewal preserves purchased contract and remaining term、current rights查询；CB/credit_admin_API_contract及React权益失效用例，原版本标签/截止时间可查 |
| A07 | CP/FEFO_idempotency_and_atomic_failure；明确A30+B20预占、A30+B5结算 |
| A08 | CP/validity_purpose_and_stable_ties、cross_expiry_and_rollback、CB/redemption_API_preserves_credit_policy；真实浏览器过期包仍不可用 |
| A09 | CP/usage_sequence_conflict_is_not_a_second_receipt、CB/reported_zero_and_missing_usage_are_distinct、Responses用量全量回归；已知/零/未知/缓存事实分开 |
| A10 | CB/stream_budget_stops_output_without_user_debt、buffered_responses_budget_preserves_json_and_accounting、realtime/Responses WS、account_retry_uses_one_bill_and_observed_boundaries；中断/重试只结算一次 |
| A11 | CP/concurrent_last_balance_and_replay、concurrent_finish_has_one_terminal_result、payment_callbacks_use_locked_credit_policy；CB/bounded_concurrent_http_consumption |
| A12 | CP/cross_expiry_and_rollback、bill_adjustment_uses_original_window_generations、SV/window_consumption；旧代次与原包不延长、不扣新代次 |
| A13 | CP/bill_adjustment_preserves_original_fefo_and_never_collects_again；CB/owned_bill_and_controlled_adjustment_API；调整意图/结果与独立日志待办可恢复 |
| A14 | CP/killed_process_preserves_transaction_boundaries五断点、process_exit_recovers_committed_intent、CB/clickhouse_keeps_visible_recoverable_log_work；真实终止、未知保留核查 |
| A15 | CB 初始真实relay的大旧余额旁路断言；SV/window_consumption免费、禁用/过期/零额度Key；Redis旧大余额依然拒绝到期包 |
| A16 | SV/balance purchase is atomic and uses eligible earliest expiry；购买来源FEFO、防重、事务失败回滚 |
| A17 | SV/manual payment review preserves evidence and serializes activation、CB/owned_orders_and_manual_payment_review_API、CP/admin_grant_and_refund_review_preserve_source；管理员取消不退款，保留在途 |
| A18 | CP/bill_adjustment_preserves_original_fefo_and_never_collects_again；返还回原来源，过期返还失效，管理员可另发有期补偿包 |
| A19 | CB/account_retry_uses_one_bill_and_observed_boundaries的known_429/unknown_submission/output_started；阶段证据失败不重放 |
| A20 | CP/two_process_takeover_rejects_original_writer、log_cleanup_preserves_ledger_and_old_event_defences；两个实际进程恰好一个新代次、旧事件仍防重 |
| A21 | AA/rotation_reorder_and_retirement、same_session_isolated_by_user、account_health_survives_key_reordering；SV权益失效及CB/权限/React缓存失效 |
| A22 | 新安装普通User.Insert、CB/initial_root_uses_shared_accounting_policy（新/旧模式、失败回滚、密码校验/一次散列、重复初始化）及两次InitDB/InitLogDB、最新发布v1.0.0-rc.42结构升级；CP/deployment_mode_persists_across_instances、credit_account_rejects_legacy_wallet_and_key_writers |
| A23 | SV/locked orders and verified payment facts、CB/CNY_draft_currency_is_preserved、stripe_versioned_paid_time_and_activation_retry、网关支付合同；React微金额与币种用例 |
| A24 | CP/platform_shortfall_never_collects_future_topups、bill_adjustment_uses_original_window_generations、supplementary_charge_respects_api_key_limit；补充值完整可用、平台承担旧超额 |

| 正确性 | 对应验收及直接证据 |
| --- | --- |
| I01 | A03/A21；多Key窗口、跨用户查询拒绝、同名会话分离 |
| I02 | A11/A20；来源、防重操作、支付事实、互斥终局重复执行 |
| I03 | A08/A15；未生效/到期/用途/Key及用户状态准入 |
| I04 | A11；多个真实SQL连接与20并发HTTP不超扣 |
| I05 | A07/A16/A18；FEFO跨包、原来源返还及独立补偿 |
| I06 | A12/A24；原窗口代次与实际/参考计量保存 |
| I07 | A08/A12/A18；到期释放/返还不恢复可用、不新增发放 |
| I08 | A09/A10；nil未知与明确零、部分回执、估算依据保留 |
| I09 | A10/A19/A24；单逻辑账单、尝试采购成本未知、平台承担 |
| I10 | A13/A14；真实事务失败、持久化结算意图、独立日志收据 |
| I11 | A09/A23/A24；既有quota_math/billingexpr全量安全回归、字段界限、非法证据拒绝 |
| I12 | A16/A17；订单、付款归属及稳定来源关联，系统不执行现金退款 |
| I13 | A14/A20；真实双进程接管、旧代次拒绝、同终局重放 |
| I14 | A20/A22；删除展示日志后核心操作/收据保留，重复启动保持未完成工作 |

故障证据：F01–F03、F05由五个SIGKILL及丢结果/独立日志用例覆盖；F04由并发互斥终局覆盖；F06由实际双进程及旧写入覆盖；F07由真实Redis旧快照、零快照、连接失败和权益重读覆盖；F08由调整防重/故障事务覆盖；F09按日志明细清理且核心防重长期保留验证，未声称已经实现核心账本物理归档产品；F10由三库新建/升级/重复启动和旧写隔离覆盖。

交付限制保持原设计：无法得到供应商真实量的字段为估算/未知，准确账本不冒充完整采购回执；ClickHouse非事务消费投影保留可见核查，SQL可带稳定ID恢复。生产容量与上线条件、外部权益资源映射、自动现金退款、转赠、历史商业数据迁移均不作为本次完成项。
