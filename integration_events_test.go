package cordis_test

import (
	"sync"
	"sync/atomic"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

// wantServiceMissingErr fails the test unless err is the typed SERVICE_MISSING
// error, which is the contract a runtime host matches on.
func wantServiceMissingErr(t *testing.T, err error) {
	t.Helper()
	frameworkErr, ok := err.(*cordis.Error)
	if !ok || frameworkErr.Code != cordis.ErrServiceMissing {
		t.Fatalf("want SERVICE_MISSING, got %v", err)
	}
}

// TestIntegrationOnceClaimedAcrossParallelWaterfallAndRegistration composes the
// dispatch paths around one once-listener: two Parallel dispatchers each drive
// one waterfall listener, every inner chain contends for the same
// once-listener, and registrations race the dispatch. The claim must hold on
// every path, every chain must settle once, and no once-listener may leave a
// stale effect entry behind.
func TestIntegrationOnceClaimedAcrossParallelWaterfallAndRegistration(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())

	// Registrations race a spinning waterfall dispatch: a listener that entered
	// the bus before its registration could install the cleanup is dispatched
	// exactly here, which is the window the release path must tolerate.
	var raced atomic.Int32
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			cordis.Waterfall[string](root, "inner", "spin", func(s string) any { return s })
		}
	}()
	const racing = 200
	for range racing {
		cordis.OnWaterfall[string](root, "inner", func(s string, next func(string) any) any {
			raced.Add(1)
			return next(s)
		}, cordis.WithOnce())
	}
	close(stop)
	<-stopped

	// The gate holds every storm chain until both arrived. Sitting in front of
	// the once-listener, it guarantees that both chains hold the listener in
	// their snapshots before either of them can claim it; later dispatches pass
	// through once hold is closed.
	arrivals := make(chan struct{}, 4)
	hold := make(chan struct{})
	cordis.OnWaterfall[string](root, "inner", func(s string, next func(string) any) any {
		arrivals <- struct{}{}
		<-hold
		return next(s)
	}, cordis.Prepend())

	var claimed atomic.Int32
	cordis.OnWaterfall[string](root, "inner", func(s string, next func(string) any) any {
		claimed.Add(1)
		return next(s + "-claimed")
	}, cordis.WithOnce())

	// The outer event is dispatched by two Parallel dispatchers. Its waterfall
	// listener runs one inner chain each; Parallel hands it a next it must not
	// call, so it returns the result of the inner chain instead.
	var finals atomic.Int32
	results := make(chan string, 2)
	cordis.OnWaterfall[string](root, "outer", func(s string, _ func(string) any) any {
		got := cordis.Waterfall[string](root, "inner", s, func(v string) any {
			finals.Add(1)
			return "final:" + v
		})
		results <- got.(string)
		return got
	})
	var outerOnce atomic.Int32
	cordis.OnOnce[string](root, "outer", func(string) { outerOnce.Add(1) })

	var dispatchers sync.WaitGroup
	dispatchers.Add(2)
	for range 2 {
		go func() {
			defer dispatchers.Done()
			if err := cordis.Parallel[string](root, "outer", "x"); err != nil {
				t.Errorf("parallel dispatch: %v", err)
			}
		}()
	}
	<-arrivals
	<-arrivals
	close(hold)
	dispatchers.Wait()

	sawClaimed, sawPlain := false, false
	for range 2 {
		switch got := <-results; got {
		case "final:x-claimed":
			sawClaimed = true
		case "final:x":
			sawPlain = true
		default:
			t.Fatalf("want final:x or final:x-claimed, got %v", got)
		}
	}
	if !sawClaimed || !sawPlain {
		t.Fatalf("want one chain per variant, got claimed=%v plain=%v", sawClaimed, sawPlain)
	}

	// One drain chain fires every once-listener that no storm chain claimed.
	cordis.Waterfall[string](root, "inner", "drain", func(s string) any {
		finals.Add(1)
		return s
	})

	if got := claimed.Load(); got != 1 {
		t.Fatalf("want 1 invocation of the inner once listener, got %d", got)
	}
	if got := outerOnce.Load(); got != 1 {
		t.Fatalf("want 1 invocation of the outer once listener, got %d", got)
	}
	if got := raced.Load(); got != racing {
		t.Fatalf("want %d racing once listeners fired exactly once, got %d", racing, got)
	}
	if got := finals.Load(); got != 3 {
		t.Fatalf("want 3 chain settlements (2 storm + 1 drain), got %d", got)
	}
	// The inner gate and the outer middleware are plain listeners, so exactly
	// their two effect entries remain.
	if got := len(root.Effects()); got != base+2 {
		t.Fatalf("want %d effects after every once listener retired, got %d", base+2, got)
	}
}

