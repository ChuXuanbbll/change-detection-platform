---
name: imagery-exploration
description: Browse imagery, phases, prepared images and models, or locate a region
  on the workspace map without starting detection.
---

# 影像浏览

读取 `workspace.context`，只查询本次问题需要的目录、影像或模型。已知区域无需重新定位；查模型只需 `model.list`，查已准备影像只需 `image.list`。

需要选择行政区时查询 `region.search`，保留完整行政路径并消除重名歧义。加载时相前查询 `imagery.scenes`，只加载用户指定的时相；UI 回执中的 context_revision 是下一操作的版本。

不启动检测。用户需要执行检测时选用变化检测 Skill。仅在目录缺失、日期精度或边界异常时读取 [边界与异常](references/recovery.md)。
