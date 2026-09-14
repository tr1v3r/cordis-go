# 插件消息后的 Fiber 与依赖传播

```text
【插件加载 / internal/plugin】

Load()
  │  registry.go: load()
  ├─ 合并 Plugin.Inject + extra inject
  ├─ runtimeFor(definition)
  ├─ newFiber(...)                         // 初始：pending + epochInactive
  └─ fiber.start()
       │
       ├─ 挂到父 Fiber 的 "child" effect
       ├─ attachFiber(runtime, fiber)
       ├─ emitInternal("internal/plugin")  // 同步通知；此时通常仍是 pending
       └─ fiber.refresh()                  // 消息处理完后才开始依赖解析


【依赖解析 / Fiber 状态机】

fiber.refresh()
  └─ beginTransition()
       ├─ busy=false → 当前调用者负责迁移
       └─ busy=true  → dirty=true，当前迁移结束后再跑一轮
            │
            ▼
         fiber.sync()
            │
            ├─ resolveInjections()
            │    ├─ 按名字和 isolation scope 查找每个服务
            │    ├─ 任一服务不存在/Provider 非 active
            │    │    └─ epoch = epochInactive
            │    └─ 全部存在
            │         └─ epoch = ":<provider UID>.<binding seq>..."
            │
            └─ 比较新旧 epoch
                 ├─ new == old
                 │    └─ 不处理
                 ├─ new == epochInactive
                 │    └─ applyUnload()
                 ├─ new != old && old generation 仍 live
                 │    └─ reload() = unload → 重新解析 → load
                 └─ new != old && Fiber 当前干净
                      └─ applyLoad()


【Provider 提供服务后的传播】

Provider Apply()
  └─ ctx.Provide("db", service)
       ├─ registerService(binding)
       │    └─ binding = {
       │         name: "db",
       │         provider: Provider Fiber,
       │         seq: 本次注册唯一序号,
       │         scopeLabel: 当前隔离作用域
       │       }
       │
       ├─ Provider 仍是 loading
       │    └─ 暂不 notify，避免暴露未初始化完成的服务
       │
       └─ Provider Apply 成功
            └─ setState(active)
                 └─ notifyProvided()
                      └─ core.notify("db", scope)
                           ├─ 扫描所有 live Fiber
                           ├─ 筛选 inject("db") 且 scope 相同者
                           └─ consumer.refresh()
                                ├─ epochInactive → ":ProviderUID.BindingSeq"
                                ├─ consumer loading
                                ├─ 执行 Consumer Apply()
                                └─ consumer active
                                     └─ 若 Consumer 也 Provide 服务
                                          └─ 继续 notify 下游


【Provider 服务消失后的传播】

Provider active
  ├─ 服务 disposer 被调用
  │    ├─ unregisterService(binding)
  │    └─ notify("db", scope)
  │
  └─ 或 Provider active → unloading
       └─ setState() 检测 active 可用性发生变化
            └─ notifyProvided()
                 │
                 ▼
              consumer.refresh()
                 ├─ resolveInjections() → epochInactive
                 ├─ consumer active → unloading
                 ├─ 按 LIFO 回收 listener/service/child/disposer
                 └─ consumer → pending
                      └─ Consumer 提供的服务也变为不可用
                           └─ 继续通知更下游 Fiber


【父子 Fiber 生命周期传播】

父 Fiber unload
  └─ 倒序清理 effects
       └─ 执行 "child" effect disposer
            └─ child.disposeFromParent()
                 └─ child.Dispose()
                      ├─ 取消 child Context
                      ├─ 从 runtime 移除
                      ├─ emitInternal("internal/plugin")
                      ├─ child unloading
                      ├─ 倒序清理 child effects
                      └─ child disposed


【插件销毁 / internal/plugin】

Fiber.Dispose()
  ├─ disposed=true
  ├─ dirty=true
  ├─ cancel lifecycle Context
  ├─ close Done
  ├─ removeFiber(runtime, fiber)
  ├─ emitInternal("internal/plugin")       // 销毁开始；effects 尚可能未清完
  └─ refresh()
       └─ unloading → 清理 effects → disposed


【核心关系】

internal/plugin
  └─ 只负责同步通知“Fiber 创建/销毁”，不直接传播依赖

父子 Fiber
  └─ 通过 "child" effect 传播 Dispose，形成生命周期树

服务依赖
  └─ 服务出现/消失或 Provider active 状态变化
       → notify
       → dependent.refresh
       → 重新计算 epoch
       → load / unload / reload
       → 继续通知下游

epoch = provider UID + binding seq
  ├─ 换 Provider                      → epoch 变化 → reload
  ├─ 同一 Provider 解绑后重新 Provide → seq 变化   → reload
  └─ ctx.Set() 原地替换服务值         → seq 不变   → 不 reload
```
