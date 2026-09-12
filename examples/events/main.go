// Command events demonstrates every cordis-go event dispatch mode side by side:
// Emit, Bail, Serial, Parallel, Waterfall, and the *Scoped variant of each.
package main

import (
	"fmt"
	"time"

	cordis "github.com/tr1v3r/cordis-go"
)

type tick struct{ n int }
type ask struct{ q string }

func section(title string) { fmt.Printf("\n== %s ==\n", title) }

func main() {
	root := cordis.New()

	// ---------------------------------------------------------------- Emit --
	// 广播：所有监听者同步执行，返回值被忽略，没有回传通道。
	section("1. Emit — 广播，忽略返回值")
	cordis.On(root, "tick", func(t tick) { fmt.Printf("  A saw tick %d\n", t.n) })
	cordis.On(root, "tick", func(t tick) { fmt.Printf("  B saw tick %d\n", t.n) })
	cordis.Emit(root, "tick", tick{n: 1})

	// ---------------------------------------------------------- EmitScoped --
	// 只投递给「同一隔离作用域」内的监听者（同一 label 的两个 ctx 共享作用域）。
	section("2. EmitScoped — 作用域内广播")
	cn1 := root.IsolateShared("region", "cn")
	cn2 := root.IsolateShared("region", "cn")
	us := root.IsolateShared("region", "us")

	cordis.On(cn1, "beat", func(s string) { fmt.Println("  cn1 got", s) })
	cordis.On(cn2, "beat", func(s string) { fmt.Println("  cn2 got", s) })
	cordis.On(us, "beat", func(s string) { fmt.Println("  us  got", s) })
	cordis.On(root, "beat", func(s string) { fmt.Println("  root got", s) })
	cordis.On(root, "beat", func(s string) { fmt.Println("  GLOBAL got", s) }, cordis.Global())

	cordis.EmitScoped(cn1, "region", "beat", "hello-cn") // cn1 同 label 的 cn2 + Global
	fmt.Println("  -- 不限作用域的 Emit 会打到所有人 --")
	cordis.Emit(root, "beat", "hello-all")

	// ---------------------------------------------------------------- Bail --
	// 顺序执行，第一个返回非 nil / 非 false 的监听者胜出并中断后续。
	section("3. Bail — 首个命中者胜出")
	cordis.OnValue(root, "ask", func(a ask) any {
		if a.q == "cache" {
			return "hit:from-cache"
		}
		return nil // 不表态，交给下一个
	})
	cordis.OnValue(root, "ask", func(a ask) any { return "fallback:" + a.q })
	cordis.OnValue(root, "ask", func(ask) any { return "never-reached" })

	value, bailed := cordis.Bail(root, "ask", ask{q: "cache"})
	fmt.Printf("  ask(cache) -> value=%v bailed=%v\n", value, bailed)
	value, bailed = cordis.Bail(root, "ask", ask{q: "other"})
	fmt.Printf("  ask(other) -> value=%v bailed=%v\n", value, bailed)

	// ---------------------------------------------------------- BailScoped --
	section("4. BailScoped — 作用域内 Bail")
	left := root.Isolate("db")
	right := root.Isolate("db")
	cordis.OnValue(left, "pick", func(string) any { return "left" })
	cordis.OnValue(right, "pick", func(string) any { return "right" })
	cordis.OnValue(root, "pick", func(string) any { return "root" })
	value, _ = cordis.BailScoped(left, "db", "pick", "x")
	fmt.Println("  BailScoped(left)  ->", value)
	value, _ = cordis.BailScoped(right, "db", "pick", "x")
	fmt.Println("  BailScoped(right) ->", value)

	// -------------------------------------------------------------- Serial --
	section("5. Serial — 与 Bail 完全等价（Cordis 叫法的别名）")
	value, bailed = cordis.Serial(root, "ask", ask{q: "cache"})
	fmt.Printf("  Serial == Bail: value=%v bailed=%v\n", value, bailed)
	value, bailed = cordis.SerialScoped(left, "db", "pick", "x")
	fmt.Printf("  SerialScoped == BailScoped: value=%v bailed=%v\n", value, bailed)

	// ------------------------------------------------------------ Parallel --
	// 每个监听者一个 goroutine 并发跑；panic 被收集，用 errors.Join 汇总返回。
	section("6. Parallel — 并发执行 + 错误汇总")
	cordis.On(root, "job", func(string) { time.Sleep(60 * time.Millisecond) })
	cordis.On(root, "job", func(string) { time.Sleep(30 * time.Millisecond) })
	cordis.On(root, "job", func(string) { panic("worker C died") })

	start := time.Now()
	err := cordis.Parallel(root, "job", "x")
	fmt.Printf("  耗时 %v（串行会是 90ms+），err=%v\n", time.Since(start).Round(10*time.Millisecond), err)

	// ------------------------------------------------------ ParallelScoped --
	section("7. ParallelScoped — 作用域内并发")
	scoped := root.Isolate("worker")
	cordis.On(scoped, "fan", func(string) { time.Sleep(20 * time.Millisecond) })
	cordis.On(scoped, "fan", func(string) { panic("scoped worker died") })
	cordis.On(root, "fan", func(string) { fmt.Println("  这条属于 root 作用域，Scoped 分发不会到它") })
	err = cordis.ParallelScoped(scoped, "worker", "fan", "x")
	fmt.Println("  err:", err)

	// ----------------------------------------------------------- Waterfall --
	// 洋葱模型：监听者调 next 继续向内，不调 next 即否决；最外层返回值胜出。
	section("8. Waterfall — 中间件链")
	cordis.OnWaterfall(root, "render", func(s string, next func(string) any) any {
		return "auth(" + next(s+"+auth").(string) + ")" // 先做事，再进内层
	})
	cordis.OnWaterfall(root, "render", func(s string, next func(string) any) any {
		return "log(" + next(s+"+log").(string) + ")"
	})
	final := func(s string) any { return "core:" + s }
	fmt.Println("  ", cordis.Waterfall(root, "render", "req", final))

	// 不调 next -> 否决剩余的链和 final。
	cordis.OnWaterfall(root, "veto", func(s string, _ func(string) any) any { return "rejected:" + s })
	fmt.Println("  veto:", cordis.Waterfall(root, "veto", "req", final))

	// ---------------------------------------------------- WaterfallScoped --
	section("9. WaterfallScoped — 作用域内中间件链")
	scopedLeft := root.IsolateShared("mw", "a")
	scopedRight := root.IsolateShared("mw", "b")
	cordis.OnWaterfall(scopedLeft, "pipe", func(s string, next func(string) any) any {
		return "L[" + next(s).(string) + "]"
	})
	cordis.OnWaterfall(scopedRight, "pipe", func(s string, next func(string) any) any {
		return "R[" + next(s).(string) + "]"
	})
	fmt.Println("  ", cordis.WaterfallScoped(scopedLeft, "mw", "pipe", "x", final))
	fmt.Println("  ", cordis.WaterfallScoped(scopedRight, "mw", "pipe", "x", final))

	// ------------------------------------------------- 一次性 / 前置 / 注册辅助 --
	section("10. 注册侧辅助：OnOnce / Prepend / Global")
	cordis.OnOnce(root, "once", func(string) { fmt.Println("  once listener ran") })
	cordis.Emit(root, "once", "a")
	cordis.Emit(root, "once", "b") // 不再触发

	cordis.On(root, "order", func(string) { fmt.Println("  second (正常追加)") })
	cordis.On(root, "order", func(string) { fmt.Println("  first  (Prepend 插到队首)") }, cordis.Prepend())
	cordis.Emit(root, "order", "x")

	// 监听者 panic 不会打断其他监听者，只会被记进日志/Parallel 的 error。
	section("11. 监听者 panic 的隔离")
	cordis.On(root, "safe", func(string) { panic("boom") })
	cordis.On(root, "safe", func(string) { fmt.Println("  仍在执行：Emit 吞掉 panic 并继续") })
	cordis.Emit(root, "safe", "x")
}
