package cordis_test

import (
	"context"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

type benchmarkService struct {
	value int
}

// benchmarkServable exercises the Serve lifecycle hooks: Start runs on
// registration, Stop when the owning fiber unwinds.
type benchmarkServable struct {
	starts int
	stops  int
}

func (s *benchmarkServable) Start() error { s.starts++; return nil }
func (s *benchmarkServable) Stop() error  { s.stops++; return nil }

// Sinks keep the measured values observable, so the compiler cannot delete the
// work being benchmarked.
var (
	benchmarkContextSink *cordis.Context
	benchmarkServiceSink *benchmarkService
	benchmarkResultSink  any
	benchmarkBoolSink    bool
	benchmarkErrorSink   error
	benchmarkMetaSink    []*cordis.EffectMeta
)

// --- Context lifecycle -----------------------------------------------------

func BenchmarkContextNewDispose(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		root := cordis.New()
		root.Fiber().Dispose()
	}
}

func BenchmarkContextWithBaseContext(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		base, cancel := context.WithCancel(context.Background())
		root := cordis.New(cordis.WithBaseContext(base))
		root.Fiber().Dispose()
		cancel()
	}
}

func BenchmarkContextFork(b *testing.B) {
	root := cordis.New()
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	var child *cordis.Context
	for b.Loop() {
		child = root.Fork("benchmark")
	}
	benchmarkContextSink = child
}

func BenchmarkContextIsolate(b *testing.B) {
	root := cordis.New()
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	var child *cordis.Context
	for b.Loop() {
		child = root.Isolate("benchmark")
	}
	benchmarkContextSink = child
	if child.Parent() != root || child.Name() != root.Name() {
		b.Fatalf("want an isolated child of the root, got %q", child.Name())
	}
}

// --- Service resolution ----------------------------------------------------

func BenchmarkServiceGet(b *testing.B) {
	b.Run("Hit", func(b *testing.B) {
		root := cordis.New()
		service := &benchmarkService{value: 42}
		if _, err := root.Provide("benchmark", service); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		var got *benchmarkService
		for b.Loop() {
			got, _ = root.Get[*benchmarkService]("benchmark")
		}
		benchmarkServiceSink = got
		if got != service {
			b.Fatalf("want the provided service, got %#v", got)
		}
	})

	b.Run("Miss", func(b *testing.B) {
		root := cordis.New()
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		var got *benchmarkService
		for b.Loop() {
			got, _ = root.Get[*benchmarkService]("missing")
		}
		benchmarkServiceSink = got
		if got != nil {
			b.Fatalf("want no service, got %#v", got)
		}
	})

	b.Run("Isolated", func(b *testing.B) {
		root := cordis.New()
		isolated := root.IsolateShared("benchmark", "tenant")
		service := &benchmarkService{value: 42}
		if _, err := isolated.Provide("benchmark", service); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		var got *benchmarkService
		for b.Loop() {
			got, _ = isolated.Get[*benchmarkService]("benchmark")
		}
		benchmarkServiceSink = got
		if got != service {
			b.Fatalf("want the isolated service, got %#v", got)
		}
	})

	// NestedFiber measures the snapshot walk: the deepest fiber of a five-deep
	// chain resolves a service provided at the root, so every level is visited.
	b.Run("NestedFiber", func(b *testing.B) {
		root := cordis.New()
		service := &benchmarkService{value: 42}
		if _, err := root.Provide("benchmark", service); err != nil {
			b.Fatal(err)
		}

		var deepest *cordis.Context
		var nest func(depth int) *cordis.Plugin[struct{}]
		nest = func(depth int) *cordis.Plugin[struct{}] {
			return cordis.Define[struct{}]("nested", func(ctx *cordis.Context, _ struct{}) error {
				if depth == 0 {
					deepest = ctx
					return nil
				}
				_, err := ctx.Load(nest(depth-1), struct{}{})
				return err
			})
		}
		if _, err := root.Load(nest(4), struct{}{}); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		var got *benchmarkService
		for b.Loop() {
			got, _ = deepest.Get[*benchmarkService]("benchmark")
		}
		benchmarkServiceSink = got
		if got != service {
			b.Fatalf("want the root service through the chain, got %#v", got)
		}
	})

	b.Run("CheckedAvailable", func(b *testing.B) {
		root := cordis.New()
		service := &benchmarkService{value: 42}
		available := true
		_, err := root.ProvideChecked("benchmark", service, func() bool { return available })
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		var got *benchmarkService
		for b.Loop() {
			got, _ = root.Get[*benchmarkService]("benchmark")
		}
		benchmarkServiceSink = got
		if got != service {
			b.Fatalf("want the available service, got %#v", got)
		}
	})

	b.Run("CheckedUnavailable", func(b *testing.B) {
		root := cordis.New()
		service := &benchmarkService{value: 42}
		available := false
		_, err := root.ProvideChecked("benchmark", service, func() bool { return available })
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		var got *benchmarkService
		var ok bool
		for b.Loop() {
			got, ok = root.Get[*benchmarkService]("benchmark")
		}
		benchmarkServiceSink, benchmarkBoolSink = got, ok
		if ok || got != nil {
			b.Fatalf("want an unavailable service, got %#v (ok=%v)", got, ok)
		}
	})
}

