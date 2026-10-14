# Vue 前端开发约束

适用于本目录。详细规范见 [docs/FRONTEND_STANDARDS.md](../docs/FRONTEND_STANDARDS.md)。

- 修改前核对组件模板、调用方、共享工作区和相关测试；保留已有未提交修改。
- 默认保持四页路由、KeepAlive、共享检测批次、当前 DOM 层级、类名和可见 UI。不通过恢复已删除控件来消除死代码。
- 移除 UI 时同步检查独占的状态、方法、watch、监听和注释模板；仍由其他控件、调用方、defineExpose 或异步业务使用的代码必须保留。
- Vue 管理的元素用模板事件和有类型的 ref；只为 Leaflet 等库生成的元素和必要的全局事件使用原生监听。
- 每个监听、观察器、计时器和请求必须有明确所有者与清理位置；临时 DOM 重建前解绑，KeepAlive 停用时暂停页面快捷键和可见性相关工作，卸载时释放。
- 延迟回调必须检查目标仍存在且属于当前组件/请求；禁止在已移除 DOM 上创建地图、图表或提交过期结果。应用级检测轮询不随页面切换停止。
- 保持 TypeScript strict、noUnusedLocals、noUnusedParameters 和 Vue strictTemplates；不得用 any、ts-ignore 或关闭检查掩盖失效绑定。允许标准 data-* 属性。
- 不做无关的全量格式化、CSS 重写或依赖升级。沿用相邻代码风格和既有 Prettier 检查范围。
- 提交前运行 npm run build、npm run format:check 和相关既有回归；事件/地图生命周期修改需真实浏览器验证。只报告实际通过的检查。
