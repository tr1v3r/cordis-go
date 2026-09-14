# Event 注册与分发流程

```text
【监听器注册】

ctx.On / OnValue / OnWaterfall / OnOnce
  └─ Context.on(name, adapter, options)
       ├─ 创建 eventListener
       │    ├─ ctx       // 注册上下文，用于 scope 匹配
       │    ├─ fn
       │    ├─ prepend
       │    ├─ global
       │    └─ once
       └─ c.effect("ctx.On(name)", ...)
            ├─ eventBus.add(listener)
            │    ├─ Prepend → 插到 hooks[name] 头部
            │    └─ 默认    → 追加到尾部
            └─ disposer → eventBus.remove(listener)

监听器生命周期
  └─ 由注册时 Context 的 effectOwner 决定
       ├─ 插件原始 ctx → 跟随 Fiber generation
       └─ Effect scope → 跟随该 Effect


【监听器选择】

事件分发
  └─ eventBus.snapshot(name)                         // 复制当前监听器切片
       └─ selectListeners(name, filter)
            ├─ 普通版本 → 全部保留
            └─ Scoped 版本
                 ├─ emitter.isolateLabel(scopeName)
                 ├─ listener.ctx.isolateLabel(scopeName)
                 └─ label 相同 OR listener.global → 保留

快照语义
  └─ 本次分发使用开始时的监听器快照
       └─ 分发期间 add/remove 不改变已取得的列表


【统一调用边界】

eventBus.invoke(listener, payload)
  ├─ once listener：atomic CompareAndSwap 抢占执行权
  │    └─ 并发分发最多一个调用者成功
  ├─ payload 在 adapter 内 assertPayload[E]
  │    └─ 类型不匹配 → panic
  ├─ 执行 listener.fn(...)
  ├─ panic → 转成 error 并记录日志
  └─ once listener 无论成功或 panic
       └─ releaseOnce()
            ├─ 从 eventBus 删除
            └─ 释放对应 Effect entry


【分发模式】

Emit(name, payload)
  └─ 按监听器顺序逐个 invoke
       ├─ 忽略返回值
       └─ 单个 listener panic 被记录，继续后续监听器

Bail(name, payload)
  └─ 按顺序 invoke
       ├─ 普通 On 固定返回 nil；参与短路必须使用 OnValue
       ├─ result == nil / false → 继续
       └─ 首个非 nil 且非 false → 立即返回(result, true)

Serial(name, payload)
  └─ 直接复用 Bail                            // Go 中没有异步 await 差异

Parallel(name, payload)
  ├─ 每个 listener 启动一个 goroutine
  ├─ WaitGroup 等待全部完成
  └─ errors.Join 汇总 panic 转成的 errors

Waterfall(name, payload, final)
  ├─ 从后向前构建 waterfallStep 链
  ├─ listener(value, next)
  │    ├─ 调 next(newValue) → 进入下一层
  │    └─ 不调 next         → 截断后续链
  ├─ 最尾部执行 final(value)
  └─ 最外层 listener 的返回值成为最终结果


【Waterfall 单次收敛】

waterfallStep.call(value)
  ├─ 第一次调用
  │    ├─ started=true
  │    ├─ 执行 listener/final
  │    ├─ 保存 result
  │    └─ finished=true + close(done)
  ├─ 执行中再次调用同一个 next
  │    └─ 等待 done，复用第一次结果
  └─ 完成后再次调用
       └─ 直接返回缓存结果

listener panic
  └─ 记录错误并继续下一 waterfall step

final panic
  └─ 记录错误并返回 nil


【内部事件】

emitInternal("internal/plugin" / "internal/status", payload)
  └─ snapshot → 顺序 invoke
       ├─ 同步执行
       ├─ 不做 isolation scope 过滤
       └─ listener panic 只记日志，不中断框架状态迁移


【核心关系】

注册
  └─ Listener 是 Effect，因此 Fiber unload 会自动注销

过滤
  └─ Scoped 按 isolation label；Global 绕过过滤

并发
  ├─ 普通 Emit/Bail/Waterfall 同步
  ├─ Parallel 主动并发
  └─ Once 使用原子 claim 保证全局最多执行一次

核心实现
  ├─ events.go     → eventBus、各分发模式、once/waterfall
  ├─ context.go    → Context 与 effect ownership
  └─ funcforms.go  → 等价包级函数入口
```
