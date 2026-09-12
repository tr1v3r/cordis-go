# cordis-go

把 [Cordis](https://cordis.moe) 元框架（v4）的插件系统语义移植到 Go。
Cordis 是 [Koishi](https://koishi.chat) 与 DeepSeek Harness 的插件引擎，它的核心主张是：
**插件的每一个副作用都必须可逆**，因此插件可以随时装卸、依赖变更时自动重载。

本库保留了这套语义，并用 Go 的方式重写了它依赖 JavaScript 动态特性的部分。

```go
rootCtx := cordis.New()
plugin := cordis.Define[dbConfig]("db", func(ctx *cordis.Context, cfg dbConfig) error {
    db, err := openDB(cfg.Path)
    if err != nil {
        return err
    }
    ctx.OnDispose(func() { db.Close() }) // 卸载时自动回收
    _, err = cordis.Provide(ctx, "db", db)
    return err
})

fiber, err := cordis.Load(rootCtx, plugin, dbConfig{Path: "app.db"})
```

加载是**同步**的：`Load` 返回时 fiber 已经定态。插件体（或配置校验）失败会同时从 `err` 和
`fiber.Error()` 报出，fiber 本身照样返回，方便检查或 `Update` 重试；**依赖未就绪停在
`pending` 不算错误**（`err == nil`）。

## 核心语义映射

| Cordis (TypeScript) | cordis-go | 说明 |
| --- | --- | --- |
| `ctx.foo` | `cordis.Get[*Foo](ctx, "foo")` | Go 没有 Proxy，改为显式、类型安全的查找 |
| `ctx.plugin(p, cfg)` | `cordis.Load(ctx, p, cfg)` | 返回 `*Fiber`；同一个 plugin 加载两次 = 两个 fiber |
| `ctx.inject(deps, cb)` | `cordis.Inject(ctx, deps, cb)` | 依赖就绪前挂起，变更时自动重载 |
| `ctx.provide(name, v)` | `cordis.Provide[T](ctx, name, v)` | 返回 `(Disposer, error)`，所有权属于当前 fiber |
| `ctx.effect(fn)` | `ctx.Effect(label, body)` | 可逆副作用 |
| `ctx.on / emit / bail / waterfall` | `cordis.On / Emit / Bail / Waterfall` | 泛型事件，payload 类型在编译期确定 |
| `ctx.isolate(name)` | `ctx.Isolate(name)` / `ctx.IsolateShared(name, label)` | 服务隔离，同名服务互不冲突；同一 label 可让两个作用域合并 |
| `ctx.extend()` | `ctx.Fork(name)` | 共享 fiber 的子上下文 |
| `@cordisjs/plugin-loader` + `cordis.yml` | `loader` 子包 + JSON 配置 | 配置驱动装配、patch 层、config dump |
| `Promise` / `await` | 同步调用 + `ctx.Context()` | 取消传播用 `context.Context` |

## 五个概念

### 1. Context — 依赖容器与生命周期作用域

`Context` 既是服务查找的入口，也是副作用的归属边界。`ctx.OnDispose` 注册的所有回收动作，
在所属 fiber 卸载时按 **后进先出** 执行。

```go
ctx.OnDispose(func() { order = append(order, "first") })
ctx.OnDispose(func() { order = append(order, "second") })
// 卸载顺序：second → first
```

`ctx.Context()` 返回一个随 fiber 一起取消的 `context.Context`，交给插件启动的 goroutine，
这样 goroutine 的存活期和插件一致。

### 2. Fiber — 每个插件实例的生命周期状态机

```
pending ──依赖就绪──> loading ──成功──> active
   ↑                     │                │
   └──── unloading <─────┴──── failed     │
                    ↑                      │
                    └──── 依赖变更/Dispose ┘
```

- `pending`：声明的依赖还没全部就绪，插件体不执行
- `active`：已加载，且它提供的服务对依赖者可见
- `failed`：插件体返回错误或 panic（panic 被捕获成 error，不会炸进程）
- 依赖的提供者被替换时，fiber 会自动 unload → load，插件体重新执行

`failed` 不是终态：`fiber.Update(cfg)` 或依赖重新就绪都会再跑一次。失败时
`Load` 已经用 `err` 报过一次，`fiber.Error()` 保存同一个错误，直到下次加载成功才清空。

### 3. Effect — 可逆副作用

Cordis 的一切副作用都通过 `ctx` 注册，因此卸载时可以精确回收。Go 版额外提供了
`Disposer`（幂等）与 `EffectMeta`（诊断标签，见 `fiber.Effects()`，嵌套关系通过
`Children()` 展开）。

在某个 `Effect` 的 body 里注册的副作用**归该 effect 所有**：销毁外层 effect 会连带
销毁内层，这与 Cordis 的 effect 收集器一致。

### 4. Service 与 Inject — 依赖声明与 epoch

服务按 **隔离作用域** 存储。`Inject` 声明依赖后，fiber 会计算一个 epoch：把每个依赖的
**提供者 fiber UID** 拼起来。epoch 变化即触发重载，所以「换掉实现」和「依赖消失/出现」
都是同一套机制。

```go
rootCtx := cordis.New()
consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
    db, _ := cordis.Get[*DB](ctx, "db") // 到这里 db 一定可用
    return nil
}).WithInject("db")

fiber, _ := cordis.Load(rootCtx, consumer, struct{}{})
// fiber.State() == StatePending —— 还没人提供 db

dispose, _ := cordis.Provide[*DB](rootCtx, "db", newDB())
// fiber.State() == StateActive —— 自动激活

dispose()
// 回到 pending；再提供新实现会重新加载
```

`Serve[T]` 是常用封装：注册服务，并在实例实现 `Start()` / `Stop()` 时自动调用。

### 5. Event — 带作用域过滤的事件总线

`On` / `OnOnce` / `OnValue` / `OnWaterfall` 注册，`Emit` / `Bail` / `Serial` /
`Parallel` / `Waterfall` 分发。每个分发模式都有 `*Scoped` 变体（`EmitScoped` /
`BailScoped` / `SerialScoped` / `ParallelScoped` / `WaterfallScoped`），只投递给同一
隔离作用域内的监听者；`cordis.Global()` 可让监听者跨越作用域（对应 Cordis 的 `global` 选项）。

## 配置驱动装配（loader）

Go 不能在运行时 `import` 代码，所以插件在编译期注册进 `Registry`，配置只负责
**选择与配置**（与 Caddy 的模块系统同样的取舍）。

```go
rootCtx := cordis.New()
registry := loader.NewRegistry()
loader.MustRegister(registry, "db", dbPlugin)

base, _ := loader.ParseLayer("base", baseJSON)          // 基础层：创建条目
profile, _ := loader.ParsePatchLayer("profile", patchJSON) // 补丁层：按 id 修改

tree, err := loader.Compose([]loader.Layer{base, profile}, loader.Strict())
tree.Dump(os.Stdout)
fibers, err := tree.Load(rootCtx, registry)
```

补丁语义与 Cordis 一致，有两点必须强调：

1. **`config` 整体替换，不深合并**。补丁里没写的字段不是"保留"，而是随整个 config 一起消失
   （除非补丁本身没写 `config`，此时保留原值）。
2. **补丁 id 匹配不到条目时不会静默丢弃**。Cordis 的 include 插件会静默跳过，
   这是"配置为什么没生效"的经典坑；本库默认记为 `Tree.Warnings`，`loader.Strict()` 下直接报错。

`Tree.Dump()` 输出带来源标注，等价于 `dsh --profile <name> --dump-config`：

```
# layers: base -> profile
- id: "server"  # from base; patched by profile
  name: "server"
  config: {"addr": ":9090"}
```

### 命令行

配置组合是纯数据操作，不需要插件注册表，因此附带一个独立的查看工具：

```sh
go run ./cmd/cordis dump base.json profile.json
go run ./cmd/cordis dump --strict base.json profile.json   # 未匹配的补丁 id 直接报错
```

输出示例（注意 `server` 的 `tls` 字段被整体替换掉了，嵌套条目也能按 id 补丁）：

```
# layers: base.json -> profile.json
# warning: layer profile.json: patch id "typo" matched no entry
- id: "server"  # from base.json; patched by profile.json
  name: "server"
  config: {"addr": ":9090"}
- id: "grp"  # group  # from base.json
  plugins:
    - id: "inner"  # from base.json; patched by profile.json
      name: "metrics"
      config: {"interval": "5s"}
```

## 与 Cordis 的差异（诚实清单）

**已经保留的**：上下文树、Fiber 状态机、可逆副作用与 LIFO 回收、服务提供/依赖注入与
epoch 重载、隔离作用域、事件分发五种模式、作用域过滤、配置驱动装配与补丁层。

**有意不同的**：

| 项 | 说明 |
| --- | --- |
| 动态属性 `ctx.foo` | Go 无 Proxy，改为 `cordis.Get[T]`。代价是失去语法糖，收益是编译期可查、可静态分析依赖 |
| 模块热替换（HMR） | **Go 无法在进程内卸载已加载的代码**。本库只做「配置热重载 + 插件生命周期重载」；真正的热插拔请把插件放进 WASM（wazero）或子进程（go-plugin） |
| `Promise` / 异步 effect | 全部同步执行。异步资源用 `ctx.Context()` 取消，用 goroutine 承载 |
| schemastery 校验 | 用 Go 结构体 + `WithValidate`，配置从 JSON 解码 |
| `intercept` / `accessor` / `mixin` | 未实现：三者都是围绕 Proxy 的机制，在静态类型语言里没有对应物 |
| `Service` 基类 / `@Inject` 装饰器 | 用 `cordis.Serve` + `WithInject` 代替 |
| 事件的 `this` 绑定 | Go 没有 `this`，需要时把上下文作为 payload 字段传入 |

### loader 的已知限制

- **group 条目不是独立 fiber**：组的子条目加载在父上下文的一个 `Fork` 里，因此整个组
  随父 fiber 一起装卸，不能单独重载。Cordis 的 group 有自己的 fiber，这里做了简化。
- **没有配置热重载 watcher**：`Compose` 是纯函数，重新组合再重新 `Load` 即可，
  但本库不内置 fsnotify 监听；diff 后增量应用（只重启变化的条目）也留给使用方。
- **配置只支持 JSON**：`ParseLayer` 用 `encoding/json`，零依赖。需要 YAML 时把文件读成
  字节后自行转换即可。
- **`config` 只能是 JSON 对象**：`Patch.Config` 是 `map[string]any`，无法区分
  "没有 config" 与 `"config": null`，因此补丁不能把配置清空或替换成标量。

## 运行

```sh
go test ./...
go run ./examples/basic
go run ./cmd/cordis dump base.json profile.json
```

`examples/basic` 演示了：配置层叠加与 dump、依赖注入、事件、插件挂起与激活、卸载回收。

## 状态

- 零第三方依赖（`go list -m all` 只有本模块），配置解码用标准库 `encoding/json`
- `go vet` / `go test -race` 全绿；52 个测试，核心包覆盖率 83.1%，loader 72.7%
- 交叉编译验证：linux/amd64、windows/amd64、darwin/arm64

## 目录结构

```
cordis-go/
├── context.go     # Context：查找、隔离、Fork、生命周期
├── fiber.go       # Fiber：状态机、epoch、加载/卸载
├── service.go     # 服务注册、查找、通知依赖者
├── registry.go    # 插件定义与 plugin runtime 注册表
├── events.go      # 事件总线与五种分发模式
├── logger.go      # 轻量日志服务
├── disposable.go  # 幂等 Disposer 与 effect 列表
├── loader/        # 配置驱动装配、patch 层、config dump
├── cmd/cordis/    # 配置 dump 命令行工具
└── examples/basic # 端到端示例
```

## 参考

- Cordis 文档：<https://cordis.moe/zh-CN/>
- Cordis 源码语义基线：`@deepseek-ai/cordis` v4.0.2（`src/fiber.ts`、`src/reflect.ts`、`src/registry.ts`）
- Caddy 模块系统（编译期注册 + 配置装配的先例）：<https://caddyserver.com/docs/modules>

## License

MIT