func BenchmarkServiceLookup(b *testing.B) {
	b.Run("Hit", func(b *testing.B) {
		root := cordis.New()
		service := &benchmarkService{value: 42}
		if _, err := root.Provide("benchmark", service); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		var got any
		for b.Loop() {
			got, _ = root.Lookup("benchmark")
		}
		benchmarkResultSink = got
		if got != any(service) {
			b.Fatalf("want the provided service, got %#v", got)
		}
	})

	b.Run("Miss", func(b *testing.B) {
		root := cordis.New()
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		var got any
		for b.Loop() {
			got, _ = root.Lookup("missing")
		}
		benchmarkResultSink = got
		if got != nil {
			b.Fatalf("want no service, got %#v", got)
		}
	})
}

func BenchmarkServiceMustGet(b *testing.B) {
	root := cordis.New()
	service := &benchmarkService{value: 42}
	if _, err := root.Provide("benchmark", service); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	var got *benchmarkService
	for b.Loop() {
		got = root.MustGet[*benchmarkService]("benchmark")
	}
	benchmarkServiceSink = got
	if got != service {
		b.Fatalf("want the provided service, got %#v", got)
	}
}

func BenchmarkServiceSet(b *testing.B) {
	root := cordis.New()
	first := &benchmarkService{value: 1}
	second := &benchmarkService{value: 2}
	if _, err := root.Provide("benchmark", first); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := root.Set("benchmark", second); err != nil {
			b.Fatal(err)
		}
	}

	got, _ := root.Get[*benchmarkService]("benchmark")
	benchmarkServiceSink = got
	if got != second {
		b.Fatalf("want the swapped value, got %#v", got)
	}
}

// BenchmarkServiceProvideDispose measures registration and teardown, and with it
// the notify scan: a service change walks every fiber in the application, so the
// unrelated-fiber variants show whether that scan is the cost driver.
func BenchmarkServiceProvideDispose(b *testing.B) {
	for _, unrelated := range []int{0, 10, 100, 1000} {
		name := "NoOtherFibers"
		if unrelated > 0 {
			name = strconv.Itoa(unrelated) + "UnrelatedFibers"
		}
		b.Run(name, func(b *testing.B) {
			root := cordis.New()
			noop := cordis.Define[struct{}]("noop", func(*cordis.Context, struct{}) error {
				return nil
			})
			for range unrelated {
				if _, err := root.Load(noop, struct{}{}); err != nil {
					b.Fatal(err)
				}
			}
			service := &benchmarkService{value: 42}
			b.Cleanup(root.Fiber().Dispose)
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				dispose, err := root.Provide("benchmark", service)
				if err != nil {
					b.Fatal(err)
				}
				dispose()
			}
		})
	}
}

