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
    _, err = ctx.Provide("db", db)
    return err
})

fiber, err := rootCtx.Load(plugin, dbConfig{Path: "app.db"})
```

加载是**同步**的：`Load` 返回时 fiber 已经定态。插件体（或配置校验）失败会同时从 `err` 和
`fiber.Error()` 报出，fiber 本身照样返回，方便检查或 `Update` 重试；**依赖未就绪停在
`pending` 不算错误**（`err == nil`）。`ctx.Load(plugin, config)` 与等价的包级
`cordis.Load(ctx, plugin, config)` 行为一致，后者用于需要把加载助手当作值传递的场合。

配置类型在**编译期**校验：`Load` 只接受 `*Plugin[C]` 与该插件的 `C`。运行期才知道类型的宿主
（例如 `loader`）在**注册时**用闭包把 `C` 固定下来，再调用泛型入口——库不再导出把类型擦除的
`Definition` 契约。

## 核心语义映射

| Cordis (TypeScript) | cordis-go | 说明 |
| --- | --- | --- |
| `ctx.foo` | `ctx.Get[*Foo]("foo")` | Go 没有 Proxy，改为显式、类型安全的查找 |
| `ctx.plugin(p, cfg)` | `ctx.Load(p, cfg)` | 返回 `*Fiber`；同一个 plugin 加载两次 = 两个 fiber |
| `ctx.inject(deps, cb)` | `cordis.Inject(ctx, deps, cb)` | 依赖就绪前挂起，变更时自动重载 |
| `ctx.provide(name, v)` | `ctx.Provide(name, v)` | 类型参数从 `v` 推断；返回 `(Disposer, error)`，所有权属于当前 fiber |
| `ctx.effect(fn)` | `ctx.Effect(label, body)` | 可逆副作用 |
| `ctx.on / emit / bail / waterfall` | `ctx.On` / `ctx.Emit` / `ctx.Bail` / `ctx.Waterfall` | 泛型事件，payload 类型在编译期确定；注册与分发都是 Context 方法，包级同名函数是等价形态 |
| `ctx.isolate(name)` | `ctx.Isolate(name)` / `ctx.IsolateShared(name, label)` | 服务按作用域 label 索引；同一 label 让两个作用域合并，但该 label 全应用只对应一个服务名（见「Service 与 Inject」） |
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
它的寿命覆盖 fiber 实例的整个生命周期。注意：依赖变化导致的 unload/reload 不会取消它；
如果某次 load 启动的 goroutine 必须随该次 load 结束，请在 `ctx.OnDispose` 里注册取消。

根 fiber 的 `context.Context` 默认派生自 `context.Background()`，整棵树的寿命由调用方掌握
（`root.Fiber().Dispose()`）。要让宿主的信号/取消来接管，用 `cordis.WithBaseContext`：

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

root := cordis.New(cordis.WithBaseContext(ctx))
// ctx 一取消 = root.Fiber().Dispose()：fiber 走 unloading → disposed，
// disposer 按 LIFO 回收，插件的 ctx.Context() 同时结束。
```

语义只有一条：**base context 取消 ≡ `root.Fiber().Dispose()`**，不存在"goroutine 停了但服务
还注册着"的中间态。两点注意：base context 的 value 与 deadline 会被整棵树继承，所以**不要**
把 request-scoped 的 context 传进来（一个 30s 的请求 deadline 会拆掉整个应用）；`root.Done()`
依然只由 Dispose 关闭。不传这个 option 时行为与以前完全一致。

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
- `disposed`：终态，走到这里就不再重启——`fiber.Update()` / `fiber.Restart()` 返回
  `INACTIVE_EFFECT`。`Dispose()` 是它的入口，但调用返回时未必已经落到这个状态（见下）
- 依赖的提供者被替换时，fiber 会自动 unload → load，插件体重新执行

`failed` 不是终态：`fiber.Update(cfg)` 或依赖重新就绪都会再跑一次。失败时
`Load` 已经用 `err` 报过一次，`fiber.Error()` 保存同一个错误，直到下次加载成功才清空。

`fiber.Dispose()` 会立即取消 `ctx.Context()`；如果调用时已有 refresh transition 在跑，
effect 回收会延后到该循环。`Dispose()` 返回不代表所有 effect 已经回收完成；调用方不应
依赖它作为资源回收完成的同步点。

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
    db, _ := ctx.Get[*DB]("db") // 到这里 db 一定可用
    return nil
}).WithInject("db")

fiber, _ := rootCtx.Load(consumer, struct{}{})
// fiber.State() == StatePending —— 还没人提供 db

dispose, _ := rootCtx.Provide("db", newDB())
// fiber.State() == StateActive —— 自动激活

