# 项目文档

本项目基于 New API 二次开发。当前目标是提供多上游、多用户的 AI 转发服务，并支持可解释的计费、订阅套餐和有有效期的积分包。

## 阅读入口

| 需要了解什么 | 文档 |
| --- | --- |
| 产品目标、已确认需求、使用规则和验收标准 | [产品设计](design/README.md) |
| 额度窗口、积分包、预占、结算和退款怎样协作 | [账户与计费设计](design/accounting.md) |
| 已确认的问题和修复状态 | [Bug 目录](bugs/README.md) |
| 实施顺序、修改范围和验证安排 | [开发计划目录](plans/README.md) |
| 当前二开的阶段任务、测试和发布条件 | [订阅、积分包与计费开发计划](plans/subscription-billing.md) |
| 当前开发轮次、入口清单和实际验证结果 | [开发进度](plans/development-progress.md) |
| 账务测试的输入、预期和恢复断点 | [账务验收与故障场景](plans/accounting-verification.md) |
| 已结束或被替代的设计、计划和问题记录 | [归档目录](archive/README.md) |

## 目录结构

```text
docs/
├── README.md
├── design/
│   ├── README.md          # 当前产品设计
│   └── accounting.md      # 当前账户与计费设计
├── bugs/
│   └── README.md
├── plans/
│   ├── README.md
│   ├── subscription-billing.md
│   └── accounting-verification.md
└── archive/
    ├── README.md
    ├── design/
    ├── bugs/
    └── plans/
```

上图只列二开文档结构。现有 `authentication.md`、`channel/`、`installation/`、`openapi/`、`plugin-api/` 和翻译词汇表等上游资料保留原位；它们不是历史归档，也不因此失效。

## 文档维护规则

- `design/` 说明目标行为和业务规则；代码组织、接口细节和实施步骤放到 `plans/`。
- 每个主题只有一个当前有效的说明；相关文档通过链接引用，不复制一套规则。
- 用户已确认的要求写为“已确认”；提议但未确定的选择写为“待商榷”，不能据此声称需求已定稿。
- 功能尚未实现属于设计缺口；只有可复现、已确认的错误才进入 `bugs/`。
- 文档归档时移动到对应分类，更新入口和引用，注明结束或被替代的原因。归档内容不作为当前实现要求。
- 修改设计时同步检查需求编号、验收案例和开发计划，避免三者相互矛盾。
