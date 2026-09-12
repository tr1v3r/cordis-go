package cordis_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	cordis "github.com/tr1v3r/cordis-go"
)

type fakeDB struct{ name string }

func TestProvideAndGet(t *testing.T) {
	root := cordis.New()
	database := &fakeDB{name: "a"}

	if _, err := cordis.Provide[*fakeDB](root, "db", database); err != nil {
		t.Fatalf("provide: %v", err)
	}
	got, ok := cordis.Get[*fakeDB](root, "db")
	if !ok || got != database {
		t.Fatalf("get: got %v ok=%v", got, ok)
	}
	if _, ok := cordis.Get[*fakeDB](root, "missing"); ok {
		t.Fatal("missing service must not resolve")
	}
	if _, ok := cordis.Get[*fakeDB](root, "db"); !ok {
		t.Fatal("typed lookup failed")
	}
}

func TestProvideTwiceFails(t *testing.T) {
	root := cordis.New()
	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "a"}); err != nil {
		t.Fatal(err)
	}
	_, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "b"})
	var code *cordis.Error
	if !errors.As(err, &code) || code.Code != cordis.ErrServiceExists {
		t.Fatalf("want SERVICE_EXISTS, got %v", err)
	}
}

func TestPluginWaitsForInjectedService(t *testing.T) {
	root := cordis.New()
	activations := 0
	plugin := cordis.Define[struct{}]("consumer", func(_ *cordis.Context, _ struct{}) error {
		activations++
		return nil
	}).WithInject("db")

	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if fiber.State() != cordis.StatePending {
		t.Fatalf("want pending, got %s", fiber.State())
	}
	if activations != 0 {
		t.Fatalf("plugin must not run before its dependency exists")
	}

	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "a"}); err != nil {
		t.Fatal(err)
	}
	if fiber.State() != cordis.StateActive {
		t.Fatalf("want active, got %s", fiber.State())
	}
	if activations != 1 {
		t.Fatalf("want 1 activation, got %d", activations)
	}
}

func TestDependentReloadsWhenProviderChanges(t *testing.T) {
	root := cordis.New()
	var seen []string
	plugin := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		database, _ := cordis.Get[*fakeDB](ctx, "db")
		seen = append(seen, database.name)
		ctx.OnDispose(func() { seen = append(seen, "off:"+database.name) })
		return nil
	}).WithInject("db")

	disposeA, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cordis.Load(root, plugin, struct{}{}); err != nil {
		t.Fatal(err)
	}

	// Removing the provider must unload the dependent; providing a replacement
	// must load it again with the new provider.
	disposeA()
	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "b"}); err != nil {
		t.Fatal(err)
	}

	want := []string{"a", "off:a", "b"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("want %v, got %v", want, seen)
	}
}

