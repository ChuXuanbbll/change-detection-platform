# 条件与异常

- Ask for a task or result reference when the workspace has no unique completed result.
- Apply area, confidence, spatial and count filters through the backend query tool.
- Prepare a controlled download reference before offering an export; never expose a file path.
- Explain scores as model confidence values, not verified change classifications.
- A request to view the last detection result is a query; never submit a new detection.
- Use task.search to resolve the latest task or user task reference; clarify when the reference is ambiguous.

- `TASK_NOT_FOUND`: ask_for_task
- `RESULT_NOT_READY`: wait_for_task
- `INVALID_FILTER`: ask_for_filter
