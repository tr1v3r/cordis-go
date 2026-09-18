# Runtime Registry 与插件多实例流程

```text
【三层身份】

Plugin[C] definition 指针
  └─ 描述插件代码、配置类型、Inject、Validate、Apply
       ↓  按指针身份索引
runtime
  └─ 同一个 definition 在应用内共享的注册记录
       ├─ name
       ├─ definition
       ├─ claims
       └─ fibers[]
            ↓  每次 Load 创建一个
Fiber
  └─ 一次独立插件实例
       ├─ 唯一 UID
       ├─ 独立 config / epoch / state
       ├─ 独立 Context
       └─ 独立 effects / resolvedServices

结论
  └─ 同一 Plugin 指针 Load 两次 = 一个 runtime + 两个独立 Fiber


【Load 取得 runtime】

load(parentCtx, definition, config)
  ├─ definition 必须是非 nil、可比较的指针
  └─ core.runtimeFor(definition)
       ├─ runtimes[definition] 已存在 → 复用 runtime
       ├─ 不存在 → 创建并登记 runtime
       └─ runtime.claims++                              // 为在途 Load 占位
            ↓
         newFiber(...)
            ↓
         fiber.start()

claims 的作用
  └─ Fiber 尚未 attach 时，阻止另一个并发失败 Load 提前删除 runtime


【Fiber 发布】

fiber.start()
  └─ 父 Fiber.tryEffect(label="child")
       └─ effect body
            ├─ core.attachFiber(runtime, fiber)
            │    ├─ runtime.fibers append fiber
            │    └─ runtime.claims--
            ├─ emit internal/plugin
            └─ 返回 child disposer
                 └─ 父生命周期结束时调用 fiber.Dispose()

发布完成后
  └─ runtime 对外可通过内置 Registry 观察
       ├─ Registry.Size()    → runtime 数量
       └─ Registry.Plugins() → definition/plugin 名称

Registry 统计的是 definition/runtime
  ├─ 同一插件指针的多个 Fiber 不会重复增加 Size()
  └─ 同名但不同 definition 指针会形成不同 runtime
       └─ Plugins() 可能出现重复名称，且顺序不保证稳定


【start 失败时归还 claim】

fiber.start() 无法挂到父生命周期
  ├─ fiber.cancel()
  └─ core.discardRuntime(runtime)
       ├─ claims--
       └─ claims==0 && fibers 为空
            └─ 删除 runtimes[definition]

结果
  └─ 未成功发布 Fiber 的 Load 不会留下 phantom runtime


【单实例销毁】

fiber.Dispose()
  ├─ core.removeFiber(runtime, fiber)
  │    ├─ 只从 runtime.fibers 删除当前 Fiber
  │    └─ 若 claims==0 && fibers 为空 → 删除 runtime
  ├─ emit internal/plugin
  └─ unload 当前 Fiber 的独立 effects

两个 Fiber 共用 runtime
  ├─ fiber1.Dispose() → fiber2 继续 active，runtime 保留
  └─ fiber2.Dispose() → 最后实例消失，runtime 删除


【插件运行失败与 runtime】

Plugin body / config 失败
  └─ Load 返回 failed Fiber + error
       ├─ Fiber 已成功 attach
       └─ runtime 仍保留

直接调用方
  └─ 可检查 Fiber.Error()、Restart()、Update() 或 Dispose()

Loader 调用方
  └─ Tree.Load 将失败 Fiber Dispose，并回滚此前 fibers


【definition 擦除边界】

Plugin[C]
  └─ 实现内部非导出 definition
       ├─ PluginName()
       ├─ InjectKeys()
       ├─ ResolveConfig(any)
       └─ Run(ctx, any)

Fiber runtime
  └─ 只通过 definition 调 ResolveConfig/Run
       └─ 不认识具体 C

直接类型化入口
  └─ Context.Load[C](plugin, config) 在编译期保证 config 类型

运行期宿主（loader）
  └─ Register[C] 闭包在注册时固定 C
       └─ 不导出 definition，也不在加载时动态实例化泛型


【核心不变量】

definition identity
  └─ 决定 runtime 是否复用

Load invocation
  └─ 决定 Fiber 实例数量

claims + fibers
  └─ 共同决定 runtime 是否仍属于 registry

Fiber ownership
  └─ 每个实例独立卸载，不影响同 runtime 的兄弟实例

核心实现
  ├─ registry.go                  → definition、runtimeFor、discardRuntime、load
  ├─ fiber.go                     → attachFiber、removeFiber、Fiber.Dispose
  └─ integration_runtime_test.go  → 同 definition 双实例与 registry 生命周期
```