func BenchmarkServiceServe(b *testing.B) {
	root := cordis.New()
	servable := &benchmarkServable{}
	plugin := cordis.Define[struct{}]("servable", func(ctx *cordis.Context, _ struct{}) error {
		_, err := ctx.Serve("benchmark", servable)
		return err
	})
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		fiber, err := root.Load(plugin, struct{}{})
		if err != nil {
			b.Fatal(err)
		}
		fiber.Dispose()
	}

	if servable.starts != b.N || servable.stops != b.N {
		b.Fatalf("want %d starts and %d stops, got %d and %d",
			b.N, b.N, servable.starts, servable.stops)
	}
}

// BenchmarkDependencyActivateDeactivate measures one provider replacement with a
// growing number of dependents, which is where notify fan-out costs show up.
func BenchmarkDependencyActivateDeactivate(b *testing.B) {
	for _, dependents := range []int{1, 10, 100} {
		b.Run(strconv.Itoa(dependents)+"Dependents", func(b *testing.B) {
			root := cordis.New()
			activations := 0
			plugin := cordis.Define[struct{}]("consumer", func(*cordis.Context, struct{}) error {
				activations++
				return nil
			}).WithInject("benchmark")
			for range dependents {
				fiber, err := root.Load(plugin, struct{}{})
				if err != nil {
					b.Fatal(err)
				}
				if fiber.State() != cordis.StatePending {
					b.Fatalf("want pending before the provider exists, got %s", fiber.State())
				}
			}
			service := &benchmarkService{value: 42}
			b.Cleanup(root.Fiber().Dispose)
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				dispose, err := root.Provide("benchmark", service)
				if err != nil {
					b.Fatal(err)
				}
				dispose()
			}

			if want := b.N * dependents; activations != want {
				b.Fatalf("want %d activations, got %d", want, activations)
			}
		})
	}
}

// --- Events ----------------------------------------------------------------

func BenchmarkEventEmit(b *testing.B) {
	for _, listeners := range []int{0, 1, 10, 100} {
		b.Run(strconv.Itoa(listeners)+"Listeners", func(b *testing.B) {
			root := cordis.New()
			calls := 0
			for range listeners {
				root.On("benchmark", func(struct{}) { calls++ })
			}
			b.Cleanup(root.Fiber().Dispose)
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				root.Emit("benchmark", struct{}{})
			}

			if want := b.N * listeners; calls != want {
				b.Fatalf("want %d listener calls, got %d", want, calls)
			}
		})
	}
}

func BenchmarkEventEmitScoped(b *testing.B) {
	b.Run("SameScope", func(b *testing.B) {
		root := cordis.New()
		scopes := make([]*cordis.Context, 8)
		for index := range scopes {
			scopes[index] = root.IsolateShared("tenant", "s"+strconv.Itoa(index))
		}
		calls := 0
		for _, scope := range scopes {
			for range 5 {
				scope.On("benchmark", func(struct{}) { calls++ })
			}
		}
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			scopes[0].EmitScoped("tenant", "benchmark", struct{}{})
		}

		if want := b.N * 5; calls != want {
			b.Fatalf("want %d calls from the emitting scope only, got %d", want, calls)
		}
	})

	b.Run("WithGlobal", func(b *testing.B) {
		root := cordis.New()
		scopes := make([]*cordis.Context, 8)
		for index := range scopes {
			scopes[index] = root.IsolateShared("tenant", "s"+strconv.Itoa(index))
		}
		calls := 0
		for _, scope := range scopes {
			for range 5 {
				scope.On("benchmark", func(struct{}) { calls++ })
			}
			scope.On("benchmark", func(struct{}) { calls++ }, cordis.Global())
		}
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			scopes[0].EmitScoped("tenant", "benchmark", struct{}{})
		}

		// Five listeners in the emitting scope plus every global one.
		if want := b.N * (5 + len(scopes)); calls != want {
			b.Fatalf("want %d calls including globals, got %d", want, calls)
		}
	})
}

