# SCENARIOS — 验收案例库

改完代码后逐条对拍。多数案例已有回归测试钉住（标注文件，A 级），
文字版保存意图与观察方式；未钉住的行为先补测试再改。
日期 2026-10-12。

## S-001：依赖未就绪 → 等待；就绪 → 自动激活
前置：插件 A inject "db"；无人提供 "db"。
动作：ctx.Load(A)；随后 B Provide "db" 并转 active。
预期：Load 返回时 A 为 pending（非错误）；"db" 出现后 A 自动到
active，无需人工干预。
验证：fiber.State() 序列 pending→loading→active；
examples/hotplug 演示同一链路（含 provider 再消失的回环）。

## S-002：provider 消失 → 级联 LIFO 回收 → pending
前置：B provide "db" 且 active；A inject "db" 已 active 并注册了
listener/service/disposer。
动作：Dispose B。
预期：A 先于 B 完成 effects 倒序回收（listener 注销、服务解绑、
disposer 运行），A 回 pending；A 提供的下游服务同样失效并继续传播。
验证：A 各 disposer 按注册逆序运行；A.State()==pending；
docs/fiber-dependency-flow.md「服务消失后的传播」段一致。

## S-003：rebind 触发 reload，Set 不触发
前置：B active 提供 "db"；A inject "db" 已 active。
动作一：B 解绑 "db" 后重新 Provide（可同值）。
预期一：binding seq 变化 → A reload（epoch 不匹配）。
动作二：B 改用 ctx.Set("db", newVal)。
预期二：A 不 reload，仍持有原 binding，读值即时变新。
验证：service_binding_test.go（钉住 Set 的 in-place 契约）。

## S-004：插件 body 失败 → 半注册副作用全部回滚
前置：插件 body 先 Provide+On 再返回 error。
动作：ctx.Load(该插件)。
预期：Load 返回 (fiber, err)；fiber 为 failed；已注册的 effects
与服务名全部回收（服务名可立即被他人 Provide）；Restart 可重试。
验证：effects 计数归零、Lookup 该服务为 false；
fiber_lifecycle_test.go、loader 侧 TestTreeLoadDisposesTheFailedEntry。

## S-005：并发 Provide/注销 互不吞并（#56 回归）
前置：一个 fiber 的 isolation scope；≥8 goroutine 循环
Provide→立即 dispose 同名服务。
动作：运行至万次量级。
预期：每次 dispose 返回时服务确已注销——同 scope 下一次 Provide
不报 SERVICE_EXISTS；无"注销被推迟进别人拆解流程"的窗口。
验证：effect_scope_provide_test.go（当初 1.4 万次内必现的缺陷）。

## S-006：Effect 内外注册的所有权分流
前置：ctx.Effect("e", scope => { scope.On(...); outerCtx.On(...); return disposer })。
动作：单独调用该 effect 的 Disposer。
预期：经 scope 注册的 listener 随 effect 注销；经外层 ctx 注册的
listener 存活（fiber 级）；body 返回后再经 scope 注册 → panic
INACTIVE_EFFECT。
验证：effect_scope_test.go / effect_tree_test.go。

## S-007：waterfall 单结算；once 单发
前置：waterfall 链上某 listener 调 next 后 panic；
另一事件挂 OnOnce，两个 goroutine 同时 Emit。
动作：触发两种分发。
预期：final 只运行一次，panic 后链返回已结算值；once-listener
body 恰好执行一次，Effects() 不再列出它。
验证：events_waterfall_test.go / events_waterfall_state_test.go /
events_once_test.go。

## S-008：同 label 不同服务名不别名（#30 回归）
前置：IsolateShared("a","shared") 与 IsolateShared("b","shared")
两个 scope；"a" 已有 provider。
动作：在 "b" 的 scope 里 Get/Lookup/Set "b"。
预期：全部落空/报错（binding.name 不匹配视为无此服务），
取不到也不会改写 "a" 的服务。
验证：service_isolation_test.go
（TestSharedLabelDoesNotAliasServiceName）。

## S-009：loader 全量回滚与配置精确性
前置：树含 3 条目，第 3 条目失败；另一配置文件写
9007199254740993；某插件 config 为 `{}` 且 UnmarshalJSON 设默认值。
动作：Tree.Load；ParseLayer 后读 Node.Config；加载 `{}` 配置条目。
预期：前两条目的 fibers 被倒序 Dispose，返回错误；整数原样到达
插件与 Dump（json.Number，不经 float64）；空对象走目标类型
UnmarshalJSON（默认值生效），无 config 才是零值。
验证：loader_test.go / layer_file_test.go / dump_test.go。

## S-010：宿主 context 取消 = 整树拆除
前置：New(WithBaseContext(hostCtx))；树下有插件及其 goroutine。
动作：cancel hostCtx。
预期：root Dispose 被触发，整树按 S-002 的级联语义回收；不存在
"goroutine 已停但服务仍注册"的中间态；Done 关闭。
验证：docs/application-shutdown-flow.md；
basecontext_test.go（baseContext 相关用例）。