func TestEffectsUnwindInReverseOrder(t *testing.T) {
	root := cordis.New()
	var order []string
	plugin := cordis.Define[struct{}]("p", func(ctx *cordis.Context, _ struct{}) error {
		ctx.OnDispose(func() { order = append(order, "first") })
		ctx.OnDispose(func() { order = append(order, "second") })
		ctx.OnDispose(func() { order = append(order, "third") })
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	fiber.Dispose()

	want := []string{"third", "second", "first"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("want %v, got %v", want, order)
	}
	if fiber.State() != cordis.StateDisposed {
		t.Fatalf("want disposed, got %s", fiber.State())
	}
}

func TestDisposingParentDisposesChildren(t *testing.T) {
	root := cordis.New()
	childPlugin := cordis.Define[struct{}]("child", func(*cordis.Context, struct{}) error {
		return nil
	})
	var childFiber *cordis.Fiber
	parentPlugin := cordis.Define[struct{}]("parent", func(ctx *cordis.Context, _ struct{}) error {
		var err error
		childFiber, err = cordis.Load(ctx, childPlugin, struct{}{})
		return err
	})

	parentFiber, err := cordis.Load(root, parentPlugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if childFiber.State() != cordis.StateActive {
		t.Fatalf("child should be active")
	}
	parentFiber.Dispose()
	if childFiber.State() != cordis.StateDisposed {
		t.Fatalf("want child disposed, got %s", childFiber.State())
	}
}

func TestIsolationScopes(t *testing.T) {
	root := cordis.New()
	left := root.Isolate("db")
	right := root.Isolate("db")

	if _, err := cordis.Provide[*fakeDB](left, "db", &fakeDB{name: "left"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cordis.Provide[*fakeDB](right, "db", &fakeDB{name: "right"}); err != nil {
		t.Fatal(err)
	}

	leftDB, _ := cordis.Get[*fakeDB](left, "db")
	rightDB, _ := cordis.Get[*fakeDB](right, "db")
	if leftDB.name != "left" || rightDB.name != "right" {
		t.Fatalf("isolation leaked: left=%v right=%v", leftDB, rightDB)
	}
	if _, ok := cordis.Get[*fakeDB](root, "db"); ok {
		t.Fatal("isolated service must not leak to the parent scope")
	}
}

func TestFailedPluginReportsError(t *testing.T) {
	root := cordis.New()
	boom := errors.New("boom")
	plugin := cordis.Define[struct{}]("bad", func(*cordis.Context, struct{}) error { return boom })
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if !errors.Is(err, boom) {
		t.Fatalf("Load must report the failure, got %v", err)
	}
	if fiber.State() != cordis.StateFailed {
		t.Fatalf("want failed, got %s", fiber.State())
	}
	if !errors.Is(fiber.Error(), boom) {
		t.Fatalf("want boom, got %v", fiber.Error())
	}
}

func TestPanicInPluginIsContained(t *testing.T) {
	root := cordis.New()
	plugin := cordis.Define[struct{}]("panic", func(*cordis.Context, struct{}) error {
		panic("kaboom")
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err == nil {
		t.Fatal("a panicking plugin must be reported as a failed load")
	}
	if fiber.State() != cordis.StateFailed {
		t.Fatalf("want failed, got %s", fiber.State())
	}
	if fiber.Error() == nil {
		t.Fatal("panic must be reported as an error")
	}
}

func TestPluginConfigValidation(t *testing.T) {
	root := cordis.New()
	type config struct{ Port int }
	plugin := cordis.Define[config]("server", func(*cordis.Context, config) error { return nil }).
		WithValidate(func(cfg *config) error {
			if cfg.Port == 0 {
				return errors.New("port is required")
			}
			return nil
		})

	fiber, err := cordis.Load(root, plugin, config{})
	if err == nil {
		t.Fatal("invalid config must fail the load")
	}
	if fiber.State() != cordis.StateFailed {
		t.Fatalf("want failed, got %s", fiber.State())
	}
	if _, err := cordis.Load(root, plugin, config{Port: 8080}); err != nil {
		t.Fatal(err)
	}
}

func TestEventDispatch(t *testing.T) {
	root := cordis.New()
	var order []string

	cordis.On[string](root, "msg", func(m string) { order = append(order, "first:"+m) })
	cordis.On[string](root, "msg", func(m string) { order = append(order, "second:"+m) })
	cordis.On[string](root, "msg", func(m string) { order = append(order, "prepend:"+m) },
		cordis.Prepend())
	cordis.Emit[string](root, "msg", "hi")

	want := []string{"prepend:hi", "first:hi", "second:hi"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("want %v, got %v", want, order)
	}

	cordis.OnOnce[string](root, "msg", func(m string) { order = append(order, "once:"+m) })
	cordis.Emit[string](root, "msg", "x")
	cordis.Emit[string](root, "msg", "y")
	fired := 0
	for _, item := range order {
		if item == "once:x" {
			fired++
		}
		if item == "once:y" {
			t.Fatalf("once listener fired again: %v", order)
		}
	}
	if fired != 1 {
		t.Fatalf("once listener must fire exactly once: %v", order)
	}
}

func TestBailAndWaterfall(t *testing.T) {
	root := cordis.New()

	cordis.OnValue[string](root, "ask", func(s string) any {
		if s == "known" {
			return "handled"
		}
		return nil
	})
	cordis.OnValue[string](root, "ask", func(string) any { return "fallback" })

	if value, bailed := cordis.Bail[string](root, "ask", "known"); !bailed || value != "handled" {
		t.Fatalf("want handled, got %v bailed=%v", value, bailed)
	}
	if value, bailed := cordis.Bail[string](root, "ask", "other"); !bailed || value != "fallback" {
		t.Fatalf("want fallback, got %v bailed=%v", value, bailed)
	}

	cordis.OnWaterfall[string](root, "cmd", func(s string, next func(string) any) any {
		if s == "blocked" {
			return "vetoed"
		}
		return next(s)
	})
	if got := cordis.Waterfall[string](root, "cmd", "blocked",
		func(string) any { return "final" }); got != "vetoed" {
		t.Fatalf("want vetoed, got %v", got)
	}
	if got := cordis.Waterfall[string](root, "cmd", "ok",
		func(s string) any { return "final:" + s }); got != "final:ok" {
		t.Fatalf("want final:ok, got %v", got)
	}
}

func TestListenersAreDisposedWithFiber(t *testing.T) {
	root := cordis.New()
	fired := 0
	plugin := cordis.Define[struct{}]("listener", func(ctx *cordis.Context, _ struct{}) error {
		cordis.On[string](ctx, "msg", func(string) { fired++ })
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	cordis.Emit[string](root, "msg", "one")
	fiber.Dispose()
	cordis.Emit[string](root, "msg", "two")
	if fired != 1 {
		t.Fatalf("want 1 dispatch, got %d", fired)
	}
}

func TestParallelEventCollectsPanics(t *testing.T) {
	root := cordis.New()
	cordis.On[string](root, "boom", func(string) { panic("listener exploded") })
	cordis.On[string](root, "boom", func(string) {})
	if err := cordis.Parallel[string](root, "boom", "x"); err == nil {
		t.Fatal("want an aggregated error")
	}
}

// Every dispatch mode is reachable both as a Context method and as a package
// function taking the context first; the two spellings must be interchangeable.
func TestMethodFormsMatchFunctionForms(t *testing.T) {
	root := cordis.New()

	// Emit
	var emitted []string
	cordis.On[string](root, "emit", func(v string) { emitted = append(emitted, v) })
	root.Emit("emit", "method")
	cordis.Emit(root, "emit", "function")
	if want := []string{"method", "function"}; !reflect.DeepEqual(emitted, want) {
		t.Fatalf("Emit forms differ: want %v, got %v", want, emitted)
	}

	// EmitScoped
	scoped := root.IsolateShared("region", "cn")
	var scopedEmitted []string
	cordis.On[string](scoped, "scoped", func(v string) { scopedEmitted = append(scopedEmitted, v) })
	scoped.EmitScoped("region", "scoped", "method")
	cordis.EmitScoped(scoped, "region", "scoped", "function")
	if want := []string{"method", "function"}; !reflect.DeepEqual(scopedEmitted, want) {
		t.Fatalf("EmitScoped forms differ: want %v, got %v", want, scopedEmitted)
	}

	// Bail
	cordis.OnValue[string](root, "bail", func(string) any { return "hit" })
	methodValue, methodBailed := root.Bail("bail", "x")
	functionValue, functionBailed := cordis.Bail(root, "bail", "x")
	if !methodBailed || methodValue != functionValue || methodBailed != functionBailed {
		t.Fatalf("Bail forms differ: (%v,%v) vs (%v,%v)",
			methodValue, methodBailed, functionValue, functionBailed)
	}

	// BailScoped
	isolated := root.IsolateShared("pick", "a")
	cordis.OnValue[string](isolated, "pick", func(string) any { return "scoped-hit" })
	value, bailed := isolated.BailScoped("pick", "pick", "x")
	scopedValue, scopedBailed := cordis.BailScoped(isolated, "pick", "pick", "x")
	if !bailed || value != "scoped-hit" || value != scopedValue || bailed != scopedBailed {
		t.Fatalf("BailScoped forms differ: (%v,%v) vs (%v,%v)",
			value, bailed, scopedValue, scopedBailed)
	}

	// Serial is Bail under another name, in both spellings.
	if v, ok := root.Serial("bail", "x"); !ok || v != methodValue {
		t.Fatalf("Serial method: got (%v,%v)", v, ok)
	}
	if v, ok := cordis.Serial(root, "bail", "x"); !ok || v != methodValue {
		t.Fatalf("Serial function: got (%v,%v)", v, ok)
	}
	if v, ok := isolated.SerialScoped("pick", "pick", "x"); !ok || v != "scoped-hit" {
		t.Fatalf("SerialScoped method: got (%v,%v)", v, ok)
	}
	if v, ok := cordis.SerialScoped(isolated, "pick", "pick", "x"); !ok || v != "scoped-hit" {
		t.Fatalf("SerialScoped function: got (%v,%v)", v, ok)
	}

	// Parallel
	var mu sync.Mutex
	var ran []string
	for _, name := range []string{"a", "b"} {
		cordis.On[string](root, "parallel", func(string) {
			mu.Lock()
			ran = append(ran, name)
			mu.Unlock()
		})
	}
	if err := root.Parallel("parallel", "x"); err != nil {
		t.Fatalf("Parallel method: %v", err)
	}
	if err := cordis.Parallel(root, "parallel", "x"); err != nil {
		t.Fatalf("Parallel function: %v", err)
	}
	if len(ran) != 4 {
		t.Fatalf("Parallel forms differ: %v", ran)
	}

	// ParallelScoped surfaces a listener panic in both spellings.
	panicky := root.IsolateShared("job", "p")
	cordis.On[string](panicky, "job", func(string) { panic("listener exploded") })
	if err := panicky.ParallelScoped("job", "job", "x"); err == nil {
		t.Fatal("ParallelScoped method must surface the panic")
	}
	if err := cordis.ParallelScoped(panicky, "job", "job", "x"); err == nil {
		t.Fatal("ParallelScoped function must surface the panic")
	}

	// Waterfall
	cordis.OnWaterfall[string](root, "render", func(s string, next func(string) any) any {
		return "wrap(" + next(s).(string) + ")"
	})
	final := func(s string) any { return "core:" + s }
	methodResult := root.Waterfall("render", "x", final)
	functionResult := cordis.Waterfall(root, "render", "x", final)
	if methodResult != functionResult || methodResult != "wrap(core:x)" {
		t.Fatalf("Waterfall forms differ: %v vs %v", methodResult, functionResult)
	}

	// WaterfallScoped
	scopedPipe := root.IsolateShared("mw", "a")
	cordis.OnWaterfall[string](scopedPipe, "pipe", func(s string, next func(string) any) any {
		return "L[" + next(s).(string) + "]"
	})
	methodResult = scopedPipe.WaterfallScoped("mw", "pipe", "x", final)
	functionResult = cordis.WaterfallScoped(scopedPipe, "mw", "pipe", "x", final)
	if methodResult != functionResult || methodResult != "L[core:x]" {
		t.Fatalf("WaterfallScoped forms differ: %v vs %v", methodResult, functionResult)
	}
}

func TestScopedEventFiltering(t *testing.T) {
	root := cordis.New()
	left := root.Isolate("db")
	right := root.Isolate("db")

	var seen []string
	cordis.On[string](left, "tick", func(v string) { seen = append(seen, "left:"+v) })
	cordis.On[string](right, "tick", func(v string) { seen = append(seen, "right:"+v) })
	cordis.On[string](root, "tick", func(v string) { seen = append(seen, "root:"+v) })
	cordis.On[string](root, "tick", func(v string) { seen = append(seen, "global:"+v) },
		cordis.Global())

	// Only listeners in the same isolation scope (plus Global ones) receive a
	// scoped dispatch: the root context has its own default scope for "db".
	cordis.EmitScoped[string](left, "db", "tick", "a")
	want := []string{"left:a", "global:a"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("want %v, got %v", want, seen)
	}
}

func TestSamePluginLoadedTwice(t *testing.T) {
	root := cordis.New()
	activations := 0
	plugin := cordis.Define[any]("p", func(*cordis.Context, any) error {
		activations++
		return nil
	})
	firstFiber, err := cordis.Load[any](root, plugin, 1)
	if err != nil {
		t.Fatal(err)
	}
	secondFiber, err := cordis.Load[any](root, plugin, 2)
	if err != nil {
		t.Fatal(err)
	}
	if activations != 2 {
		t.Fatalf("want 2 activations, got %d", activations)
	}
	firstFiber.Dispose()
	if secondFiber.State() != cordis.StateActive {
		t.Fatalf("disposing one fiber must not affect the other: %s", secondFiber.State())
	}
}

// Plugin loading follows the same rule as event dispatch: a Context method plus
// an equivalent package function. Both spellings must reach one plugin runtime.
func TestLoadMethodFormsMatchFunctionForms(t *testing.T) {
	root := cordis.New()
	activations := 0
	plugin := cordis.Define[any]("p", func(*cordis.Context, any) error {
		activations++
		return nil
	})

	first, err := root.Load(plugin, 1) // method form, C inferred from the plugin
	if err != nil {
		t.Fatal(err)
	}
	second, err := cordis.Load(root, plugin, 2) // function form
	if err != nil {
		t.Fatal(err)
	}
	if activations != 2 {
		t.Fatalf("want 2 activations, got %d", activations)
	}

	registry, ok := cordis.Get[cordis.Registry](root, "registry")
	if !ok {
		t.Fatal("registry service missing")
	}
	if size := registry.Size(); size != 1 {
		t.Fatalf("both spellings must share one plugin runtime, got %d", size)
	}
	if names := registry.Plugins(); len(names) != 1 || names[0] != "p" {
		t.Fatalf("want [p], got %v", names)
	}

	first.Dispose()
	if second.State() != cordis.StateActive {
		t.Fatalf("disposing one fiber must not affect the other: %s", second.State())
	}
	second.Dispose()

	// LoadWithInject gates on the extra service in both spellings.
	waiting := cordis.Define[struct{}]("waiting", func(*cordis.Context, struct{}) error {
		return nil
	})
	methodFiber, err := root.LoadWithInject(waiting, struct{}{}, "cache")
	if err != nil {
		t.Fatal(err)
	}
	functionFiber, err := cordis.LoadWithInject(root, waiting, struct{}{}, "cache")
	if err != nil {
		t.Fatal(err)
	}
	if methodFiber.State() != cordis.StatePending {
		t.Fatalf("method form: want pending without cache, got %s", methodFiber.State())
	}
	if functionFiber.State() != cordis.StatePending {
		t.Fatalf("function form: want pending without cache, got %s", functionFiber.State())
	}
	if _, err := root.Provide("cache", &fakeDB{name: "cache"}); err != nil {
		t.Fatal(err)
	}
	if methodFiber.State() != cordis.StateActive {
		t.Fatalf("method form: want active after cache, got %s", methodFiber.State())
	}
	if functionFiber.State() != cordis.StateActive {
		t.Fatalf("function form: want active after cache, got %s", functionFiber.State())
	}
}

func TestContextCancelledOnDispose(t *testing.T) {
	root := cordis.New()
	stopped := make(chan struct{})
	plugin := cordis.Define[struct{}]("worker", func(ctx *cordis.Context, _ struct{}) error {
		go func() {
			<-ctx.Context().Done()
			close(stopped)
		}()
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	fiber.Dispose()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled on dispose")
	}
}

func TestEffectOnDisposedContextPanics(t *testing.T) {
	root := cordis.New()
	plugin := cordis.Define[struct{}]("p", func(*cordis.Context, struct{}) error { return nil })
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	fiber.Dispose()

	defer func() {
		reason := recover()
		if reason == nil {
			t.Fatal("want panic")
		}
		err, ok := reason.(*cordis.Error)
		if !ok || err.Code != cordis.ErrInactiveEffect {
			t.Fatalf("want INACTIVE_EFFECT, got %v", reason)
		}
	}()
	fiber.Ctx.OnDispose(func() {})
}

func TestServiceOwnership(t *testing.T) {
	root := cordis.New()
	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "a"}); err != nil {
		t.Fatal(err)
	}
	plugin := cordis.Define[struct{}]("intruder", func(ctx *cordis.Context, _ struct{}) error {
		err := ctx.Set("db", &fakeDB{name: "hacked"})
		var code *cordis.Error
		if !errors.As(err, &code) || code.Code != cordis.ErrServiceOwnership {
			return errors.New("expected SERVICE_OWNERSHIP")
		}
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if fiber.State() != cordis.StateActive {
		t.Fatalf("want active, got %s: %v", fiber.State(), fiber.Error())
	}
}

func TestServiceAvailabilityCheck(t *testing.T) {
	root := cordis.New()
	ready := false
	if _, err := cordis.ProvideChecked[*fakeDB](root, "db", &fakeDB{name: "a"},
		func() bool { return ready }); err != nil {
		t.Fatal(err)
	}
	if _, ok := cordis.Get[*fakeDB](root, "db"); ok {
		t.Fatal("unavailable service must not resolve")
	}
	ready = true
	if _, ok := cordis.Get[*fakeDB](root, "db"); !ok {
		t.Fatal("service must resolve once available")
	}
}

func TestRestartUnloadsAndReloads(t *testing.T) {
	root := cordis.New()
	var events []string
	plugin := cordis.Define[struct{}]("p", func(ctx *cordis.Context, _ struct{}) error {
		events = append(events, "load")
		ctx.OnDispose(func() { events = append(events, "unload") })
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fiber.Restart(); err != nil {
		t.Fatal(err)
	}
	want := []string{"load", "unload", "load"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("want %v, got %v", want, events)
	}
}

func TestEffectMetadata(t *testing.T) {
	root := cordis.New()
	plugin := cordis.Define[struct{}]("p", func(ctx *cordis.Context, _ struct{}) error {
		cordis.On[string](ctx, "tick", func(string) {})
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	effects := fiber.Effects()
	if len(effects) != 1 || effects[0].Label != `ctx.On("tick")` {
		t.Fatalf("unexpected effects: %+v", effects)
	}
}

func TestPluginCanUseItsOwnService(t *testing.T) {
	root := cordis.New()
	seen := ""
	plugin := cordis.Define[struct{}]("self", func(ctx *cordis.Context, _ struct{}) error {
		if _, err := cordis.Provide[*fakeDB](ctx, "db", &fakeDB{name: "self"}); err != nil {
			return err
		}
		// Cordis makes a service visible to its own provider immediately; the
		// fiber store snapshot is what carries that here.
		got, ok := cordis.Get[*fakeDB](ctx, "db")
		if !ok {
			return errors.New("own service is not visible")
		}
		seen = got.name
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if fiber.State() != cordis.StateActive {
		t.Fatalf("want active, got %s: %v", fiber.State(), fiber.Error())
	}
	if seen != "self" {
		t.Fatalf("want self, got %q", seen)
	}
}

func TestTwoIsolatedProvidersOfSameNameOnRoot(t *testing.T) {
	root := cordis.New()
	left := root.Isolate("db")
	right := root.Isolate("db")
	if _, err := cordis.Provide[*fakeDB](left, "db", &fakeDB{name: "left"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cordis.Provide[*fakeDB](right, "db", &fakeDB{name: "right"}); err != nil {
		t.Fatal(err)
	}
	// The root fiber's snapshot can only hold one implementation per name, so
	// this exercises the scope-guarded fallback path.
	if got, _ := cordis.Get[*fakeDB](left, "db"); got.name != "left" {
		t.Fatalf("left scope leaked: %v", got)
	}
	if got, _ := cordis.Get[*fakeDB](right, "db"); got.name != "right" {
		t.Fatalf("right scope leaked: %v", got)
	}
}

func TestDisposeDuringLoadUnwindsCleanly(t *testing.T) {
	root := cordis.New()
	entered := make(chan struct{})
	allowPluginReturn := make(chan struct{})
	var fiber *cordis.Fiber
	unwound := 0

	plugin := cordis.Define[struct{}]("slow", func(ctx *cordis.Context, _ struct{}) error {
		ctx.OnDispose(func() { unwound++ })
		fiber = ctx.Fiber()
		close(entered)
		<-allowPluginReturn
		return nil
	})

	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		if _, err := cordis.Load(root, plugin, struct{}{}); err != nil {
			t.Errorf("load: %v", err)
		}
	}()

	<-entered
	// The plugin body is still running: Dispose must defer the teardown to the
	// in-flight transition instead of unloading concurrently.
	fiber.Dispose()
	close(allowPluginReturn)
	<-loaded

	if fiber.State() != cordis.StateDisposed {
		t.Fatalf("want disposed, got %s", fiber.State())
	}
	if unwound != 1 {
		t.Fatalf("want the effect unwound once, got %d", unwound)
	}
}

func TestConcurrentLifecycleIsRaceFree(t *testing.T) {
	root := cordis.New()
	plugin := cordis.Define[any]("p", func(ctx *cordis.Context, _ any) error {
		cordis.On[string](ctx, "tick", func(string) {})
		_, _ = cordis.Provide[*fakeDB](ctx, "db", &fakeDB{name: "x"})
		return nil
	})

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 40; round++ {
				fiber, err := cordis.Load[any](root, plugin, round)
				if err != nil {
					continue
				}
				cordis.Emit[string](root, "tick", "x")
				fiber.Dispose()
			}
		}()
	}
	wg.Wait()
	root.Fiber().Dispose()
	if root.Fiber().State() != cordis.StateDisposed {
		t.Fatalf("want disposed, got %s", root.Fiber().State())
	}
}

func TestMustGet(t *testing.T) {
	root := cordis.New()
	database := &fakeDB{name: "a"}
	if _, err := cordis.Provide[*fakeDB](root, "db", database); err != nil {
		t.Fatal(err)
	}
	if got := cordis.MustGet[*fakeDB](root, "db"); got != database {
		t.Fatalf("want %v, got %v", database, got)
	}

	defer func() {
		reason := recover()
		if reason == nil {
			t.Fatal("want panic for a missing service")
		}
		err, ok := reason.(*cordis.Error)
		if !ok || err.Code != cordis.ErrServiceMissing {
			t.Fatalf("want SERVICE_MISSING, got %v", reason)
		}
	}()
	cordis.MustGet[*fakeDB](root, "nope")
}

// --- regressions for the independent review (M1-M4 and minors) ---

func TestDisposerIsIdempotentAfterUnload(t *testing.T) {
	root := cordis.New()
	var kept cordis.Disposer
	count := 0
	plugin := cordis.Define[struct{}]("p", func(ctx *cordis.Context, _ struct{}) error {
		kept = ctx.OnDispose(func() { count++ })
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	fiber.Dispose()
	kept() // must not run the teardown a second time
	if count != 1 {
		t.Fatalf("disposer ran %d times, want 1", count)
	}
}

func TestEffectRegisteredWhileDisposingIsUnwound(t *testing.T) {
	root := cordis.New()
	plugin := cordis.Define[struct{}]("p", func(*cordis.Context, struct{}) error { return nil })
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	unwound := 0
	fiber.Ctx.Effect("racy", func() cordis.Disposer {
		fiber.Dispose()
		return func() { unwound++ }
	})
	if fiber.State() != cordis.StateDisposed {
		t.Fatalf("want disposed, got %s", fiber.State())
	}
	if unwound != 1 {
		t.Fatalf("effect registered while disposing was orphaned: unwound=%d", unwound)
	}
}

func TestUpdateOnFailedFiberReloads(t *testing.T) {
	root := cordis.New()
	type config struct{ Fail bool }
	runs := 0
	plugin := cordis.Define[config]("p", func(_ *cordis.Context, cfg config) error {
		runs++
		if cfg.Fail {
			return errors.New("boom")
		}
		return nil
	})
	fiber, err := cordis.Load(root, plugin, config{Fail: true})
	if err == nil {
		t.Fatal("the first load is meant to fail")
	}
	if fiber.State() != cordis.StateFailed {
		t.Fatalf("want failed, got %s", fiber.State())
	}
	if err := fiber.Update(config{Fail: false}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if fiber.State() != cordis.StateActive {
		t.Fatalf("want active, got %s", fiber.State())
	}
	if runs != 2 {
		t.Fatalf("plugin ran %d times, want 2", runs)
	}
	if fiber.Error() != nil {
		t.Fatalf("stale error after successful update: %v", fiber.Error())
	}
}

func TestErrorClearedAfterSuccessfulReload(t *testing.T) {
	root := cordis.New()
	type config struct{ Bad bool }
	plugin := cordis.Define[config]("p", func(_ *cordis.Context, cfg config) error {
		if cfg.Bad {
			return errors.New("bad config")
		}
		return nil
	})
	fiber, err := cordis.Load(root, plugin, config{Bad: true})
	if err == nil {
		t.Fatal("the first load is meant to fail")
	}
	if err := fiber.Update(config{Bad: false}); err != nil {
		t.Fatal(err)
	}
	if fiber.State() != cordis.StateActive {
		t.Fatalf("want active, got %s", fiber.State())
	}
	if fiber.Error() != nil {
		t.Fatalf("an active fiber must not report a stale failure: %v", fiber.Error())
	}
}

func TestEffectChildren(t *testing.T) {
	root := cordis.New()
	plugin := cordis.Define[struct{}]("p", func(ctx *cordis.Context, _ struct{}) error {
		ctx.Effect("outer", func() cordis.Disposer {
			ctx.OnDispose(func() {})
			ctx.OnDispose(func() {})
			return func() {}
		})
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	effects := fiber.Effects()
	if len(effects) != 1 || effects[0].Label != "outer" {
		t.Fatalf("unexpected effects: %+v", effects)
	}
	if children := effects[0].Children(); len(children) != 2 {
		t.Fatalf("want 2 nested effects, got %d", len(children))
	}
}

func TestOnceListenerEffectIsCleanedUp(t *testing.T) {
	root := cordis.New()
	plugin := cordis.Define[struct{}]("p", func(ctx *cordis.Context, _ struct{}) error {
		cordis.OnOnce[string](ctx, "tick", func(string) {})
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fiber.Effects()) != 1 {
		t.Fatalf("want 1 effect, got %d", len(fiber.Effects()))
	}
	cordis.Emit[string](root, "tick", "x")
	if effects := fiber.Effects(); len(effects) != 0 {
		t.Fatalf("fired once-listener left a stale effect: %+v", effects)
	}
}

func TestScopedBailAndWaterfall(t *testing.T) {
	root := cordis.New()
	left := root.Isolate("db")

	cordis.OnValue[string](left, "ask", func(string) any { return "left" })
	cordis.OnValue[string](root, "ask", func(string) any { return "root" })
	if value, bailed := cordis.BailScoped[string](left, "db", "ask", "x"); !bailed ||
		value != "left" {
		t.Fatalf("scoped bail leaked across scopes: %v %v", value, bailed)
	}
	cordis.OnValue[string](root, "unscoped", func(string) any { return "root" })
	if value, bailed := cordis.Bail[string](root, "unscoped", "x"); !bailed || value != "root" {
		t.Fatalf("unscoped bail: %v %v", value, bailed)
	}

	cordis.OnWaterfall[string](left, "cmd", func(s string, next func(string) any) any {
		return next(s + "-left")
	})
	if got := cordis.WaterfallScoped[string](left, "db", "cmd", "x",
		func(s string) any { return s }); got != "x-left" {
		t.Fatalf("scoped waterfall: %v", got)
	}
}

type startableService struct {
	started  bool
	stopped  bool
	startErr error
}

func (s *startableService) Start() error { s.started = true; return s.startErr }
func (s *startableService) Stop() error  { s.stopped = true; return nil }

func TestServeLifecycle(t *testing.T) {
	root := cordis.New()
	service := &startableService{}
	if _, err := cordis.Serve(root, "svc", service); err != nil {
		t.Fatal(err)
	}
	if !service.started {
		t.Fatal("Start was not called")
	}
	if _, ok := cordis.Get[*startableService](root, "svc"); !ok {
		t.Fatal("service not registered")
	}
	root.Fiber().Dispose()
	if !service.stopped {
		t.Fatal("Stop was not called on dispose")
	}
}

func TestServeRollsBackOnStartError(t *testing.T) {
	root := cordis.New()
	service := &startableService{startErr: errors.New("cannot start")}
	if _, err := cordis.Serve(root, "svc", service); err == nil {
		t.Fatal("want a Start error")
	}
	if _, ok := cordis.Get[*startableService](root, "svc"); ok {
		t.Fatal("failed Start must roll the registration back")
	}
}

func TestIsolateSharedJoinsScope(t *testing.T) {
	root := cordis.New()
	first := root.IsolateShared("db", "shared")
	second := root.IsolateShared("db", "shared")
	if _, err := cordis.Provide[*fakeDB](first, "db", &fakeDB{name: "x"}); err != nil {
		t.Fatal(err)
	}
	got, ok := cordis.Get[*fakeDB](second, "db")
	if !ok || got.name != "x" {
		t.Fatalf("joined scopes must share the service, got %v ok=%v", got, ok)
	}
}

func TestRootDisposeRemovesBuiltinServices(t *testing.T) {
	root := cordis.New()
	if _, ok := cordis.Get[cordis.Registry](root, "registry"); !ok {
		t.Fatal("registry service missing")
	}
	root.Fiber().Dispose()
	if root.Fiber().State() != cordis.StateDisposed {
		t.Fatalf("want disposed, got %s", root.Fiber().State())
	}
	if _, ok := cordis.Get[cordis.Registry](root, "registry"); ok {
		t.Fatal("built-in services must be removed with the root fiber")
	}
}

func TestDisposingEffectDisposesNestedEffects(t *testing.T) {
	root := cordis.New()
	var outer cordis.Disposer
	unwound := 0
	plugin := cordis.Define[struct{}]("p", func(ctx *cordis.Context, _ struct{}) error {
		outer = ctx.Effect("outer", func() cordis.Disposer {
			ctx.OnDispose(func() { unwound++ })
			return func() {}
		})
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	outer()
	if unwound != 1 {
		t.Fatalf("nested effect not unwound: %d", unwound)
	}
	if effects := fiber.Effects(); len(effects) != 0 {
		t.Fatalf("outer effect not removed: %+v", effects)
	}
}

func TestFailedLoadUnwindsPartialEffects(t *testing.T) {
	root := cordis.New()
	unwound := 0
	boom := errors.New("boom")
	plugin := cordis.Define[struct{}]("partial", func(ctx *cordis.Context, _ struct{}) error {
		ctx.OnDispose(func() { unwound++ })
		return boom
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if !errors.Is(err, boom) {
		t.Fatalf("a failed plugin body must be reported by Load, got %v", err)
	}
	if fiber == nil {
		t.Fatal("Load must still return the fiber when the body fails")
	}
	if fiber.State() != cordis.StateFailed {
		t.Fatalf("want failed, got %s", fiber.State())
	}
	if unwound != 1 {
		t.Fatalf("partial effects must be unwound on failure, got %d", unwound)
	}
	if !errors.Is(fiber.Error(), boom) {
		t.Fatalf("error must be preserved after rollback, got %v", fiber.Error())
	}
}

// TestLoadErrorContract pins the boundary between the two ways a load can end
// without the plugin body running to completion: a failure is an error, unmet
// dependencies are not.
func TestLoadErrorContract(t *testing.T) {
	root := cordis.New()
	boom := errors.New("boom")

	failing := cordis.Define[struct{}]("failing", func(*cordis.Context, struct{}) error {
		return boom
	})
	fiber, err := cordis.Load(root, failing, struct{}{})
	if !errors.Is(err, boom) {
		t.Fatalf("Load: want the startup error, got %v", err)
	}
	if fiber == nil || fiber.State() != cordis.StateFailed {
		t.Fatalf("Load: want the failed fiber back, got %v", fiber)
	}

	// ctx.Load and Inject go through load(), so they share the contract.
	if _, err := root.Load(failing, struct{}{}); !errors.Is(err, boom) {
		t.Fatalf("ctx.Load: want the startup error, got %v", err)
	}
	if _, err := cordis.Inject(root, nil,
		func(*cordis.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Inject: want the startup error, got %v", err)
	}

	waiting := cordis.Define[struct{}]("waiting", func(*cordis.Context, struct{}) error {
		return nil
	}).WithInject("nobody-provides-this")
	pendingFiber, err := cordis.Load(root, waiting, struct{}{})
	if err != nil {
		t.Fatalf("unmet dependencies must not be an error, got %v", err)
	}
	if pendingFiber.State() != cordis.StatePending {
		t.Fatalf("want pending, got %s", pendingFiber.State())
	}
}

func TestFailedFiberReleasesServiceName(t *testing.T) {
	root := cordis.New()
	boom := errors.New("boom")
	plugin := cordis.Define[struct{}]("partial", func(ctx *cordis.Context, _ struct{}) error {
		if _, err := cordis.Provide[*fakeDB](ctx, "db", &fakeDB{name: "broken"}); err != nil {
			return err
		}
		return boom
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if !errors.Is(err, boom) {
		t.Fatalf("want the startup error, got %v", err)
	}
	if fiber.State() != cordis.StateFailed {
		t.Fatalf("want failed, got %s", fiber.State())
	}
	// The failed plugin's service registration must not shadow the name.
	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "good"}); err != nil {
		t.Fatalf("failed fiber kept the service name occupied: %v", err)
	}
	got, ok := cordis.Get[*fakeDB](root, "db")
	if !ok || got.name != "good" {
		t.Fatalf("want good, got %v ok=%v", got, ok)
	}
}