dispose()
// 回到 pending；再提供新实现会重新加载
```

`ctx.Serve` 是常用封装：注册服务，并在实例实现 `Start()` / `Stop()` 时自动调用。

服务族都是类型化方法：`ctx.Get[*DB]("db")`；`ctx.Provide("db", db)` 的类型参数从服务值推断，
所以既有调用点不用改，只持有 `any` 的宿主直接写 `ctx.Provide("db", svcAny)`（T 推断为 `any`）。
**只知道服务名字**的宿主用无类型的 `ctx.Lookup(name)` / `ctx.Set(name, svc)`：Go 的方法集一个
名字只能有一个方法，这两个名字因此留给无类型形态。

⚠️ **隔离 label 的真实语义**：服务绑定表按 **label** 索引（没有隔离时 label 就是服务名本身），
所以**同一个 label 在整个应用里只能对应一个服务名**。`IsolateShared("db", "shared")` 与
`IsolateShared("cache", "shared")` 会落在同一个槽位上：第二次 `Provide` 报
`SERVICE_EXISTS`，而错误信息里的 owner 是第一个绑定的提供者。用共享 label 合并作用域时，
让 label 与服务名一一对应。

#### 内置服务

`cordis.New()` 已经把三个服务装进 root 作用域；它们就是普通服务，按名字注入即可：

| 名字 | 类型 | 用途 |
| --- | --- | --- |
| `registry` | `cordis.Registry` | 插件注册表的只读视图：`Size()` / `Plugins()` |
| `events` | `*cordis.EventService` | 事件总线本身，`Context()` 返回拥有它的上下文 |
| `logger` | `*cordis.LoggerService` | 日志工厂：`Logger(name)` 返回带名字的 `*Logger` |

```go
rootCtx := cordis.New()
registry := rootCtx.MustGet[cordis.Registry]("registry") // examples/hotplug 的用法
```

### 5. Event — 带作用域过滤的事件总线

`ctx.On` / `ctx.OnOnce` / `ctx.OnValue` / `ctx.OnWaterfall` **注册**，`ctx.Emit` /
`ctx.Bail` / `ctx.Serial` / `ctx.Parallel` / `ctx.Waterfall` **分发**。每个分发模式都有
`*Scoped` 变体（`EmitScoped` / `BailScoped` / `SerialScoped` / `ParallelScoped` /
`WaterfallScoped`），只投递给同一隔离作用域内的监听者；`cordis.Global()` 可让监听者
跨越作用域（对应 Cordis 的 `global` 选项）。

```go
type Tick struct{ N int }

rootCtx.On("tick", func(t Tick) { ... })          // 注册：Context 方法（Go 1.27 泛型方法）
rootCtx.Emit("tick", Tick{N: 1})                  // 分发：Context 方法
cordis.On(rootCtx, "tick", func(t Tick) { ... })  // 等价函数形态，行为一致
```

每个方法都有等价的包级函数（首参为 context），用于必须把助手当作值传递的场合——泛型方法要
先实例化才能取方法值；`ctx.Load` / `ctx.LoadWithInject` 与服务族（`ctx.Get` / `ctx.MustGet` /
`ctx.Provide` / `ctx.ProvideChecked` / `ctx.Serve`）都遵循同一规则。

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

补丁语义与 Cordis 一致，有三点必须强调：

1. **`config` 整体替换，不深合并**。补丁里没写的字段不是"保留"，而是随整个 config 一起消失
   （除非补丁本身没写 `config`，此时保留原值）。
2. **补丁 id 匹配不到条目时不会静默丢弃**。Cordis 的 include 插件会静默跳过，
   这是"配置为什么没生效"的经典坑；本库默认记为 `Tree.Warnings`，`loader.Strict()` 下直接报错。
3. **层文件里的数字按字面量保留**。解析用 `json.Number`，所以 `Patch.Config` / `Node.Config` 里的
   数字是 `json.Number` 而不是 `float64`：超过 2^53 的整数不会被静默取整，`Dump` 打印的也是文件里
   写的那个值。交给插件的配置仍按目标字段类型解码，因此类型对不上（例如把 `{"count": 1.0}` 塞进
   `int64` 字段）会在加载时报错，而不是被悄悄截断。

`Tree.Dump()` 输出带来源标注，等价于 `dsh --profile <name> --dump-config`：

```
# cordis-go config dump
# layers: base -> profile
- id: "server"  # from base; patched by profile
  name: "server"
  config: {"addr": ":9090"}
