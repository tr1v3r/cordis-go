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

	// -------------------------------------------------------- two spellings --
	// Registration and dispatch are both Context methods; the package-level
	// function of the same name forwards to it and behaves identically:
	//
	//	root.On("tick", func(t tick) { ... })   // method form (preferred)
	//	cordis.On(root, "tick", ...)            // function form, context first
	//	root.Emit("tick", t)
	//	cordis.Emit(root, "tick", t)
	//
	// Keep the function form for values: a generic method has to be instantiated
	// before it can be passed around.
	section("0. registration and dispatch are both ctx methods")
	root.On("greet", func(s string) { fmt.Println("  got:", s) }) // method form
	root.Emit("greet", "method form")                             // method form
	cordis.Emit(root, "greet", "function form")                   // function form, equivalent

	// ----------------------------------------------------------------- Emit --
	// Broadcast: every listener runs synchronously, return values are ignored.
	section("1. Emit — broadcast, return values ignored")
	root.On("tick", func(t tick) { fmt.Printf("  A saw tick %d\n", t.n) })
	root.On("tick", func(t tick) { fmt.Printf("  B saw tick %d\n", t.n) })
	root.Emit("tick", tick{n: 1})

	// ----------------------------------------------------------- EmitScoped --
	// Delivered only to listeners in the same isolation scope (two contexts
	// isolated under one label share a scope).
	section("2. EmitScoped — scoped broadcast")
	cn1 := root.IsolateShared("region", "cn")
	cn2 := root.IsolateShared("region", "cn")
	us := root.IsolateShared("region", "us")

	cn1.On("beat", func(s string) { fmt.Println("  cn1 got", s) })
	cn2.On("beat", func(s string) { fmt.Println("  cn2 got", s) })
	us.On("beat", func(s string) { fmt.Println("  us  got", s) })
	root.On("beat", func(s string) { fmt.Println("  root got", s) })
	root.On("beat", func(s string) { fmt.Println("  GLOBAL got", s) }, cordis.Global())

	cn1.EmitScoped("region", "beat", "hello-cn") // cn2 shares the label; Global always hears it
	fmt.Println("  -- an unscoped Emit reaches everyone --")
	root.Emit("beat", "hello-all")

	// ----------------------------------------------------------------- Bail --
	// Sequential: the first listener returning non-nil / non-false wins and
	// stops the dispatch.
	section("3. Bail — first hit wins")
	root.OnValue("ask", func(a ask) any {
		if a.q == "cache" {
			return "hit:from-cache"
		}
		return nil // abstain, let the next listener answer
	})
	root.OnValue("ask", func(a ask) any { return "fallback:" + a.q })
	root.OnValue("ask", func(ask) any { return "never-reached" })

	// function form: cordis.Bail(root, "ask", ...)
	value, bailed := root.Bail("ask", ask{q: "cache"})
	fmt.Printf("  ask(cache) -> value=%v bailed=%v\n", value, bailed)
	value, bailed = root.Bail("ask", ask{q: "other"})
	fmt.Printf("  ask(other) -> value=%v bailed=%v\n", value, bailed)

	// ----------------------------------------------------------- BailScoped --
	section("4. BailScoped — scoped bail")
	left := root.Isolate("db")
	right := root.Isolate("db")
	left.OnValue("pick", func(string) any { return "left" })
	right.OnValue("pick", func(string) any { return "right" })
	root.OnValue("pick", func(string) any { return "root" })
	value, _ = left.BailScoped("db", "pick", "x")
	fmt.Println("  BailScoped(left)  ->", value)
	value, _ = right.BailScoped("db", "pick", "x")
	fmt.Println("  BailScoped(right) ->", value)

	// --------------------------------------------------------------- Serial --
	section("5. Serial — an alias of Bail, kept for the Cordis spelling")
	value, bailed = root.Serial("ask", ask{q: "cache"})
	fmt.Printf("  Serial == Bail: value=%v bailed=%v\n", value, bailed)
	value, bailed = left.SerialScoped("db", "pick", "x")
	fmt.Printf("  SerialScoped == BailScoped: value=%v bailed=%v\n", value, bailed)

	// ------------------------------------------------------------- Parallel --
	// One goroutine per listener; panics are collected and joined into the
	// returned error.
	section("6. Parallel — concurrent, errors joined")
	root.On("job", func(string) { time.Sleep(60 * time.Millisecond) })
	root.On("job", func(string) { time.Sleep(30 * time.Millisecond) })
	root.On("job", func(string) { panic("worker C died") })

	start := time.Now()
	err := root.Parallel("job", "x")
	fmt.Printf("  took %v (sequential would be 90ms+), err=%v\n",
		time.Since(start).Round(10*time.Millisecond), err)

	// ------------------------------------------------------- ParallelScoped --
	section("7. ParallelScoped — scoped concurrency")
	scoped := root.Isolate("worker")
	scoped.On("fan", func(string) { time.Sleep(20 * time.Millisecond) })
	scoped.On("fan", func(string) { panic("scoped worker died") })
	root.On("fan", func(string) {
		fmt.Println("  this one is in the root scope, so Scoped skips it")
	})
	err = scoped.ParallelScoped("worker", "fan", "x")
	fmt.Println("  err:", err)

	// ------------------------------------------------------------ Waterfall --
	// Onion model: a listener calls next to continue inward, and skipping next
	// vetoes the rest; the outermost return value wins.
	section("8. Waterfall — middleware chain")
	root.OnWaterfall("render", func(s string, next func(string) any) any {
		return "auth(" + next(s+"+auth").(string) + ")" // act, then descend
	})
	root.OnWaterfall("render", func(s string, next func(string) any) any {
		return "log(" + next(s+"+log").(string) + ")"
	})
	final := func(s string) any { return "core:" + s }
	fmt.Println("  ", root.Waterfall("render", "req", final))

	// Not calling next vetoes both the remaining listeners and final.
	root.OnWaterfall("veto", func(s string, _ func(string) any) any {
		return "rejected:" + s
	})
	fmt.Println("  veto:", root.Waterfall("veto", "req", final))

	// ------------------------------------------------------ WaterfallScoped --
	section("9. WaterfallScoped — scoped middleware chain")
	scopedLeft := root.IsolateShared("mw", "a")
	scopedRight := root.IsolateShared("mw", "b")
	scopedLeft.OnWaterfall("pipe", func(s string, next func(string) any) any {
		return "L[" + next(s).(string) + "]"
	})
	scopedRight.OnWaterfall("pipe", func(s string, next func(string) any) any {
		return "R[" + next(s).(string) + "]"
	})
	fmt.Println("  ", scopedLeft.WaterfallScoped("mw", "pipe", "x", final))
	fmt.Println("  ", scopedRight.WaterfallScoped("mw", "pipe", "x", final))

	// ------------------------------------------------- registration helpers --
	section("10. registration helpers: OnOnce / Prepend / Global")
	root.OnOnce("once", func(string) { fmt.Println("  once listener ran") })
	root.Emit("once", "a")
	root.Emit("once", "b") // no longer fires

	root.On("order", func(string) { fmt.Println("  second (appended)") })
	root.On("order", func(string) {
		fmt.Println("  first  (Prepend moves it to the front)")
	}, cordis.Prepend())
	root.Emit("order", "x")

	// A panicking listener does not interrupt the others; it is logged, and
	// Parallel reports it through the returned error.
	section("11. panicking listener isolation")
	root.On("safe", func(string) { panic("boom") })
	root.On("safe", func(string) {
		fmt.Println("  still runs: Emit logs the panic and continues")
	})
	root.Emit("safe", "x")
}
