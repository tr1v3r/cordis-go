# FLOW — cordis-go 组件总运行图

组件边界：fiber 生命周期、effect tree、服务依赖解析、disposables、loader 装配。
子流程深潜图已存在于 docs/（A 级，直接引用，此处不重复）：
fiber-dependency-flow / effect-lifecycle-flow / fiber-recovery-flow /
application-shutdown-flow / service-resolution-flow / event-dispatch-flow /
loader-compose-flow / runtime-registry-flow。
本文是全链路骨架：一个插件从装载到销毁，含主路径与异常路径。
记录日期 2026-10-12，依据代码现状（fiber.go / context.go / service.go /
disposable.go / registry.go / loader/loader.go）。

```text
【装载入口 / registry.go load()】
ctx.Load(plugin, config)              ← 同步：返回时 fiber 已 settled
  ├─ 合并 Plugin.Inject + extra inject
  ├─ runtimeFor(definition)           ← definition 指针身份 → 共享 runtime
  │    └─ claims++（加载中占位；失败归还，防幻影 runtime）
  └─ fiber.start()
       ├─ 父 fiber 已 unloading/disposed
       │    └─ 归还 claim，返回错误                    ← 异常：不留孤儿 fiber
       ├─ 挂到父的 "child" effect → attachFiber
       │    → emitInternal("internal/plugin")（同步，此时多仍 pending）
       └─ fiber.refresh()

【依赖解析与状态迁移 / fiber.go refresh→drive→sync】
beginTransition：busy 空则自己驱动；busy 被占则 dirty=true，由现主再跑一轮
sync 消费请求（优先级从高到低）：
  ├─ disposed    → unload → finalizeDispose            ← 终态，销毁压倒一切
  ├─ forceReload → reload（Restart/Update 专用）
  ├─ resolveInjections()：逐名查 binding，任一缺失 → epochInactive
  │    可满足 epoch = ":<providerUID>.<bindingSeq>..." ← 代际身份证
  └─ 新旧 epoch 对比
       ├─ 相同       → 不动
       ├─ inactive   → applyUnload → 回 pending
       ├─ 异且 live  → reload：先卸旧代 → 重新解析（防旧 binding）→ load
       └─ 异且干净   → applyLoad
load() 的异常分支：
  ├─ ResolveConfig 出错/panic → fail
  ├─ Run 出错/panic           → fail：先 unload 回滚半注册 effects → failed
  ├─ provider 在解析后已失活  → 归还 live、记 inactive、回 pending 重跑
  │    （不归还则 epoch 已提交，rerun 匹配成功 → 永卡 loading，#20 教训）
  └─ 成功 → setState(active) → notifyProvided() 唤醒下游

【副作用注册 / disposable.go + context.go】
经插件原始 ctx：On / OnDispose / Provide / Serve / Load
  └─ fiber.disposables（Fiber 级，随 fiber 卸载）
ctx.Effect(label, body(scope))：scope 是派生 Context，显式携带所有权
  ├─ 经 scope 注册      → effectEntry.children（Effect 级，随 effect 回收）
  ├─ body 内仍用外层 ctx → 平级副作用，归 Fiber（刻意区分，#56）
  ├─ entry 先发布再跑 body → 并发 unload 必能看见；
  │    disposer 未安装时收到销毁请求 → deferred，安装后补做
  └─ body panic → 摘除 entry + 回收已收养 children → 原 panic 重抛 ← 异常
回收统一 LIFO（新者先卸）；Disposer 幂等（Once）；
disposer 自身 panic 只记日志，不中断其余回收                  ← 异常

【服务传播 / service.go】
Provide → registerService：同 scope 重名 → SERVICE_EXISTS 报错 ← 异常
  ├─ provider 仍 loading → 暂不 notify（不暴露未初始化服务）
  └─ provider active     → notify(name, scope)
        └─ 扫描 live fibers 中 inject 该名且同 scope 者 → refresh
rebind（解绑再 Provide）→ seq+1 → 依赖者 epoch 变 → reload
Set（仅限自有服务）→ 原地换值，seq 不变 → 依赖者【不】reload
服务消失（binding disposer 或 provider 失活）
  └─ notify → 下游 epochInactive → 级联 LIFO 回收 → pending → 可再激活

【loader 装配 / loader/loader.go】
JSON layer(s) → Compose
  ├─ patch 按 id 定位、整字段替换（Config 整体替换，不深合并）
  ├─ id 未命中 → 默认 Warning / Strict() 报错        ← Cordis 静默丢弃，改可见
  └─ insert+其他字段混写 / 非组条目带 children → 拒绝或警告
Tree.Load(ctx, registry) → all-or-nothing：
  任一条目失败 → 倒序 Dispose 已加载 fibers + 失败条目自身 → 返回错误
Register[C] 在注册期用闭包固定类型参数 → 运行期只调闭包（不做运行期擦除）
```

图与代码不一致时以代码为准（核对入口：fiber.go sync/load、
context.go Effect/resolveService、service.go provide/notify、loader.go Compose）。
