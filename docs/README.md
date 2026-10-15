# 文档索引与维护范围

核对日期：2026-10-12。代码基线：`ce3d52d`。正式开发目录为 `Code/change-detection-platform`；Inbox 中的 release 副本保留为回退材料，不作为当前实现依据。

2026-10-15 补充 AI 助手能力 TODO 专题及路线图关联；此次未重新核对全部现状文档，各专题以自身核对日期和证据范围为准。

## 当前文档

| 文档 | 权威范围 |
|---|---|
| [项目说明](../README.md) | 用途、快速使用与入口 |
| [当前系统状态](CURRENT_STATUS.md) | 已实现能力、边界、代码依据与验证范围 |
| [环境安装](../ENVIRONMENT_SETUP.md) | Windows 依赖、启动与首次配置 |
| [管理与配置](MANAGEMENT.md) | 模型契约、来源、版本、生效与回退 |
| [接口](API.md) | 对外接口与内部计算边界 |
| [工作区](UI_WORKSPACE.md) | 四页交互、选区、结果口径与预览限制 |
| [本地影像](LOCAL_IMAGERY.md) | 检测输入、目录索引与同月拼接 |
| [行政区与兼容显示源](imagery-integration.md) | 区域键、边界及年度/外部显示接口 |
| [检测进度](detection-progress.md) | 权重、阶段、持久化与失败语义 |
| [瓦片加载](tile-loading.md) | 渲染、取消、重试、缓存与默认值 |
| [开发与测试](DEVELOPMENT.md) | 代码入口、验证命令与修改约束 |
| [前端规范](FRONTEND_STANDARDS.md) / [协作约定](../frontend/AGENTS.md) | 状态、生命周期、类型与清理规则 |
| [模型运行组件](../model-runtime/README.md) | 平台调用及独立 CLI 的能力边界 |
| [容器部署](../deploy/README.md) | 部署配方与待实测步骤 |
| [开发待办](ROADMAP.md) | 候选工作、待用户决策项与拟定验收条件 |
| [AI 助手能力演进 — TODO](ai-assistant-capability-todo.md) | 已确认目标、能力缺口、候选实现与 TODO；不代表已上线能力 |

文档冲突应回到代码、配置 schema 和运行诊断核对，不用旧报告推断当前默认值。持久化业务配置以当前生效版本为准，文档默认值仅表示新建配置或首次迁移的值。代码已实现不等于所有部署环境都已验收。

## 历史记录

旧版根目录、docs、部署和模型 Markdown 全部存入 [归档目录](archive/README.md)，不作为当前操作指南。其中阶段报告保留当时测试数量、性能样本和依赖审计结果，不滚动当作最新结论。

[migration](migration/README.md) 的 JSON 为原始迁移证据，保持原样。历史清单引用已删除文件时，不据此恢复旧功能或修改证据。`deploy/environment-observed-windows.txt` 是迁移环境快照；安装使用当前锁文件。

`data/validation` 是本机测试产物，通常不进入 Git；报告只证明各自范围，缺少本地报告不表示源码缺失。本轮文档整理没有重新执行产品验收。

## 后续维护

实现变更时同步更新对应主题与当前状态；待办附实现及验收证据后再移入已实现内容。未来事项须标为“已确认”或“待决策”，用户未选定的候选项不构成开发承诺。