func BenchmarkEventEmitManyNames(b *testing.B) {
	root := cordis.New()
	const names = 100
	calls := 0
	for index := range names {
		root.On("benchmark/"+strconv.Itoa(index), func(struct{}) { calls++ })
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		root.Emit("benchmark/50", struct{}{})
	}

	if want := b.N; calls != want {
		b.Fatalf("want %d calls for the emitted name, got %d", want, calls)
	}
}

func BenchmarkEventEmitPrepend(b *testing.B) {
	root := cordis.New()
	calls := 0
	for range 10 {
		root.On("benchmark", func(struct{}) { calls++ })
	}
	root.On("benchmark", func(struct{}) { calls++ }, cordis.Prepend())
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		root.Emit("benchmark", struct{}{})
	}

	if want := b.N * 11; calls != want {
		b.Fatalf("want %d listener calls, got %d", want, calls)
	}
}

// BenchmarkEventOnOnce measures a once-listener's whole life: registration, the
// single dispatch that claims it, and the release of its effect entry.
func BenchmarkEventOnOnce(b *testing.B) {
	root := cordis.New()
	calls := 0
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		root.OnOnce("benchmark", func(struct{}) { calls++ })
		root.Emit("benchmark", struct{}{})
	}

	if calls != b.N {
		b.Fatalf("want %d once-listener calls, got %d", b.N, calls)
	}
}

func BenchmarkEventBail(b *testing.B) {
	for _, bailAt := range []int{0, 9} {
		name := "FirstListener"
		if bailAt != 0 {
			name = "LastListener"
		}
		b.Run(name, func(b *testing.B) {
			root := cordis.New()
			for index := range 10 {
				root.OnValue("benchmark", func(struct{}) any {
					if index == bailAt {
						return index + 1
					}
					return nil
				})
			}
			b.Cleanup(root.Fiber().Dispose)
			b.ReportAllocs()
			b.ResetTimer()

			var result any
			var bailed bool
			for b.Loop() {
				result, bailed = root.Bail("benchmark", struct{}{})
			}
			benchmarkResultSink = result
			if !bailed || result != bailAt+1 {
				b.Fatalf("want listener %d to bail, got %#v", bailAt, result)
			}
		})
	}
}

func BenchmarkEventParallel(b *testing.B) {
	for _, listeners := range []int{1, 10, 100} {
		b.Run(strconv.Itoa(listeners)+"Listeners", func(b *testing.B) {
			root := cordis.New()
			for range listeners {
				root.On("benchmark", func(struct{}) {})
			}
			b.Cleanup(root.Fiber().Dispose)
			b.ReportAllocs()
			b.ResetTimer()

			var err error
			for b.Loop() {
				err = root.Parallel("benchmark", struct{}{})
			}
			benchmarkErrorSink = err
			if err != nil {
				b.Fatal(err)
			}
		})
	}
}

func BenchmarkEventWaterfall(b *testing.B) {
	for _, listeners := range []int{1, 10, 100} {
		b.Run(strconv.Itoa(listeners)+"Listeners", func(b *testing.B) {
			root := cordis.New()
			for range listeners {
				root.OnWaterfall("benchmark", func(value int, next func(int) any) any {
					return next(value + 1)
				})
			}
			b.Cleanup(root.Fiber().Dispose)
			b.ReportAllocs()
			b.ResetTimer()

			var result any
			for b.Loop() {
				result = root.Waterfall("benchmark", 0, func(value int) any { return value })
			}
			benchmarkResultSink = result
			if result != listeners {
				b.Fatalf("want waterfall result %d, got %#v", listeners, result)
			}
		})
	}
}

func BenchmarkEventRegisterDispose(b *testing.B) {
	b.Run("Plain", func(b *testing.B) {
		root := cordis.New()
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			dispose := root.On("benchmark", func(struct{}) {})
			dispose()
		}
	})

	b.Run("Once", func(b *testing.B) {
		root := cordis.New()
		b.Cleanup(root.Fiber().Dispose)
		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			dispose := root.OnOnce("benchmark", func(struct{}) {})
			dispose()
		}
	})
}

