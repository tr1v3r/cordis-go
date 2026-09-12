# AGENTS.md

面向在本仓库改代码的 AI 代理（人类贡献者同样适用）。使用者文档看 `README.md`，这里只讲**怎么改**。

`CLAUDE.md` 是指向本文件的软链接。

## 项目

`cordis-go` 是 Cordis v4（Koishi / DeepSeek Harness 背后的插件框架）的 Go 移植：Context 树 + 每插件
fiber 生命周期状态机 + 可逆副作用。零第三方依赖（`go list -m all` 只有本模块）。

## 工具链

- **需要 Go 1.27+**：`go.mod` 的 `go 1.27.0` 就是最低门槛。事件分发与插件加载是**泛型方法**，
  属于 Go 1.27 的语言特性。
- 命令统一走 `Makefile`：

```sh
make tools   # 安装固定版本的 staticcheck / revive，只需一次
make fmt     # gofmt -w .
make vet     # go vet ./...
make lint    # gofmt 检查 + vet + staticcheck + revive
make test    # go test -race ./...
make ci      # lint + test；GitHub Actions 跑的就是这个入口
```

三个容易踩的坑：

1. **lint 工具必须用不低于 `go.mod` 的 Go 构建**。旧 Go 编译的 `staticcheck` 读不了新版本的
   export data，旧 `revive` 会把泛型方法判成语法错误。`make tools` 装的是验证过的版本。
2. **`staticcheck` 在 `GOCACHE` 不可写时会静默空转**：只打印 `warning: "./..." matched no
   packages` 然后以 0 退出。看到这句就当它没跑过，不要当成通过。
3. **`revive` 传显式文件列表时会把每个文件当独立包**，`package-comments` 一定误报。要复现 CI
   就用包形态：`revive -config .revive.toml ./...`。

## 风格

- **100 列**上限，规则固化在 `.revive.toml`（revive 默认规则集 + `line-length-limit`）。
- **Go 源码的注释与输出一律英文**；`README.md` 是仓库里唯一的中文文件，不要再往 `.go` 里写中文。
- 导出符号必须有文档注释，以符号名开头、以句号结尾；非导出函数按需写，不要为凑格式补噪音。
- 测试失败信息统一 `want X, got Y`。
- 提交信息用 Conventional Commits，正文讲**为什么**；破坏性变更加 `!` 并写 `BREAKING CHANGE:`。

## 架构约束（动手前先看）

- **擦除边界是 `registry.go` 里非导出的 `definition` 接口**：fiber 不认识配置类型，只能通过它的
  `ResolveConfig` / `Run` 触达插件体。不要重新导出它——曾经的 `Definition` 已经删除。
- **泛型入口 = Context 方法 + 等价包级函数**：`ctx.Emit` / `cordis.Emit`、`ctx.Load` /
  `cordis.Load` 成对存在且行为一致。包级函数是"把助手当值传递"的唯一通道，因为泛型方法必须先
  实例化才能取方法值。完整约定写在 `doc.go`。
- **有些操作只能留在包级**：`On[E]` / `OnOnce[E]` / `OnValue[E]` / `OnWaterfall[E]` /
  `Get[T]` / `Provide[T]` / `ProvideChecked[T]`。原因是 `Context` 上这些名字已被**非泛型**方法
  占用，而 Go 不允许泛型方法与非泛型方法同名。给某个操作加方法形态之前，先确认 `Context` 上
  没有同名方法。
- **运行期才知道插件类型的宿主**（参考 `loader`）在**注册时**用闭包固定类型参数：`Register[C]`
  里把 `load` 闭包建好，运行期只调这个闭包。不要在加载时试图擦除类型。
- **同一 definition 加载两次 = 一个 plugin runtime、两个 fiber**：`runtime` 按 definition 身份
  （可比较指针）索引，改这里要连带看 `TestSamePluginLoadedTwice`。
- **`examples/*` 是活文档**：新增能力时同步加示例，并保证每个示例都能直接 `go run`。

## 目录

```
context.go      # Context：查找、隔离、Fork、生命周期
fiber.go        # Fiber：状态机、epoch、加载/卸载
service.go      # 服务注册、查找、依赖者通知
registry.go     # 插件定义（definition 擦除边界）与 runtime 注册表
events.go       # 事件总线与五种分发模式
logger.go       # 轻量日志服务
disposable.go   # 幂等 Disposer 与 effect 列表
loader/         # 配置驱动装配、patch 层、config dump
cmd/cordis/     # config dump CLI
examples/       # 可运行示例（活文档）
Makefile        # fmt / vet / lint / test / ci
.revive.toml    # 风格规则（含 100 列上限）
.github/        # CI：Go 1.27 + make ci
```

## 提交前

```sh
make ci    # 必须绿
```

一个提交一个逻辑变更，并且**每个提交都应能独立 `go build ./... && go test ./...`**——按可独立
评审、独立合入的粒度组织提交。
