# AI 助手检测链路 Review 与修复

## 现象与证据

本次读取了本地前端对应的 AI session 和 workflow 记录。用户要求“我画框区域，执行检测 2025-9/2025-3 两个时相对比”，当时已有区域和 Polygon AOI。一次执行产生 26 次模型工具请求，最终任务确实成功，但编排中存在多次失败和无效调用：

- 意图只保留了 March–September 的 date_range，目录返回 3、5、7、9 月，模型将四期全部加载。用户明确的两期选择语义丢失。
- 同一区域被再次选择；后续加载继续使用旧版本，因此 UI_CONTEXT_STALE。load_phases 的手动 bump 与异步 watch 又造成无用户操作的第二次版本变化，刚返回的版本随即过期。
- 模型为检测调用地图定位，AOI bounds 与“行政区完整 bounds”前置约束冲突，产生无关失败。
- 模型需要手动编排 plan、prepare、operation.status、submit；prepare 已经等待终态，模型仍再次查询状态。用尽 24 次工具预算后又尝试两次 task.status。
- UI 失败被折叠为通用且可重试的 UI_ACTION_FAILED，成功回执又缺最新版本和工作区，模型难以修正下一步。
- 最后一步仅凭模型文本就可结束为成功，未核对本轮检测是否真的提交。

这些问题同时涉及业务入口、状态协议、意图表示和 Skill 包装；只补提示词不能消除版本错误，也不能保证只比较两期。

## 已实施

1. 三个 Skill 改为独立标准目录：SKILL.md 元数据与正文、references 条件参考、runtime.yaml 平台权限/schema 扩展。Go 只负责读取和校验，不承载长篇 Skill 指令。支持 AI_SKILLS_DIR 启动加载外部分发包，修改后重启即可生效；默认使用嵌入版本。
2. 真正分阶段披露：metadata selection → 选中正文与槽位 schema → 执行时按需 read_reference。参考读取限定当前包，无任意文件或跨包访问。选择阶段增加一次模型请求，以降低后续无关上下文和复杂调用；实际总耗时/费用仍需真实模型测量。
3. 新增 ui.submit_detection，与页面右下角“提交检测任务”按钮共同调用 useWorkspace.submitDetection。服务端将本轮已验证的日期、区域、模型和参数槽位绑定进命令，执行模型只需提供 expected_revision。
4. 两个明确日期保留为 phase_dates。前端从当前区域目录精确选出对应两期，替换此前多余时相，沿用当前 AOI 和未覆盖的面板参数。日期缺失、资源冲突或配置无效时停止，不悄悄换资源。
5. 同步观察器统一管理工作区版本，去除命令中的重复 bump；无变化操作保留版本。参数变更同样使旧版本失效。成功/失败工具回复携带最新 workspace、context_revision 和实际错误码。
6. 检测 Skill 不再暴露计划/准备/提交底层链或地图飞行；就绪工作区正常只需 context → submit_detection。提交成功返回首个 task_id 与 pair_count，工作区继续管理后续时相组和进度，AI 不额外轮询或宣称推理已完成。
7. 同一操作的成功、待确认或失败提交均不重复发起；拒绝的过期命令可核对新状态后修正。成功回执可幂等重传，冲突回执被拒绝。达到预算立即停止并修复未配对工具消息；无任务引用的检测文本不能标记为成功。
8. 新对话发生在影像准备期间时，准备完成后不会再创建旧会话的任务；已经提交的任务继续由工作区管理。

## 验证边界

- Go 全量测试与 AI/API go vet；关键新增路径通过 race 检查。
- 前端 TypeScript 与生产构建、既有格式检查、功能回归。
- 三个 Skill 通过 skill-creator quick_validate。
- 隔离 Chrome 验收涵盖四时相选择两期、回执版本连续使用、无变化操作、面板参数过期拒绝、AI/手动按钮参数一致、重复任务拦截及既有 SSE/导航/新对话回归。报告位于 data/validation/ai-browser/report.json。
- 验收使用确定性 HTTP 和模型夹具，没有调用真实云端模型或运行真实推理。因此不声称实际 token、耗时、费用或真实模型选 Skill 准确率已改善；这些需在重新启动应用后单独进行真实请求验收。

格式依据：[Agent Skills / Build skills](https://learn.chatgpt.com/docs/build-skills)。实现说明见 [ai-tool-use.md](ai-tool-use.md)。