// BenchmarkEventPanicIsolation measures the containment path: the listener
// panics, invoke recovers it, and the dispatch carries on.
func BenchmarkEventPanicIsolation(b *testing.B) {
	root := cordis.New()
	calls := 0
	root.On("benchmark", func(struct{}) { panic("boom") })
	root.On("benchmark", func(struct{}) { calls++ })
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		root.Emit("benchmark", struct{}{})
	}

	if calls != b.N {
		b.Fatalf("want %d calls after the panic, got %d", b.N, calls)
	}
}

// --- Plugin and effect lifecycle -------------------------------------------

func BenchmarkPluginLoadDispose(b *testing.B) {
	for _, effects := range []int{0, 3} {
		b.Run(strconv.Itoa(effects)+"Effects", func(b *testing.B) {
			root := cordis.New()
			disposals := 0
			plugin := cordis.Define[struct{}]("benchmark",
				func(ctx *cordis.Context, _ struct{}) error {
					for range effects {
						ctx.OnDispose(func() { disposals++ })
					}
					return nil
				})
			b.Cleanup(root.Fiber().Dispose)
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				fiber, err := root.Load(plugin, struct{}{})
				if err != nil {
					b.Fatal(err)
				}
				fiber.Dispose()
			}

			if want := b.N * effects; disposals != want {
				b.Fatalf("want %d disposals, got %d", want, disposals)
			}
		})
	}
}

func BenchmarkPluginNestedLoad(b *testing.B) {
	for _, children := range []int{1, 10} {
		b.Run(strconv.Itoa(children)+"Children", func(b *testing.B) {
			root := cordis.New()
			loads := 0
			child := cordis.Define[struct{}]("child", func(*cordis.Context, struct{}) error {
				loads++
				return nil
			})
			parent := cordis.Define[struct{}]("parent",
				func(ctx *cordis.Context, _ struct{}) error {
					for range children {
						if _, err := ctx.Load(child, struct{}{}); err != nil {
							return err
						}
					}
					return nil
				})
			b.Cleanup(root.Fiber().Dispose)
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				fiber, err := root.Load(parent, struct{}{})
				if err != nil {
					b.Fatal(err)
				}
				fiber.Dispose()
			}

			if want := b.N * children; loads != want {
				b.Fatalf("want %d child loads, got %d", want, loads)
			}
		})
	}
}

func BenchmarkPluginInject(b *testing.B) {
	root := cordis.New()
	if _, err := root.Provide("benchmark", &benchmarkService{value: 42}); err != nil {
		b.Fatal(err)
	}
	activations := 0
	body := func(*cordis.Context) error {
		activations++
		return nil
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		fiber, err := cordis.Inject(root, []string{"benchmark"}, body)
		if err != nil {
			b.Fatal(err)
		}
		fiber.Dispose()
	}

	if activations != b.N {
		b.Fatalf("want %d activations, got %d", b.N, activations)
	}
}

