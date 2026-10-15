---
name: result-inspection
description: Query existing tasks and their status, inspect completed results, filter
  patches, and prepare a controlled export without creating a detection.
---

# 检测结果

先读 `workspace.context`。根据明确 task_id 或 `task.search` 验证的唯一引用查询状态、摘要或图斑；“上次结果”先通过 task.search 解析。只执行用户要求的操作，不创建新检测。

状态查询返回当前状态即可。图斑筛选使用后端过滤字段；导出仅在用户要求时调用 `result.export.prepare` 并返回受控下载引用。分数表示模型置信度，不能称为已确认的违法或地物分类。

引用不唯一时调用 request_clarification。仅在结果未就绪或导出问题时读取 [结果异常](references/recovery.md)。
