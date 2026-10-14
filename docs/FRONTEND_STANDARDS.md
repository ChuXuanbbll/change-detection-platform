# Vue 前端开发规范

按 2026-10-12 的前端栈、类型配置与测试入口整理。本文约束实现维护；用户确认的新需求可改变产品行为，但需同步相关说明与验收。

## 维护边界

本项目采用 Vue 3、TypeScript、Vue Router、Leaflet 和 ECharts。四个页面通过 KeepAlive 缓存，检测批次由 App 提供的工作区管理。清理以保留现有行为和可见 UI 为验收标准；不得改变路由、组件层级、布局类名、地理坐标约定或检测接口来简化清理。

先阅读模板、调用方、props/emits、defineExpose、watch 和相关测试，再决定是否删除。未被当前页面直接调用不等于无用：重试按钮、其他分支、地图插件、外部组件引用和共享批次都可能是有效调用方。禁止批量按名字匹配删除函数或 CSS。

## 模板与状态

- 元素通过有类型的模板 ref 访问，事件优先使用 @click、@change 等 Vue 指令，不用全局选择器寻找组件内部节点。
- 移除控件时清理其独占的状态、computed、watch、事件处理和导入；删除注释掉的旧模板，历史由 Git 保留。
- 条件渲染的 ref 必须允许 null。等待 nextTick 后重新检查 ref；对地图、图表等实例，还需检查 isConnected 和可见尺寸。
- 错误必须有有效消费方。删除状态栏时，将仍需报告的失败接入已有错误提示，不保留只写不读的响应式状态或监听。
- props 与 emits 使用明确类型。保留 defineExpose 的实际调用接口，模板与组件事件由 strictTemplates 检查。

## 生命周期与资源所有权

| 资源 | 建立/恢复 | 暂停/释放 |
| --- | --- | --- |
| Vue 模板事件、同步建立的 watch | setup / 模板 | 由 Vue 随组件销毁；KeepAlive 下 watch 仍可能运行 |
| 页面全局快捷键 | mounted / activated，同一回调避免重复绑定 | deactivated 与 unmounted 移除；回调另检查地图可见性 |
| 临时边编辑 DOM 的原生监听 | 每次创建编辑图层 | 重建、取消、清除、卸载时用 AbortController 释放 |
| ResizeObserver、地图、图表 | mounted；activated 校正尺寸和内容 | 页面隐藏时避免操作分离 DOM；卸载断开观察器并清空实例引用 |
| 选区尺寸等页面请求和计时器 | 当前可见选区 | 选区变化、停用、卸载时取消；过期响应不得更新旧节点 |
| 共享检测任务和结果 | App 工作区 | 保持跨页面运行；由工作区重置/卸载取消，不能由视图停用终止 |

原生监听须保存同一回调或使用 AbortSignal；Leaflet 事件由对应图层/地图持有，必要时 off，最终由 remove/dispose 释放。清理第三方实例后将引用置空，防止排队的 nextTick 或观察器回调继续使用已销毁实例。异步请求用 AbortSignal、请求身份或 generation 检查阻止过期写入。

## 检查与风格

沿用相邻代码风格，避免将全量格式化混入功能清理。既有 Prettier 检查仍覆盖管理模块；扩展检查范围应单独处理。TypeScript 开启 strict、noUnusedLocals、noUnusedParameters，Vue 开启 strictTemplates；data-* 为标准 HTML 扩展属性。禁止以宽泛类型或关闭检查规避错误。

在 frontend 目录执行：

```powershell
npm run typecheck
npm run build
npm run format:check
```

缺少依赖时先 npm ci。在项目根目录执行现有功能回归：

```powershell
node --test scripts/test_frontend_workflow.mjs scripts/test_province.mjs scripts/test_region_view.mjs scripts/test_selection_geometry.mjs
```

地图编辑和 KeepAlive 生命周期的改动还需执行真实浏览器回归：

```powershell
$env:PLAYWRIGHT_MODULE = '<已安装的 playwright 模块绝对路径>'
$env:FRONTEND_URL = 'http://127.0.0.1:5173'
node scripts/test_polygon_browser.mjs
node scripts/test_workspace_browser.mjs
```

使用确定性 API 夹具或隔离服务，不对开发实例创建真实任务。额外 Vite 实例须设置独立 VITE_CACHE_DIR。验收检查四页导航、切页与返回、选区与编辑、检测与结果、预览与图表、分页与导出及桌面/移动布局；按实际改动选择相关测试。交付区分静态检查、夹具浏览器测试与真实后端/推理验证。