func BenchmarkPluginRestart(b *testing.B) {
	root := cordis.New()
	loads := 0
	disposals := 0
	plugin := cordis.Define[struct{}]("benchmark", func(ctx *cordis.Context, _ struct{}) error {
		loads++
		ctx.OnDispose(func() { disposals++ })
		return nil
	})
	fiber, err := root.Load(plugin, struct{}{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := fiber.Restart(); err != nil {
			b.Fatal(err)
		}
	}

	if loads != b.N+1 || disposals != b.N {
		b.Fatalf("want %d loads and %d disposals, got %d and %d",
			b.N+1, b.N, loads, disposals)
	}
}

func BenchmarkPluginUpdate(b *testing.B) {
	root := cordis.New()
	loads := 0
	lastConfig := 0
	plugin := cordis.Define[int]("benchmark", func(_ *cordis.Context, config int) error {
		loads++
		lastConfig = config
		return nil
	})
	fiber, err := root.Load(plugin, 0)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	updates := 0
	for b.Loop() {
		updates++
		if err := fiber.Update(updates); err != nil {
			b.Fatal(err)
		}
	}

	if loads != b.N+1 || lastConfig != b.N {
		b.Fatalf("want %d loads ending at config %d, got %d and %d",
			b.N+1, b.N, loads, lastConfig)
	}
}

// BenchmarkEffectNested measures an effect body that registers an effect of its
// own, which is the nesting path: the inner entry is adopted by the outer one
// because the body registers it through the scope context it was handed.
func BenchmarkEffectNested(b *testing.B) {
	root := cordis.New()
	disposals := 0
	plugin := cordis.Define[struct{}]("benchmark", func(ctx *cordis.Context, _ struct{}) error {
		ctx.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
			scope.Effect("inner", func(*cordis.Context) cordis.Disposer {
				return func() { disposals++ }
			})
			return func() { disposals++ }
		})
		return nil
	})
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		fiber, err := root.Load(plugin, struct{}{})
		if err != nil {
			b.Fatal(err)
		}
		fiber.Dispose()
	}

	if want := b.N * 2; disposals != want {
		b.Fatalf("want %d disposals, got %d", want, disposals)
	}
}

func BenchmarkEffectsSnapshot(b *testing.B) {
	root := cordis.New()
	plugin := cordis.Define[struct{}]("benchmark", func(ctx *cordis.Context, _ struct{}) error {
		for range 100 {
			ctx.OnDispose(func() {})
		}
		ctx.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
			for range 10 {
				scope.Effect("inner", func(*cordis.Context) cordis.Disposer { return func() {} })
			}
			return func() {}
		})
		return nil
	})
	fiber, err := root.Load(plugin, struct{}{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	var effects []*cordis.EffectMeta
	for b.Loop() {
		effects = fiber.Effects()
	}
	benchmarkMetaSink = effects
	if len(effects) != 101 {
		b.Fatalf("want 101 top-level effects, got %d", len(effects))
	}
}

func BenchmarkDisposerIdempotent(b *testing.B) {
	root := cordis.New()
	calls := 0
	dispose := root.OnDispose(func() { calls++ })
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		dispose()
		dispose()
	}

	if calls != 1 {
		b.Fatalf("want the disposer to run once, got %d calls", calls)
	}
}

// --- Logger ----------------------------------------------------------------

func BenchmarkLogger(b *testing.B) {
	root := cordis.New()
	logger := root.Logger("benchmark")
	b.Cleanup(root.Fiber().Dispose)

	b.Run("Filtered", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			logger.Debug("value=%d", 1)
		}
	})

	b.Run("Discard", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			logger.Info("benchmark line")
		}
	})

	b.Run("Formatted", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			logger.Info("value=%d name=%s", 42, "benchmark")
		}
	})
}

// --- Concurrency -----------------------------------------------------------
//
// The serial cases above measure the cost of one call. These measure what
// happens when many goroutines take the same locks: the read paths (service
// resolution, event dispatch) share core.mu, fiber.mu and the bus lock, and the
// write paths (plugin load, service registration) additionally share the runtime
// registry and the parent fiber's effect list.
//
// A benchmark body must not call b.Fatal from a RunParallel goroutine, so the
// failures are recorded and reported once the parallel section has joined.

// parallelFailure records the first error a benchmark goroutine hit.
type parallelFailure struct {
	mu  sync.Mutex
	err error
}

func (f *parallelFailure) add(err error) {
	if err == nil {
		return
	}
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
}

func (f *parallelFailure) fatal(b *testing.B) {
	b.Helper()
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	if err != nil {
		b.Fatal(err)
	}
}

