# Effect 注册与回收流程

```text
【所有权入口】

插件原始 ctx
  └─ effectOwner == nil
       └─ On / Provide / Serve / Load / OnDispose / Effect
            └─ 注册到 Fiber.disposables                     // Fiber 级

ctx.Effect(label, body)
  └─ 创建派生 scope：scope.effectOwner = 当前 effectEntry
       └─ body(scope) 内通过 scope 注册的副作用
            └─ 注册到 effectEntry.children                 // Effect 级

body 内若仍使用原始 ctx
  └─ 不继承 scope.effectOwner
       └─ 副作用仍与外层 Effect 平级，归 Fiber 所有


【注册主链】

Context.effect(label, body)
  └─ Fiber.effect(owner, label, body)
       └─ Fiber.tryEffect(owner, label, body)
            │
            ├─ Fiber disposed / unloading
            │    └─ INACTIVE_EFFECT
            │
            ├─ 创建 effectEntry{running:true}
            ├─ owner != nil
            │    └─ owner.adopt(entry)
            │         └─ 仅 owner body 尚在运行时允许收养
            ├─ owner == nil
            │    └─ Fiber.disposables.add(entry)
            │
            ├─ 先发布 entry，再执行 body(entry)
            ├─ entry.finishBody()                           // scope 自此禁止新注册
            └─ entry.setDispose(body 返回的 disposer)
                 ├─ 无并发销毁请求 → 返回幂等释放句柄
                 └─ body 运行期间已收到销毁请求
                      └─ 立即 runDisposer(entry)

先发布、后执行 body
  └─ 并发 unload 一定能看见 entry
       └─ disposer 尚未产生时先记 deferred
            └─ body 返回并安装 disposer 后补做回收

Effect body panic
  ├─ 从 owner/Fiber 列表移除当前 entry
  ├─ 回收 panic 前已收养的 children
  └─ 原 panic 继续向调用方抛出


【普通主动释放】

调用 Effect/On/Provide 返回的 disposer
  └─ Once(...)
       └─ Fiber.runDisposer(entry)
            ├─ requestDispose()
            ├─ releaseOnce.Do(...)
            ├─ claimDispose()：标记 done，取出 disposer + children
            ├─ 先执行当前 entry 的 disposer
            └─ 再倒序执行 children                          // newest first
                 └─ 从 owner/Fiber 的 effect 列表摘除 entry

多 goroutine 同时释放同一 entry
  └─ 全部汇合到 releaseOnce
       └─ disposer 和 children 只执行一次


【Fiber 卸载回收】

Fiber.unload()
  ├─ live=false
  ├─ StateUnloading
  ├─ Fiber.disposables.clear()
  │    ├─ 清空 Fiber 顶层列表
  │    └─ 返回顶层 entries 的逆注册顺序
  ├─ 逐个 runDisposer(entry)
  │    └─ owner disposer → nested children 逆序回收
  ├─ resolvedServices=nil
  ├─ config=nil
  └─ pending / disposed

最终顺序
  └─ Fiber 顶层：后注册先释放
       └─ 单个 Effect：先释放自己，再释放 children（children 后注册先释放）


【注册 API 与实际 Effect】

ctx.On(...)          → label=ctx.On("...")       → disposer 移除 listener
ctx.Provide(...)     → label=ctx.Provide("...")  → disposer 注销 service
ctx.Serve(...)       → 外层 Serve effect
  ├─ 内层 Provide effect                           → 注销 service
  └─ 外层 disposer                                 → Stop()
       └─ 实际回收：Stop() → 内层 Provide 注销
ctx.Load(...)        → label=child                 → disposer 调用 child.Dispose()
ctx.OnDispose(fn)    → label=anonymous             → disposer 调用 fn


【关键边界】

派生 scope 只在 Effect body 返回前有效
  └─ 返回后再用 scope 注册 → INACTIVE_EFFECT

ctx.Context()/ctx.Done()
  └─ 表示整个 Fiber 生命周期
       └─ 依赖 reload 不会取消它
            └─ 每代资源必须通过 Effect/OnDispose 自行回收

cleanup 不能形成 disposer 调用环
  └─ 自己调用自己 / 调用祖先 / 平级互相调用
       └─ 会像递归 sync.Once.Do 一样死锁

核心实现
  ├─ context.go     → Effect、OnDispose、effectOwner
  ├─ fiber.go       → tryEffect、runDisposer、unload
  └─ disposable.go  → effectEntry、disposableList、Once
```
