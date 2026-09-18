# Fiber 失败、重试与并发收敛流程

```text
【正常激活】

fiber.load(bindings, epoch)
  ├─ live=true
  ├─ setState(loading)
  ├─ 再次检查每个 binding.live()
  │    ├─ 全部存活 → 继续
  │    └─ 任一失效
  │         ├─ live=false
  │         ├─ epoch=epochInactive
  │         ├─ setState(pending)
  │         └─ refresh() 重试
  ├─ 提交 epoch + resolvedServices snapshot
  ├─ resolveConfig(rawConfig)
  ├─ 保存验证后的 config
  ├─ runtime.definition.Run(ctx, config)
  └─ 成功 → err=nil → setState(active)

只有通过二次 liveness check 后才提交 epoch
  └─ 从未运行插件体的 generation 不会被误记成已加载


【错误与 panic 边界】

ResolveConfig(raw)
  ├─ 返回 error → fail(err)
  └─ panic       → recover → error → fail(err)

Plugin Run(ctx, config)
  ├─ 返回 error → fail(err)
  └─ panic       → recover → error → fail(err)

Fiber.fail(err)
  ├─ unload()
  │    ├─ loading → unloading
  │    ├─ 清理本代已注册的全部 effects
  │    ├─ 注销本代已提供的 services/listeners/children
  │    └─ unloading → pending
  ├─ 保存 Fiber.err
  ├─ 若已 Dispose → 保持销毁路径，不再写 failed
  └─ 否则 setState(failed) + 记录日志

结果
  └─ 失败插件不会遗留半注册服务或副作用


【Failed 不是终态】

StateFailed
  ├─ Fiber.Error()                  → 返回上次失败
  ├─ 相同依赖/相同配置              → 不会无条件自动重试
  ├─ Fiber.Update(newRawConfig)
  │    ├─ rawConfig=newRawConfig
  │    ├─ err=nil
  │    ├─ forceReload=true
  │    └─ refresh()
  ├─ Fiber.Restart()
  │    ├─ 保留 rawConfig
  │    ├─ forceReload=true
  │    └─ refresh()
  └─ 依赖先失效再恢复
       ├─ epoch → epochInactive
       └─ 恢复后 epoch 再变化 → load()

StateDisposed
  └─ Update / Restart → INACTIVE_EFFECT               // 唯一不可重启终态


【强制重载】

sync()
  └─ forceReload=true
       └─ reload()
            ├─ unload 当前 generation（幂等）
            ├─ epoch=epochInactive
            ├─ err=nil
            ├─ 重新 resolveInjections()
            ├─ 依赖不全 → 保持 pending/failed 后的干净状态
            └─ 依赖齐全 → load(new bindings, new epoch)

重载必须先 unload 再重新解析
  └─ unload 可能注销服务并改变整个 registry
       └─ 禁止复用卸载前解析出的 bindings


【并发 refresh 收敛】

任意触发源
  ├─ 服务 Provide / unregister / Set
  ├─ Provider active 状态变化
  ├─ Restart / Update
  └─ Dispose
       ↓
    fiber.refresh()
       └─ beginTransition()
            ├─ busy=false
            │    └─ busy=true，当前调用者成为 transition owner
            └─ busy=true
                 ├─ dirty=true
                 └─ 立即返回，不启动并行迁移

transition owner.drive()
  └─ 循环
       ├─ dirty=false
       ├─ sync() 执行一个完整 pass
       ├─ dirty=true  → 保持所有权，再执行一轮
       └─ dirty=false → busy=false，释放所有权

结果
  ├─ 同一 Fiber 不会并发 load/unload
  ├─ 迁移期间的新变化不会丢失
  └─ 高频通知被收敛为必要的后续 pass


【Dispose 与在途迁移竞争】

Fiber.Dispose()
  ├─ disposed=true + dirty=true
  ├─ 立即 cancel lifecycle Context + close Done
  ├─ 立即从 runtime registry 移除
  └─ refresh()
       ├─ 无在途 owner → 当前调用直接 unload/finalize
       └─ 已有 owner   → owner 下一 pass 看到 disposed
                            └─ Dispose 优先于 forceReload 和依赖变化

setState(state)
  └─ disposed 后只允许 StateUnloading / StateDisposed
       └─ 在途成功或失败不能把终态改回 active/failed


【状态主路径】

首次加载
  ├─ 依赖不全：pending
  └─ 依赖齐全：pending → loading → active

插件失败
  └─ loading → unloading → pending → failed

依赖消失
  └─ active → unloading → pending

依赖替换 / Restart / Update
  └─ active → unloading → pending → loading → active/failed

永久销毁
  └─ 任意非终态 → unloading → disposed

核心实现
  ├─ fiber.go                  → refresh/drive/sync/load/fail/reload/Dispose
  ├─ fiber_epoch_test.go       → 瞬时失效与 epoch 提交边界
  └─ fiber_lifecycle_test.go   → 并发迁移与终态竞争
```
