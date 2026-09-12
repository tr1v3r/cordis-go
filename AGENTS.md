# AGENTS.md

本仓库面向编码代理的工作说明（人类贡献者同样适用）。使用者文档见 `README.md`——这里只写
**改代码时需要知道、而 README 不会告诉你**的东西。`CLAUDE.md` 是指向本文件的软链接。

## 先跑起来

```sh
make tools   # 一次性：安装固定版本的 staticcheck / revive
make ci      # 改完必须绿：gofmt 检查 + vet + staticcheck + revive + go test -race
make fmt     # 也可以单跑某一环：make fmt / vet / lint / test
```

**需要 Go 1.27+**（`go.mod` 声明 `go 1.27.0`）：事件分发与插件加载是泛型方法，属于 1.27 的
语言特性。

三个会骗人的坑：

1. **lint 工具必须用不低于 `go.mod` 的 Go 构建**。旧 Go 编译的 `staticcheck` 读不了新的 export
   data；旧 `revive` 会把泛型方法判成语法错误。别用系统里那个旧的，用 `make tools` 装的。
2. **`staticcheck` 在 `GOCACHE` 不可写时会静默空转**：只打印 `./... matched no packages` 然后
   exit 0。看到这句就当它没跑过，别当成通过。
3. **`revive` 传显式文件列表会把每个文件当独立包**，`package-comments` 必然误报。要复现 CI 就用
   包形态：`revive -config .revive.toml ./...`。

## 不能碰的约束

- **擦除边界**：`registry.go` 的 `definition` 是**非导出**接口，fiber 靠它在不认识配置类型的前提下
  调用 `ResolveConfig` / `Run`。不要重新导出它——`Definition` 已经删除（见 README 破坏性变更）。
- **泛型入口成对出现**：Context 方法 + 等价包级函数（`ctx.Emit` / `cordis.Emit`、`ctx.Load` /
  `cordis.Load`）。包级函数不是历史包袱——泛型方法必须先实例化才能当方法值，它是唯一通道。
  完整约定写在 `doc.go`。
- **这些名字没有方法形态**：`Get[T]` / `Provide[T]` / `ProvideChecked[T]`。原因是 `Context` 上
  同名**非泛型**方法已存在（运行期按名字取服务是刚需），而 Go 不允许泛型方法与非泛型方法同名
  ——方法集一个名字只能有一个方法。给某个操作加方法形态前，先确认 `Context` 上没有同名方法。
- **运行期才知道插件类型的宿主**（参考 `loader`）：在**注册时**用闭包固定类型参数
  （`Register[C]` 里把 `load` 闭包建好），运行期只调那个闭包。不要在加载时尝试擦除类型。
- **同一 definition 加载两次 = 一个 runtime、两个 fiber**：`runtime` 按 definition 指针身份索引，
  `TestSamePluginLoadedTwice` 守着这条语义。

## 风格

`.revive.toml` 是唯一权威（revive 默认规则集 + **100 列**上限）：

- Go 源码的注释与输出**一律英文**；`README.md` 是本仓库唯一的中文文件。
- 导出符号要有文档注释，以符号名开头、以句号结尾；非导出函数按需写，别为凑格式补噪音。
- 测试失败信息统一 `want X, got Y`。
- 提交信息用 Conventional Commits，正文讲**为什么**；破坏性变更加 `!` 并写 `BREAKING CHANGE:`。
- 一个提交一个逻辑变更，并且**每个提交都要能独立 `go build ./... && go test ./...`**——按可独立
  评审、独立合入的粒度组织提交。
- 别提交 `.vscode/`、`__debug_bin*`（delve 产物）这类本地产物，`.gitignore` 已覆盖，不要用
  `git add -f` 绕过去。

## 常见改动落在哪

| 要做什么 | 动哪里 |
| --- | --- |
| 加一种事件分发模式 | `events.go`：方法 + `*Scoped` 变体 + 包级转发三条，同步 `doc.go` 的约定段、`examples/events`、以及方法/函数形态的对拍测试 |
| 改公开 API 形态 | 先读 `doc.go` 的泛型约定，再确认 `Context` 上没有同名非泛型方法 |
| 加示例 | `examples/<名字>/main.go`，同步 README 的运行节与目录树，并保证能直接 `go run` |
| 改配置装配 | `loader/loader.go`——`Register[C]` 的注册期擦除是关键 |
| 调 lint 规则 | `.revive.toml`；规则选项语法是 `arguments = [...]` 而不是 `max = ...`（写错会静默退回默认 80 列） |
| 调 CI | `Makefile` 与 `.github/workflows/ci.yml`，本地先 `make ci` 验证 |
