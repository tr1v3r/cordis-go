# Root 生命周期与应用关闭流程

```text
【应用初始化】

cordis.New(options...)
  ├─ 创建 core
  │    ├─ serviceBindings
  │    ├─ runtimes
  │    ├─ eventBus
  │    └─ loggerService
  ├─ 创建 root Context
  ├─ newRootFiber(rootCtx)
  │    ├─ UID=0
  │    ├─ StateActive
  │    ├─ live=true
  │    └─ lifecycleCtx 派生自 baseCtx 或 context.Background()
  └─ 通过普通 Provide 注册内置服务
       ├─ registry
       ├─ events
       └─ logger

内置服务也是 root Fiber effects
  └─ 应用关闭时和普通服务走同一套注销流程


【宿主 Context 接管生命周期】

New(WithBaseContext(hostCtx))
  └─ context.AfterFunc(hostCtx, rootFiber.Dispose)
       └─ stopBase disposer 作为 root Effect 注册

hostCtx
  ├─ value / deadline 被所有子 Fiber lifecycleCtx 继承
  └─ Done 触发
       └─ 等价于 root.Fiber().Dispose()

注意
  └─ request-scoped context 不应作为 baseCtx
       └─ 请求超时会拆掉整个应用


【插件树挂载】

rootCtx.Load(plugin)
  └─ plugin Fiber 作为 root 的 "child" Effect
       └─ pluginCtx.Load(child)
            └─ child Fiber 再作为 plugin Fiber 的 "child" Effect

形成两棵相关结构
  ├─ Context/Fiber 生命周期树
  └─ 每个 Fiber 内部的 Effect 树

依赖关系另由 service inject 构成
  └─ 不要求与生命周期父子树一致


【根关闭入口】

rootFiber.Dispose()
  ├─ disposed=true + dirty=true
  ├─ cancel(root lifecycleCtx)
  │    └─ Go context 取消向所有子 Fiber/goroutine 传播
  ├─ close(root.Done)
  ├─ root 没有 runtime
  │    └─ 不发送 internal/plugin，也无需从 registry 移除
  └─ root 特殊路径：直接 root.unload()

取消先发生、Effect 回收后发生
  └─ goroutine 可立刻观察 Context.Done
       └─ services/listeners/children 随后按生命周期顺序拆除


【整棵树回收】

root.unload()
  ├─ root active → unloading
  │    └─ notifyProvided()：内置服务开始不可用
  ├─ root.disposables.clear()                     // 顶层 Effect 逆注册顺序
  └─ 逐个 runDisposer
       ├─ 用户后加载的 child Fiber 通常先销毁
       │    └─ child.Dispose()
       │         └─ 递归清理它的 effects 和后代
       ├─ root 上注册的 listeners/services 被注销
       └─ 内置 logger/events/registry 最终被注销
            ↓
         root resolvedServices/config 清空
            ↓
         finalizeDispose()
            └─ StateDisposed


【子 Fiber Dispose】

父 Effect 执行 child disposer
  └─ child.disposeFromParent()
       ├─ 清掉 parentEffectDisposer，避免回收环
       └─ child.Dispose()
            ├─ cancel child lifecycleCtx
            ├─ close child.Done
            ├─ removeFiber(runtime, child)
            ├─ emit internal/plugin                     // effects unwind 前
            └─ refresh → unloading → disposed

子 Fiber 自己提前 Dispose
  └─ finalizeDispose() 调 parentEffectDisposer()
       └─ 从父 Effect 列表摘除自己的 child entry


【依赖传播与关闭交织】

Provider active → unloading
  └─ setState 检测 active 可用性改变
       └─ notifyProvided()
            └─ 依赖者 refresh → unloading/pending

随后父子生命周期继续 Dispose
  └─ 已卸载的 dependent 再收到 Dispose
       └─ unload 幂等，直接 finalizeDispose

结果
  └─ 无论先由依赖失效卸载，还是先由父树 Dispose
       └─ effects、services、runtime 最终只释放一次


【完成语义】

rootFiber.Dispose()
  └─ root 直接执行 unload，调用返回时完成根回收

普通 Fiber.Dispose()
  └─ 若该 Fiber 正在 transition
       └─ 调用返回时可能尚未完成 effect teardown

观察普通 Fiber 终态
  └─ Fiber.State() == StateDisposed
       └─ 没有独立的“等待完全销毁”API


【核心不变量】

baseCtx cancel
  └─ 必须等价于 root.Dispose，而不是只取消 goroutine

root Dispose
  └─ 必须通过 Effect 树递归拆除服务、监听器和子 Fiber

依赖 unload
  └─ 不取消 Fiber lifecycleCtx

永久 Dispose
  └─ 取消 lifecycleCtx，并进入不可重启的 StateDisposed

核心实现
  ├─ context.go      → New、WithBaseContext、内置服务、Context tree
  ├─ fiber.go        → newRootFiber、Dispose、unload、disposeFromParent
  └─ disposable.go   → Effect 树与逆序回收
```