// TestIntegrationOnceWaterfallPanickingAfterNextRetiresAndSettles crosses the
// once retirement with the settle latch: a once-listener that panics after
// calling next must leave the bus and Effects(), and the chain must settle
// exactly once with the result the next call already produced.
func TestIntegrationOnceWaterfallPanickingAfterNextRetiresAndSettles(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())

	var ran, finals atomic.Int32
	cordis.OnWaterfall[string](root, "cmd", func(s string, next func(string) any) any {
		next(s + "-a")
		ran.Add(1)
		panic("boom")
	}, cordis.WithOnce())
	if got := len(root.Effects()); got != base+1 {
		t.Fatalf("want %d effects while registered, got %d", base+1, got)
	}

	got := cordis.Waterfall[string](root, "cmd", "x", func(s string) any {
		finals.Add(1)
		return "final:" + s
	})
	if got != "final:x-a" {
		t.Fatalf("want final:x-a from the single settlement, got %v", got)
	}
	if n := finals.Load(); n != 1 {
		t.Fatalf("want 1 call to final, got %d", n)
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("want 1 invocation of the panicking once listener, got %d", n)
	}
	if n := len(root.Effects()); n != base {
		t.Fatalf("want %d effects after the once listener retired, got %d", base, n)
	}

	// Retirement must persist: a later chain reaches final only, without a
	// second mutation from the retired listener.
	after := cordis.Waterfall[string](root, "cmd", "y", func(s string) any {
		finals.Add(1)
		return "final:" + s
	})
	if after != "final:y" {
		t.Fatalf("want final:y from the retired chain, got %v", after)
	}
	if n := finals.Load(); n != 2 {
		t.Fatalf("want 2 calls to final after both chains, got %d", n)
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("want the once listener to stay retired, got %d invocations", n)
	}
}

// TestIntegrationOnceRetirementAcrossEmitAndPluginUnload pins both retirement
// paths of a once-listener owned by a plugin fiber: firing it through Emit
// releases the effect entry, unloading the owning fiber retires an unfired
// listener, and a dispatch that is still inside the gate when the fiber unloads
// still runs the once body exactly once.
func TestIntegrationOnceRetirementAcrossEmitAndPluginUnload(t *testing.T) {
	root := cordis.New()

	// Path 1: fire through Emit, then unload the owner.
	var fired atomic.Int32
	var watcherCtx *cordis.Context
	watcher := cordis.Define[struct{}]("watcher", func(ctx *cordis.Context, _ struct{}) error {
		watcherCtx = ctx
		cordis.OnOnce[string](ctx, "tick", func(string) { fired.Add(1) })
		return nil
	})
	watcherFiber, err := root.Load(watcher, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, watcherFiber, cordis.StateActive)
	if got := len(watcherCtx.Effects()); got != 1 {
		t.Fatalf("want 1 effect while the once listener is registered, got %d", got)
	}

	cordis.Emit[string](root, "tick", "a")
	cordis.Emit[string](root, "tick", "b")
	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 invocation after two emits, got %d", got)
	}
	if got := len(watcherCtx.Effects()); got != 0 {
		t.Fatalf("want 0 effects after the once listener fired, got %d", got)
	}

	watcherFiber.Dispose()
	waitFiberState(t, watcherFiber, cordis.StateDisposed)
	cordis.Emit[string](root, "tick", "c")
	if got := fired.Load(); got != 1 {
		t.Fatalf("want the once listener retired across the unload, got %d invocations", got)
	}

	// Path 2: two dispatchers hold the listener in their snapshots while the
	// owning fiber unloads; the claim must still admit exactly one of them.
	var blocked atomic.Int32
	arrived := make(chan struct{}, 2)
	hold := make(chan struct{})
	blocker := cordis.Define[struct{}]("blocker", func(ctx *cordis.Context, _ struct{}) error {
		cordis.On[string](ctx, "tock", func(string) {
			arrived <- struct{}{}
			<-hold
		})
		cordis.OnOnce[string](ctx, "tock", func(string) { blocked.Add(1) })
		return nil
	})
	blockerFiber, err := root.Load(blocker, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, blockerFiber, cordis.StateActive)

	var emitters sync.WaitGroup
	emitters.Add(2)
	for range 2 {
		go func() {
			defer emitters.Done()
			cordis.Emit[string](root, "tock", "x")
		}()
	}
	<-arrived
	<-arrived
	// Unloading removes the listeners from the bus, but both dispatchers hold
	// their snapshots, so the once claim still arbitrates between them.
	blockerFiber.Dispose()
	waitFiberState(t, blockerFiber, cordis.StateDisposed)
	close(hold)
	emitters.Wait()

	if got := blocked.Load(); got != 1 {
		t.Fatalf("want 1 invocation of a once listener claimed across an unload, got %d", got)
	}
	cordis.Emit[string](root, "tock", "y")
	if got := blocked.Load(); got != 1 {
		t.Fatalf("want the once listener retired after the unload, got %d invocations", got)
	}
}

