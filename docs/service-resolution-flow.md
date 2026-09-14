# Service 查找与隔离作用域流程

```text
【作用域标签】

默认 Context
  └─ isolateLabel("db") = "db"

ctx.Isolate("db")
  └─ 创建唯一 label："db#<scope seq>"
       └─ 该子树中的 db 与父级/兄弟 scope 隔离

ctx.IsolateShared("db", "tenant-a")
  └─ isolateLabel("db") = "tenant-a"
       └─ 使用相同 name + label 的 Context 共享同一服务槽位

isolateLabel(name)
  └─ 从当前 Context 沿 parent 向上查找第一个 name 映射
       └─ 未找到时退回服务名本身


【服务注册】

ctx.Provide(name, service)
  └─ provide(ctx, name, service, availabilityCheck=nil)
       ├─ scopeLabel = ctx.isolateLabel(name)
       ├─ 创建 serviceBinding
       │    ├─ name
       │    ├─ scopeLabel
       │    ├─ provider = ctx.fiber
       │    ├─ service
       │    └─ seq = 全局递增注册序号
       └─ 作为 Effect 注册
            ├─ registerService(binding)
            │    └─ serviceBindings[scopeLabel] = binding
            └─ disposer
                 ├─ unregisterService(binding)
                 └─ notify(name, scopeLabel)

一个 scopeLabel 只有一个槽位
  ├─ 已有相同服务名   → SERVICE_EXISTS
  └─ 已有其他服务名   → 同样冲突
       └─ 共享 label 必须保持“一个 label 对应一个服务名”


【普通实时查找】

ctx.Lookup/Get(name)
  └─ Context.resolveService(name)
       ├─ scopeLabel = ctx.isolateLabel(name)
       ├─ 先查当前 Fiber generation 的 resolvedServices[name]
       │    ├─ binding.name == name
       │    ├─ binding.scopeLabel == scopeLabel
       │    └─ availabilityCheck 通过
       │         └─ 返回 pinned binding
       │
       ├─ 未命中时沿 Fiber.Parent 向上查 snapshot
       │    ├─ 父 Context 仍处于相同 scope → 继续
       │    └─ scope 改变 / 回到同一 Fiber → 停止
       │
       └─ 最后查 live service registry
            ├─ name 必须匹配
            ├─ Provider 必须 StateActive
            └─ availabilityCheck 必须通过

Lookup(name)
  └─ 返回 any

Get[T](name)
  └─ 在 Lookup 语义上再做 T 类型断言

MustGet[T](name)
  └─ 不可用/类型不符 → panic(SERVICE_MISSING)


【为什么先查 generation snapshot】

Consumer load
  └─ resolveInjections() 固定 bindings
       └─ f.resolvedServices = bindings
            └─ 本代 Consumer 始终读取自己加载时绑定的 Provider

Provider 正在 unloading
  ├─ live registry 已拒绝该 Provider
  └─ 已运行 Consumer 的 snapshot 暂时仍可读
       └─ Provider 状态变化同时触发 Consumer.refresh()
            └─ Consumer 卸载完成后清空 snapshot

结果
  └─ 一个 generation 不会在执行中静默切换到另一 Provider


【动态可用性】

ctx.ProvideChecked(name, service, check)
  └─ 每次解析/读取调用 binding.available()
       ├─ check() == true  → 可用
       ├─ check() == false → 当作服务缺失
       └─ check() panic
            ├─ 当作不可用
            └─ 每个 binding 只记录一次 panic 日志

resolveInjections() 后、插件体执行前
  └─ load() 再调用 binding.live() 做二次存活检查
       ├─ 仍可用 → 提交 epoch + snapshot，运行插件体
       └─ 已失效 → 放弃本代，回到 pending，再 refresh

availabilityCheck 自身状态变化
  └─ 不会主动发送 notify
       └─ 需要后续服务注册/生命周期变化等触发 refresh 后才会重新判定


【Set 与重新 Provide】

ctx.Set(name, newService)
  ├─ 只允许 binding.provider == 当前 Fiber
  ├─ 原地替换 binding.service
  ├─ binding.seq 不变
  └─ notify dependents
       └─ dependent epoch 未变 → 不 reload

service disposer + 再次 Provide
  ├─ 旧 binding 被移除
  ├─ 新 binding 获得新 seq
  └─ dependent epoch 改变 → reload


【Serve 生命周期】

ctx.Serve(name, service)
  └─ 创建外层 Serve Effect
       ├─ 在其 scope 内 Provide(name, service)
       ├─ service 实现 Starter → Start()
       │    └─ 失败 → 回滚整个 Effect 和服务注册
       └─ service 实现 Stopper → 外层 disposer 调 Stop()

卸载顺序
  └─ 外层 Serve disposer：Stop()
       └─ 再清理内层 Provide child：unregisterService()


【核心关系】

服务身份
  └─ name + scopeLabel 定位；provider UID + binding seq 标识 generation

读取稳定性
  └─ 已激活 Fiber 优先读取 pinned snapshot

对外可见性
  └─ live registry 只暴露 active Provider

隔离
  └─ Context 树决定 scope label，Fiber 父子关系不等于服务可见关系

核心实现
  ├─ context.go  → isolateLabel、resolveService、Get/Lookup/Serve
  ├─ service.go  → serviceBinding、注册、可用性、notify
  └─ fiber.go    → resolvedServices snapshot、epoch
```
