# HISTORY — 演变记录（整理版，非 git log 复制）

仓库 69 commits / ~60 PRs（2026-09-10 ~ 09-18）。只记方向性变化与
被踩过的坑。日期 2026-10-12。证据：A=提交可回查；C=推测。

## H-001：初始移植与当天补漏（2026-09-10，ba70b18）
变化：从 Cordis v4 整体移植——context tree、fiber 状态机、可逆
effects、epoch 依赖重载、隔离 scope、事件分发；JS 动态性（Proxy
查找、import()）换成显式类型访问与编译期注册（A：提交正文）。
同日两个跟进修复构成完整故事：插件失败必须回滚半注册 effects
（be2f462）；同步 Load 没有后续 await 点，失败必须当场返回（69daa63）。
当前影响：失败路径「先卸载再进 failed」是刻意语义，改它=回归。

## H-002：API 形态三天内三次破坏性重排（2026-09-12~13）
f493ea3：dispatch/load 改 Context 泛型方法，Definition 去导出；
d0341d9：事件注册改方法（无调用者的 untyped On 直接删）；
b9b1a80：类型化服务访问改方法（untyped Get/Provide 挡住 Get[T]，
让位给 Lookup/Set）。共同根因：Go 方法集一名一方法，泛型方法与
untyped 同名方法互斥（A：三个提交正文均写明）。
当前影响：再给 Context 加无类型 Get/Provide 会让整包编译不过
（AGENTS.md 已列为禁令）。

## H-003：并发安全攻坚主线（#17→#18→#19→#47→#56）
一条从「隐式共享状态」到「显式所有权」的演进：
- #17（00b1e74）：状态迁移 race-free 化；
- #18（2093b0b，BREAKING）：重做转换协议（busy/dirty 单主驱动），
  删除 Fiber.Disposed 完成信号；
- #19（d7688b8）：嵌套 effect 可达性——panic body 的 children
  曾永远不被回收；并发注册互相收养；
- #47（724dc4b）：并发 Load 同一插件互踩 effect scope（集成测试在
  双核 runner 上抓到）；
- #56（ad48534，BREAKING）：归属彻底显式化（见 D-004）。
当前影响：改任何注册/装载路径必须显式传 owner；「恢复隐式标记」
类简化都是重蹈覆辙（A 全链可回查）。

## H-004：依赖解析语义修复群（2026-09-13）
- #20（62754d9）：epoch 在代际未启动前提交 → fiber 永卡 loading；
  改为活性质检通过才随快照一起提交，失败归还 claim 回 pending。
- #21（635f52a）：epoch 加 binding seq（见 D-005）。
- #27（14b0ae3）：start 失败的 load 留幻影 runtime；引入 claims 计数。
- #30（b729b30）：scope label 单独作键导致跨名别名——"b" 取到 "a"
  的服务；读路径全部加 name 校验。
当前影响：epoch 提交时机与 claim 归还是最容易改坏的两处不变量。

## H-005：loader「fail-loud」系列（2026-09-13~14）
#22 失败条目泄漏 fiber；#23 非组条目 children 静默丢弃；#24 重复 id
索引与 Find 不一致；#26 insert 与其他字段混写丢一半配置；#28 基层
嵌套 insert 不展开；#32 空 config 绕过 UnmarshalJSON；#33 JSON 大数
取整；#34 dump/标签/配置拷贝加固；#54 结构化插入后索引重建。
（C：归纳）共同主题：Cordis 上游"静默丢弃"类行为在本库一律改为
警告或报错——Tree.Warnings 的存在本身即此立场（A：loader.go 注释
点名 include plugin 的静默丢弃是 recurring bug 源）。
当前影响：给 Compose 加"宽容跳过"逻辑前先看这条。

## H-006：事件分发「恰好一次」语义修复群（2026-09-13~14）
#25（3b2ab8d）once-listener 并发双发；#29（9e3a210）waterfall 链
双结算（panic 后 final 跑两次）；#38（b7ab88a）listener panic 重复
记日志；#53（4acc697）waterfall 游标/闩锁 race-free 化。
另有一条有意的上游差异：监听器 panic 不中断分发，记日志继续
（Parallel 把 panic 并入返回 error）（A：README「与 Cordis 的差异」）。
当前影响：分发路径新增模式时必须过 once/单结算约束。

## H-007：宿主生命周期集成（2026-09-13，87e5efe）
WithBaseContext：root 生命周期派生自宿主 context，其取消=整树
Dispose——刻意不存在"goroutine 已停但服务/监听仍注册"的中间态；
AfterFunc 不常驻 goroutine，watcher 注册为 root effect 随应用释放。
当前影响：取消与销毁合一是文档化契约，拆开它们=引入泄漏窗口。

## H-008：质量门禁建设（2026-09-13~14）
#49 goleak 双包 TestMain（生命周期机器按设计停泊 goroutine，泄漏
测试看不见——net 必须有）；#50 配置路径 fuzz；713886c 90 场景基准
+ 基线回归门禁；#59（a5ceb84）记录一个测得的负优化：给 lookup
快照换 RWMutex 反而慢 24%（108→134ns，临界区只是单 map 读）。
当前影响：并发"优化"前先看基准；负结果也是决策依据，别重试。

## H-009：深潜文档沉淀（2026-09-18，2c24937 起）
八张流程/架构图 + docs/README.md 索引（A 级证据来源，本知识库
直接引用不重复）。此前 README/AGENTS 与实现同步过两轮
（0bcea51 #36、5baf6d8 #48）。
当前影响：改控制流后 docs/ 的图与 README 对应小节需同步（见 TODO）。