// TestIntegrationReprovideCycleKeepsAliasBlindAndReloadsDependent crosses the
// by-name lookup with the binding epoch: through a provide, unbind and
// reprovide cycle the label carries one service name, so a context that maps
// another name onto that label never reads or replaces the service, while the
// dependent pinned to the released binding reloads against the new one.
func TestIntegrationReprovideCycleKeepsAliasBlindAndReloadsDependent(t *testing.T) {
	root := cordis.New()
	type integCfg struct{ gen int }

	shared := root.IsolateShared("cfg", "shared")
	alias := shared.IsolateShared("other", "shared")

	var (
		providerCtx *cordis.Context
		unbind      cordis.Disposer
	)
	provider := cordis.Define[struct{}]("provider", func(ctx *cordis.Context, _ struct{}) error {
		disposer, err := cordis.Provide[*integCfg](ctx, "cfg", &integCfg{gen: 1})
		if err != nil {
			return err
		}
		providerCtx, unbind = ctx, disposer
		return nil
	})
	if _, err := shared.Load(provider, struct{}{}); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var (
		runs        atomic.Int32
		consumerCtx *cordis.Context
	)
	consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		consumerCtx = ctx
		if runs.Add(1) == 2 {
			entered <- struct{}{}
			<-release
		}
		return nil
	}).WithInject("cfg")
	consumerFiber, err := shared.Load(consumer, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, consumerFiber, cordis.StateActive)
	if got := runs.Load(); got != 1 {
		t.Fatalf("want 1 load of the consumer, got %d", got)
	}
	if cfg, ok := cordis.Get[*integCfg](consumerCtx, "cfg"); !ok || cfg.gen != 1 {
		t.Fatalf("want the first binding (gen 1), got %+v (ok=%v)", cfg, ok)
	}

	// The label of "cfg" carries one name: a context that maps "other" onto it
	// must neither read nor replace the service registered there.
	if got, ok := cordis.Get[*integCfg](alias, "other"); ok {
		t.Fatalf("want no service %q, got %+v", "other", got)
	}
	if _, ok := alias.Lookup("other"); ok {
		t.Fatalf("want lookup of %q to fail, got a service", "other")
	}
	wantServiceMissingErr(t, alias.Set("other", &integCfg{gen: 9}))
	if cfg, ok := cordis.Get[*integCfg](alias, "cfg"); !ok || cfg.gen != 1 {
		t.Fatalf("want the shared service under its own name, got %+v (ok=%v)", cfg, ok)
	}

	// The provider replaces its binding while the dependent's second load is
	// busy: the new binding identity must reload the dependent a third time.
	restarted := make(chan error, 1)
	go func() { restarted <- consumerFiber.Restart() }()
	<-entered
	unbind()
	if _, err := cordis.Provide[*integCfg](providerCtx, "cfg", &integCfg{gen: 2}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-restarted; err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, consumerFiber, cordis.StateActive)
	if got := runs.Load(); got != 3 {
		t.Fatalf("want a third load against the rebound binding, got %d", got)
	}
	if cfg, ok := cordis.Get[*integCfg](consumerCtx, "cfg"); !ok || cfg.gen != 2 {
		t.Fatalf("want the rebound service (gen 2), got %+v (ok=%v)", cfg, ok)
	}

	// The cycle did not widen the label: "other" stays blind and "cfg" serves
	// the rebound value through every context of the scope.
	if got, ok := cordis.Get[*integCfg](alias, "other"); ok {
		t.Fatalf("want no service %q after the cycle, got %+v", "other", got)
	}
	wantServiceMissingErr(t, alias.Set("other", &integCfg{gen: 9}))
	if cfg, ok := cordis.Get[*integCfg](shared, "cfg"); !ok || cfg.gen != 2 {
		t.Fatalf("want the rebound service in the shared scope, got %+v (ok=%v)", cfg, ok)
	}
}

