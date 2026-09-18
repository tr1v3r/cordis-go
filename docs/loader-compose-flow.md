# Loader 配置装配与加载流程

```text
【编译期插件注册】

loader.Register[C](registry, name, plugin)
  ├─ 校验 registry/name/plugin
  ├─ 拒绝重复 name
  └─ 注册 registeredPlugin.load 闭包
       ├─ 闭包捕获 *cordis.Plugin[C]
       ├─ decodeConfig[C](map[string]any)
       └─ cordis.LoadWithInject(ctx, plugin, typedConfig, extra...)

类型擦除边界
  └─ Registry map 只保存无泛型 load 闭包
       └─ C 已在 Register[C] 时固定
            └─ 运行时无需按插件名动态决定 Go 类型


【配置层解析】

ParseLayer(label, JSON)
  └─ 生成 Layer{Patch:false}                       // base：创建节点

ParsePatchLayer(label, JSON)
  └─ 生成 Layer{Patch:true}                        // patch：修改已有节点

parseEntries()
  ├─ JSON 顶层必须是单个数组
  ├─ Decoder.UseNumber()
  └─ 数字保留为 json.Number，避免超过 2^53 后静默取整


【配置层组合】

Compose([base, profile, override...])
  └─ 按层顺序处理
       ├─ validateLayer()
       │    └─ 同一 entry 不能同时声明 insert 和普通字段
       ├─ Base Layer
       │    └─ applyBase()：创建 Node，建立全树 id 索引
       └─ Patch Layer
            ├─ entry.insert → 追加新 Node
            └─ entry.id     → 在全树索引中定位并 mergePatch()

mergePatch(node, patch)
  ├─ 仅覆盖 patch 明确出现的字段
  ├─ inject 数组整体替换
  └─ config map 整体替换                              // 不做字段级深合并

未命中 id / 重复 id / 非 group 携带 children
  ├─ 默认 → Tree.Warnings
  └─ Strict() → Compose 直接返回 error


【配置树加载】

Tree.Load(ctx, registry)
  └─ loadNodes(ctx, rootNodes, inheritedDeps=[])
       ├─ nil Node      → 跳过
       ├─ disabled Node → 跳过该节点及整棵子树
       ├─ deps = inheritedDeps + node.Inject
       │
       ├─ node.Group=true
       │    ├─ label = node.Label || node.ID || "group"
       │    ├─ childCtx = ctx.Fork(label)                  // 命名子 Context，不自动隔离服务
       │    └─ 递归 children，并向全部后代传递 deps
       │
       └─ 普通插件 Node
            ├─ registry.get(node.Name)
            │    └─ 未注册 → error
            ├─ 普通 Node 带 children → error
            └─ registeredPlugin.load(ctx, node.Config, deps)
                 ├─ map config → JSON → C
                 ├─ C 的 UnmarshalJSON/default 逻辑生效
                 └─ LoadWithInject(plugin, typedConfig, inherited+own inject)

依赖未满足
  └─ Fiber pending，但不算 Tree.Load 失败


【全有或全无回滚】

任一 Node 加载失败
  ├─ 若失败调用已返回 Fiber → 先 Dispose 该失败 Fiber
  ├─ 已成功/已 pending 的 fibers 按逆序 Dispose
  └─ Tree.Load 返回 nil + 包装后的 entry error

全部成功
  └─ 返回 fibers[]
       └─ 每个 Fiber 都挂在传入 ctx 的显式 Effect ownership 下


【配置值路径】

JSON config
  ↓  Parse：map[string]any + json.Number
Node.Config
  ↓  patch 时整体替换并深拷贝容器
registeredPlugin.load
  ↓  json.Marshal(map)
decodeConfig[C]
  ↓  json.Decoder.Decode(&config)
typed C
  ↓  cordis.LoadWithInject
Plugin.ResolveConfig / Validate
  ↓
Plugin Apply

nil config
  └─ C 零值

空对象 config: {}
  └─ 仍执行 JSON decode
       └─ 指针配置可被分配，自定义 UnmarshalJSON 可应用默认值


【核心关系】

配置只负责
  └─ 选择插件 + 组合配置 + 补充 inject

Go 代码负责
  └─ 在编译期把插件名注册到类型固定的 load 闭包

Group
  └─ 传播 Context 名称与 inject，不等于 isolate

失败
  └─ 整棵 Tree 回滚，不留下半加载应用

核心实现
  ├─ loader/loader.go  → Parse、Compose、Register、Tree.Load
  ├─ loader/dump.go    → 组合结果与来源输出
  └─ registry.go       → LoadWithInject 与 Fiber 创建
```
