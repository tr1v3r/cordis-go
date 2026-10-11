# DECISIONS — 架构与选型说明

「看起来能优化/能对称化」但不可顺手改的决策。日期 2026-10-12。
证据：A=可回查提交（短哈希）或代码现状；C=推测（待维护者确认）。

## D-001：definition 接口保持非导出（擦除边界）
背景：fiber 必须在不认识配置类型的前提下调 ResolveConfig/Run（f493ea3）。
选择：非导出 `definition` 接口；类型化入口走 `ctx.Load[C]` 泛型方法，
编译期检查配置类型；曾导出的 `Definition` 已删除。
替代方案：导出 erased `ctx.Load(Definition, any)`——因 Go 1.27 泛型方法
落地而废弃（A：f493ea3 提交正文）。
重新评估条件：Go 出现按名字参数化方法的新机制，或出现必须传 erased
definition 的宿主场景（那样应仿 loader 用闭包，而不是重新导出）。
状态：生效中（AGENTS.md「不能碰的约束」第一条）。

## D-002：泛型入口「方法 + 包级孪生」成对出现；Inject 唯一例外
背景：方法值（ctx.Emit[T]）绑定接收者，类型里没有 ctx，无法当
`func(*Context,...)` 传递；上游 TS 只有方法形态（A：funcforms.go 注释、
b7e5c97）。孪生集中在 funcforms.go，只放转发。
例外：`Inject` 只有 registry.go 的包级形态（对应 Cordis ctx.inject），
没有方法孪生——补一个即破坏本约定（A：AGENTS.md）。
重新评估条件：上游 Cordis 官方提供函数形态，或 Go 方法值语义变化。
状态：生效中。

## D-003：同步语义；Dispose 不是完成屏障
背景：Promise → 同步调用（doc.go）。Load 返回时 fiber 已 settled
（active/pending/failed），没有后续 await 点，失败必须当场返回
（A：69daa63）。`Fiber.Disposed()` 完成信号已删除：Dispose 只保证发起
卸载，终态用 `State()==StateDisposed` 观察（A：2093b0b，BREAKING）。
替代方案：保留 Disposed channel 作同步点——因转换协议重做而移除（A）。
重新评估条件：出现需要「等待全部 effect 回收完」的宿主 API；
届时加显式 Wait 而不是恢复旧 Disposed。
状态：生效中。

## D-004：effect 所有权由 Context 显式携带（不是 fiber 共享标记）
背景：归属曾由 fiber 上单个"当前 body"标记隐式推断，并发注册互相
收养——并发 Provide 后 dispose 返回时服务仍注册，再 Provide 报
SERVICE_EXISTS（A：724dc4b #47、ad48534 #56，BREAKING：Effect body 签名
改为 `func(*Context) Disposer`）。插件自身 context 一律重置为 fiber 级
scope，即使它由某 effect 内的 Load 创建。
重新评估条件：无现实推翻条件；任何「恢复隐式 current-effect」的
简化都应视为回归（回归测试 effect_scope_provide_test.go）。
状态：生效中（2026-09-14 起）。

## D-005：epoch = provider UID + binding seq；Set 不触发 reload
背景：只编码 provider UID 时，provider 解绑再 Provide 同名服务对依赖者
不可见——依赖者跨 swap 忙碌后 epoch 匹配，继续用已释放的旧对象
（A：635f52a #21）。每 binding 发 seq，epoch 命名 binding 而非 owner。
`ctx.Set` 原地换值、seq 不变、依赖者不 reload——这是被测试钉住的
契约（A：service_binding_test.go）。
重新评估条件：宿主需要"值替换也重载依赖者"语义时，需同时改
service_binding_test.go 钉住的契约并升主版本。
状态：生效中。

## D-006：热重载限于配置与生命周期，不做代码级 HMR
背景：Go 不能卸载已加载代码；Cordis 的 import() 动态性无进程内等价物
（A：doc.go、ba70b18 初始提交）。运行时零第三方依赖（唯一依赖 goleak
为 test-scoped，A：1756078）；loader 配置只支持 JSON（同因，A：README）。
重新评估条件：宿主需要真代码热替换时，走进程级/build 级方案，
不在此库内补 in-process HMR。
状态：生效中。

## D-007：loader 在注册期擦除类型，运行期只调闭包
背景：运行期才知道插件名的宿主（配置驱动）无法提供类型参数。
选择：`Register[C]` 注册时把 decode+Load 建成闭包（A：loader.go、
f493ea3）。运行期不做 reflect 擦除。
重新评估条件：需要运行期动态构造插件定义时——先问能否仍用闭包。
状态：生效中（AGENTS.md 明令禁止加载期擦除）。

## D-008：配置数值与边界语义精确化（宿主可见）
json：`UseNumber` 解码 layer，整数不经 float64 取整（2^53 外精确），
`Patch.Config/Node.Config` 中数字是 json.Number——宿主断言
`.(float64)` 会破坏（A：575666c #33，行为变更）。空 `config:{}` 与
缺 config 分流：空对象必须过目标类型 UnmarshalJSON（默认值/校验生效），
nil 才代表"无配置"（A：cf8349a #32）。
重新评估条件：宿主生态强烈要求 float64 兼容时——倾向提供转换助手
而非回退取整。
状态：生效中。

## D-009：Tree.Load all-or-nothing 回滚
任一条目失败：倒序 Dispose 已加载 fibers + 失败条目自身的 fiber
（失败条目仍占父 effect 槽与 runtime，必须显式回收，A：0bce5d3 #22）。
半应用树比无树更难推理（A：提交正文）。
重新评估条件：宿主要求"尽力加载、跳过坏条目"时，作为显式选项
而非默认行为引入。
状态：生效中。