// BenchmarkServiceGetParallel measures the read path under contention: every
// goroutine walks the same fiber chain and takes the same locks.
func BenchmarkServiceGetParallel(b *testing.B) {
	root := cordis.New()
	service := &benchmarkService{value: 42}
	if _, err := root.Provide("benchmark", service); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	var hits atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if got, ok := root.Get[*benchmarkService]("benchmark"); ok && got == service {
				hits.Add(1)
			}
		}
	})

	if got := hits.Load(); got != int64(b.N) {
		b.Fatalf("want %d successful lookups, got %d", b.N, got)
	}
}

// BenchmarkEventEmitParallel measures dispatch under contention: every goroutine
// copies the same listener list and invokes the same closures.
func BenchmarkEventEmitParallel(b *testing.B) {
	root := cordis.New()
	const listeners = 10
	var calls atomic.Int64
	for range listeners {
		root.On("benchmark", func(struct{}) { calls.Add(1) })
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			root.Emit("benchmark", struct{}{})
		}
	})

	if want := int64(b.N) * listeners; calls.Load() != want {
		b.Fatalf("want %d listener calls, got %d", want, calls.Load())
	}
}

// BenchmarkPluginLoadDisposeParallel measures concurrent loads of one plugin
// definition: every load claims the same runtime, and registers a child lifetime
// on the shared parent, so the runtime registry and the parent's effect list are
// both contended.
func BenchmarkPluginLoadDisposeParallel(b *testing.B) {
	root := cordis.New()
	var loads, disposals atomic.Int64
	plugin := cordis.Define[struct{}]("benchmark", func(ctx *cordis.Context, _ struct{}) error {
		loads.Add(1)
		ctx.OnDispose(func() { disposals.Add(1) })
		return nil
	})
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	var failures parallelFailure
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			fiber, err := root.Load(plugin, struct{}{})
			if err != nil {
				failures.add(err)
				continue
			}
			fiber.Dispose()
		}
	})
	failures.fatal(b)

	if got := loads.Load(); got != int64(b.N) {
		b.Fatalf("want %d loads, got %d", b.N, got)
	}
	if got := disposals.Load(); got != int64(b.N) {
		b.Fatalf("want %d disposals, got %d", b.N, got)
	}
}

// BenchmarkServiceProvideDisposeParallel measures the write path: each goroutine
// registers a service in its own isolation scope, so they share core.mu and the
// root fiber's effect list instead of colliding on one service name.
//
// This is the shape that found the concurrent-registration defect, and it is
// measurable only because effect ownership is explicit: one goroutine's
// registration used to be adopted by another goroutine's running effect, which
// ran the disposal in its own goroutine. Dispose then returned with the binding
// still registered - about one registration in 1400 at eight or more goroutines -
// and the next Provide on that scope failed with SERVICE_EXISTS. With ownership
// stated by the registering context, a fiber-level registration cannot be adopted
// at all, and the benchmark asserts the cleanup it now guarantees.
func BenchmarkServiceProvideDisposeParallel(b *testing.B) {
	root := cordis.New()
	service := &benchmarkService{value: 42}

	// RunParallel calls the body once per goroutine, and it calls it again on
	// every calibration pass, so each pass must hand the scopes back. A scope is
	// claimed on entry and returned when that goroutine's pass ends; there is one
	// per goroutine, and the extra room is slack for a raised b.SetParallelism.
	scopes := make(chan *cordis.Context, 4*runtime.GOMAXPROCS(0))
	for index := range cap(scopes) {
		scopes <- root.IsolateShared("benchmark", "s"+strconv.Itoa(index))
	}
	b.Cleanup(root.Fiber().Dispose)
	b.ReportAllocs()
	b.ResetTimer()

	var (
		provides atomic.Int64
		failures parallelFailure
	)
	b.RunParallel(func(pb *testing.PB) {
		scope := <-scopes
		defer func() { scopes <- scope }()
		for pb.Next() {
			dispose, err := scope.Provide("benchmark", service)
			if err != nil {
				failures.add(err)
				continue
			}
			provides.Add(1)
			dispose()
		}
	})
	failures.fatal(b)

	if got := provides.Load(); got != int64(b.N) {
		b.Fatalf("want %d registrations, got %d", b.N, got)
	}
}