// TestIntegrationWithInjectSurvivesRebindCycle pins the inject lifecycle across
// a rebind: the consumer stays pending while the name is unbound, activates
// against the first binding, returns to pending when the provider releases it,
// and activates again against the rebound service. The scope label is shared
// with another name that must never see the service.
func TestIntegrationWithInjectSurvivesRebindCycle(t *testing.T) {
	root := cordis.New()
	type integDB struct{ gen int }

	alias := root.IsolateShared("other", "db")

	var (
		runs        atomic.Int32
		consumerCtx *cordis.Context
	)
	consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		consumerCtx = ctx
		runs.Add(1)
		return nil
	}).WithInject("db")
	consumerFiber, err := root.Load(consumer, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, consumerFiber, cordis.StatePending)
	if got := runs.Load(); got != 0 {
		t.Fatalf("want 0 loads while the injected service is missing, got %d", got)
	}

	var (
		providerCtx *cordis.Context
		unbind      cordis.Disposer
	)
	provider := cordis.Define[struct{}]("provider", func(ctx *cordis.Context, _ struct{}) error {
		disposer, err := cordis.Provide[*integDB](ctx, "db", &integDB{gen: 1})
		if err != nil {
			return err
		}
		providerCtx, unbind = ctx, disposer
		return nil
	})
	if _, err := root.Load(provider, struct{}{}); err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, consumerFiber, cordis.StateActive)
	if got := runs.Load(); got != 1 {
		t.Fatalf("want 1 load once the service appeared, got %d", got)
	}
	if db, ok := cordis.Get[*integDB](consumerCtx, "db"); !ok || db.gen != 1 {
		t.Fatalf("want the first service (gen 1), got %+v (ok=%v)", db, ok)
	}

	// "other" shares the label of "db" but must never resolve to it.
	if got, ok := cordis.Get[*integDB](alias, "other"); ok {
		t.Fatalf("want no service %q, got %+v", "other", got)
	}
	wantServiceMissingErr(t, alias.Set("other", &integDB{gen: 9}))

	// Releasing the registration parks the dependent; rebinding the name from
	// the same provider fiber activates it against the new binding.
	unbind()
	waitFiberState(t, consumerFiber, cordis.StatePending)
	if got := runs.Load(); got != 1 {
		t.Fatalf("want the load count frozen while pending, got %d", got)
	}

	if _, err := cordis.Provide[*integDB](providerCtx, "db", &integDB{gen: 2}); err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, consumerFiber, cordis.StateActive)
	if got := runs.Load(); got != 2 {
		t.Fatalf("want 2 loads across the rebind, got %d", got)
	}
	if db, ok := cordis.Get[*integDB](consumerCtx, "db"); !ok || db.gen != 2 {
		t.Fatalf("want the rebound service (gen 2), got %+v (ok=%v)", db, ok)
	}

	if got, ok := cordis.Get[*integDB](alias, "other"); ok {
		t.Fatalf("want no service %q after the rebind, got %+v", "other", got)
	}
	if db, ok := cordis.Get[*integDB](alias, "db"); !ok || db.gen != 2 {
		t.Fatalf("want the rebound service under its own name, got %+v (ok=%v)", db, ok)
	}
}