```

### 命令行

配置组合是纯数据操作，不需要插件注册表，因此附带一个独立的查看工具（下面的
`base.json` / `profile.json` 是示意路径，仓库里没有这两个文件——换成你自己的配置）：

```sh
go run ./cmd/cordis dump base.json profile.json
go run ./cmd/cordis dump --strict base.json profile.json   # 未匹配的补丁 id 直接报错
go run ./cmd/cordis dump --patch=base.json base.json       # 强制第一个文件按 patch 层解析
```

`--patch=<path>` 可以重复，被点名的文件无论出现在哪个位置（包括第一个）都按 patch 层解析。
显式标记覆盖的只是「第一个文件按 base 层解析」这条默认规则：`.patch.json` 结尾的第一个文件
本来就是 patch 层，标记对它没有额外影响。显式请求帮助用
`-h` / `--help`：用法打到 stdout 并以 0 退出；用法错误（未知命令、未知 flag、缺少参数）把用法
打到 stderr 并以 2 退出；加载或组合失败以 1 退出。

输出示例（注意 `server` 的 `tls` 字段被整体替换掉了，嵌套条目也能按 id 补丁）：

```
# cordis-go config dump
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
| 动态属性 `ctx.foo` | Go 无 Proxy，改为 `ctx.Get[T]`。代价是失去语法糖，收益是编译期可查、可静态分析依赖 |
| 模块热替换（HMR） | **Go 无法在进程内卸载已加载的代码**。本库只做「配置热重载 + 插件生命周期重载」；真正的热插拔请把插件放进 WASM（wazero）或子进程（go-plugin） |
| `Promise` / 异步 effect | 全部同步执行。异步资源用 `ctx.Context()` 取消，用 goroutine 承载 |
| schemastery 校验 | 用 Go 结构体 + `WithValidate`，配置从 JSON 解码 |
| `intercept` / `accessor` / `mixin` | 未实现：三者都是围绕 Proxy 的机制，在静态类型语言里没有对应物 |
| `Service` 基类 / `@Inject` 装饰器 | 用 `ctx.Serve` + `WithInject` 代替 |
| 事件的 `this` 绑定 | Go 没有 `this`，需要时把上下文作为 payload 字段传入 |
| 监听器 panic | 上游的 throw 会中断 `emit` / `bail` / `serial` / `waterfall`；本库把 panic 捕获成错误、记日志后**继续**投递给下一个监听者（`Parallel` 把 panic join 进返回的 error）。单个坏监听者不会带走整次分发，这是有意的 Go 侧差异 |

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
go run ./examples/events
go run ./examples/hotplug
go run ./examples/isolation
go run ./examples/serviceprobe
go run ./examples/walkthrough
go run ./cmd/cordis dump base.json profile.json   # base.json / profile.json 是示意路径
```

`examples/basic` 演示配置层叠加与 dump、依赖注入、事件、插件挂起与激活、卸载回收。
`examples/events` 逐一演示 `Emit` / `Bail` / `Serial` / `Parallel` / `Waterfall` 五种分发
模式及各自的 `*Scoped` 变体，附 `OnOnce` / `Prepend` / `Global` 与 panic 隔离（panic 继续分发
而非中断，见「与 Cordis 的差异」）。
`examples/hotplug` 演示应用持续运行时 provider 插件消失，依赖方进入 `pending`，再注册一个
提供同名服务的新插件后依赖方自动恢复。
`examples/isolation` 演示默认、隔离与显式共享三种服务作用域。
`examples/serviceprobe` 是服务容器的行为探针：自提供服务可见性、同名冲突、依赖方跟随 provider
状态、`Set` 语义、可用性谓词与 `Serve` 生命周期钩子。
`examples/walkthrough` 打印 effect 树的卸载顺序（LIFO）。

## 开发

```sh
make tools   # 安装 lint 工具（staticcheck、revive），只需一次
make ci      # CI 跑的东西：gofmt 检查 + go vet + staticcheck + revive + go test -race
```

| 目标 | 作用 |
| --- | --- |
| `make fmt` | `gofmt -w .` |
| `make vet` | `go vet ./...` |
| `make lint` | gofmt 检查 + `go vet` + `staticcheck` + `revive -config .revive.toml` |
| `make test` | `go test -race ./...` |
| `make integration` | `go test -race -count=3 ./...`，重复跑以抖出时序问题；`.github/workflows/integration.yml` 执行 |
| `make fuzz` | 对 loader 配置路径模糊测试 30s（`FuzzCompose`；种子语料随普通 `go test` 回归） |
| `make examples` | 逐个执行六个示例（`go vet` 只编译它们；能跑起来是另一回事），CI 执行 |
| `make tools` | 安装固定版本的 staticcheck / revive |
| `make ci` | `lint` + `test`，`.github/workflows/ci.yml` 执行同一入口 |

风格约定由 `.revive.toml` 固化（revive 默认规则集 + **100 列**行宽上限）。

⚠️ **lint 工具必须用不低于 `go.mod` 声明的 Go 版本构建**：用旧 Go 编译的 `staticcheck` /
`revive` 读不懂泛型方法，前者报 export data 版本错误，后者把源文件判成语法错误。
`make tools` 装的就是验证过的版本。另外注意 `staticcheck` 在 `GOCACHE` 不可写时会**静默
空转**（只打印 `./... matched no packages` 并返回 0）——看到这句就别信它的"通过"。

## 状态

- 需要 **Go 1.27+**：事件分发、插件加载与服务访问用泛型方法（`go.mod` 的 `go 1.27.0` 即最低
  工具链要求）
- 运行路径零第三方依赖（库本身不 import 任何第三方模块），配置解码用标准库 `encoding/json`；
  测试工具链引入 `go.uber.org/goleak` 作为**唯一的测试期依赖**，两个被测包各挂一个
  `TestMain`，套件跑完后校验没有测试遗留 goroutine（dispose / 卸载 / 回滚路径漏掉的协程
  会被整个包的红灯抓出来）
- `go vet` / `go test -race` 全绿。测试分三层：单元测试、跨特性集成测试（`integration_*.go`，
  `make integration` 以 `-race -count=3` 重复跑，独立 workflow 验证）、以及 loader 配置路径的
  模糊测试（`FuzzCompose`，`make fuzz`，CI 里每次跑 30s；写这条时的验证轮已跑过 390 万次执行）。
  两个被测包各挂一个 `TestMain` 做泄漏检测。覆盖率现场量：
  `go test -race -cover ./...` 给出核心包 93% 上下、`loader` 92% 上下。这两个数字只是某个 dev
  基线上的量级，会随提交变化（加一个测试就会动），所以这里不写死——要当前值就跑那条命令。
  `examples/` 不在统计里（六个示例由 `make examples` 在 CI 里逐个执行）；`cmd/cordis` 的退出码契约由它自己的进程内测试
  钉住（`--help` / 用法错误 / 加载失败三档）
- 交叉编译验证：linux/amd64、windows/amd64、darwin/arm64
- ⚠️ 破坏性变更：`Definition` 不再导出；`ctx.Load` / `ctx.LoadWithInject` 改为泛型方法
  `(plugin, config)`；类型化服务访问改为方法形态——`ctx.Get` / `ctx.MustGet` / `ctx.Provide` /
  `ctx.ProvideChecked` / `ctx.Serve`，无类型的 `ctx.Get(name)` 由 `ctx.Lookup(name)` 取代，
  包级 `cordis.Get` / `cordis.Provide` 等签名不变（迁移说明见「Service 与 Inject」）
- ⚠️ 行为变更（宿主可见）：`Patch.Config` / `Node.Config` 里的数字现在是 `json.Number` 而不是
  `float64`，超过 2^53 的整数不再被静默取整，小数进整型字段会报错；断言 `.(float64)` 的宿主需要改，
  插件侧不受影响（见「配置驱动装配」第 3 点）
- ⚠️ 破坏性变更：`Fiber.Disposed()` 方法已删除——判断终态改用
  `fiber.State() == cordis.StateDisposed`；`fiber.Dispose()` 只保证发起卸载，**不是**「所有
  effect 已回收」的同步点（见「Fiber」）

## 目录结构

```
cordis-go/
├── context.go            # Context：查找、隔离、Fork、生命周期
├── fiber.go              # Fiber：状态机、epoch、加载/卸载
├── service.go            # 服务注册、查找、通知依赖者
├── registry.go           # 插件定义与 plugin runtime 注册表
├── events.go             # 事件总线与五种分发模式
├── logger.go             # 轻量日志服务
├── disposable.go         # 幂等 Disposer 与 effect 列表
├── funcforms.go          # 包级函数形态：转发到同名 Context 方法
├── Makefile              # fmt / vet / lint / test / integration / ci 入口
├── .revive.toml          # 风格规则：revive 默认集 + 100 列行宽
├── .github/workflows/    # CI：Go 1.27，ci.yml 跑 make ci，integration.yml 跑 make integration
├── loader/               # 配置驱动装配、patch 层、config dump
├── cmd/cordis/           # 配置 dump 命令行工具
└── examples/
    ├── basic             # 端到端示例
    ├── events            # 五种事件分发模式及 Scoped 变体
    ├── hotplug           # 运行中替换插件
    ├── isolation         # 默认、隔离与共享服务作用域
    ├── serviceprobe      # 服务容器行为探针
    └── walkthrough       # effect 卸载顺序
```

## 参考

- Cordis 文档：<https://cordis.moe/zh-CN/>
- Cordis 源码语义基线：`@deepseek-ai/cordis` v4.0.2（`src/fiber.ts`、`src/reflect.ts`、`src/registry.ts`）
- Caddy 模块系统（编译期注册 + 配置装配的先例）：<https://caddyserver.com/docs/modules>

## License

MIT
