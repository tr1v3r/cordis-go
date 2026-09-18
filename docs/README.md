# 核心工作流索引

```text
【架构总览】

cordis-go-architecture.html
  └─ 组件地图：Context/Fiber/Effect/服务/事件 + loader 装配
     （可编辑源：cordis-go-architecture.json，改后需重新生成 HTML）


【生命周期与依赖】

fiber-dependency-flow.md
  └─ internal/plugin → Fiber 状态 → service notify → 下游 reload

effect-lifecycle-flow.md
  └─ effect ownership → 注册 → disposer → LIFO/嵌套回收

fiber-recovery-flow.md
  └─ load 失败 → 清理 → failed → Restart/Update → 并发收敛

application-shutdown-flow.md
  └─ base context/root Dispose → 递归拆除 Fiber 与 Effect 树


【服务与事件】

service-resolution-flow.md
  └─ isolation label → binding → snapshot/live registry → Set/rebind

event-dispatch-flow.md
  └─ listener effect → scope 过滤 → Emit/Bail/Parallel/Waterfall/Once


【装配与实例】

loader-compose-flow.md
  └─ JSON layer → patch compose → 类型固定闭包 → Tree.Load/回滚

runtime-registry-flow.md
  └─ definition identity → shared runtime → independent Fiber instances


【阅读顺序】

理解插件为什么启动/重载
  └─ fiber-dependency-flow
       → service-resolution-flow
       → effect-lifecycle-flow

理解事件为什么被调用/注销
  └─ event-dispatch-flow
       → effect-lifecycle-flow
       → application-shutdown-flow

理解配置如何变成运行实例
  └─ loader-compose-flow
       → runtime-registry-flow
       → fiber-recovery-flow
```
