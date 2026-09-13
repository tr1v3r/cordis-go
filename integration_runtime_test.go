package cordis_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cordis "github.com/tr1v3r/cordis-go"
)

// This file holds the runtime-domain integration tests. Each scenario crosses
// the bug-fix seams of several PRs at once — #19 effect-tree reachability,
// #20 epoch commit, #21 binding identity, #27 phantom runtime, plus the
// events/once semantics merged beside them — in combinations none of the
// individual PR regression tests covers. Every scenario synchronizes through
// channels and WaitGroups; deadlines exist only as hang guards, never as
// synchronization.

// rtRecv receives one value from ch or fails the test, so a lost wakeup is
// reported instead of wedging the suite.
func rtRecv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// TestIntegrationConcurrentLoadDisposeReloadStress combines the runtime-domain
// fixes under concurrent churn: a dependent reloads across service rebinds
// (binding identity, #21), each reload rebuilding a nested effect tree (#19)
// with a plain listener and a once-listener, while unrelated event dispatch
// races the reloads and a sibling fiber is repeatedly loaded and disposed
// beside it (registry bookkeeping, #27). The rebind goroutine is the only
// driver of the dependent's transitions, so once it joins, each round has
// settled deterministically even though the emitters raced the reload freely.
func TestIntegrationConcurrentLoadDisposeReloadStress(t *testing.T) {
	root := cordis.New()
	registry, ok := cordis.Get[cordis.Registry](root, "registry")
	if !ok {
		t.Fatal("want registry service, got none")
	}

	type rtDB struct{ gen int }
	var (
		providerCtx *cordis.Context
		unbind      cordis.Disposer
	)
	provider := cordis.Define[struct{}]("rt-stress-provider",
		func(ctx *cordis.Context, _ struct{}) error {
			disposer, err := cordis.Provide[*rtDB](ctx, "db", &rtDB{gen: 0})
			if err != nil {
				return err
			}
			providerCtx, unbind = ctx, disposer
			return nil
		})
	if _, err := root.Load(provider, struct{}{}); err != nil {
		t.Fatal(err)
	}

	var runs, outerW, innerW, genW, boots, ticks atomic.Int32
	worker := cordis.Define[struct{}]("rt-stress-worker",
		func(ctx *cordis.Context, _ struct{}) error {
			runs.Add(1)
			ctx.Effect("rt-outer", func() cordis.Disposer {
				cordis.On(ctx, "rt-tick", func(struct{}) { ticks.Add(1) })
				cordis.OnOnce(ctx, "rt-boot", func(struct{}) { boots.Add(1) })
				ctx.OnDispose(func() { innerW.Add(1) })
				return func() { outerW.Add(1) }
			})
			ctx.OnDispose(func() { genW.Add(1) })
			return nil
		}).WithInject("db")
	workerFiber, err := root.Load(worker, struct{}{})
	if err != nil {
		t.Fatal(err)
	}

	var ephRuns, ephW atomic.Int32
	ephemeral := cordis.Define[struct{}]("rt-stress-ephemeral",
		func(ctx *cordis.Context, _ struct{}) error {
			ephRuns.Add(1)
			ctx.Effect("rt-eph", func() cordis.Disposer {
				cordis.OnOnce(ctx, "rt-eph-boot", func(struct{}) {})
				return func() { ephW.Add(1) }
			})
			return nil
		})

	// Generation 0 is the only live listener before any churn.
	root.Emit("rt-boot", struct{}{})
	root.Emit("rt-tick", struct{}{})
	if got := boots.Load(); got != 1 {
		t.Fatalf("want 1 boot delivery for generation 0, got %d", got)
	}
	if got := ticks.Load(); got != 1 {
		t.Fatalf("want 1 tick delivery for generation 0, got %d", got)
	}

	const rounds = 6
	for round := 1; round <= rounds; round++ {
		gun := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { // rebind: the only driver of worker transitions
			defer wg.Done()
			<-gun
			unbind()
			next, err := cordis.Provide[*rtDB](providerCtx, "db", &rtDB{gen: round})
			if err != nil {
				t.Error(err)
				return
			}
			unbind = next
		}()
		go func() { // emit: dispatch races the unload and the reload
			defer wg.Done()
			<-gun
			for i := 0; i < 4; i++ {
				root.Emit("rt-tick", struct{}{})
				root.Emit("rt-boot", struct{}{})
			}
		}()
		go func() { // churn: load and dispose a sibling fiber
			defer wg.Done()
			<-gun
			fiber, err := root.Load(ephemeral, struct{}{})
			if err != nil {
				t.Error(err)
				return
			}
			fiber.Dispose()
		}()
		close(gun)
		wg.Wait()

		// The rebind has joined and drove every worker transition itself, so
		// generation `round` is now the one live listener set.
		root.Emit("rt-boot", struct{}{})
		root.Emit("rt-tick", struct{}{})
	}

	if got := runs.Load(); got != rounds+1 {
		t.Fatalf("want %d worker generations, got %d", rounds+1, got)
	}
	if got := boots.Load(); got != rounds+1 {
		t.Fatalf("want %d once deliveries (one per generation), got %d", rounds+1, got)
	}
	if got := ticks.Load(); got < rounds+1 {
		t.Fatalf("want at least %d tick deliveries, got %d", rounds+1, got)
	}
	if got := outerW.Load(); got != rounds {
		t.Fatalf("want %d outer disposers run, got %d", rounds, got)
	}
	if got := innerW.Load(); got != rounds {
		t.Fatalf("want %d nested disposers run, got %d", rounds, got)
	}
	if got := genW.Load(); got != rounds {
		t.Fatalf("want %d generation disposers run, got %d", rounds, got)
	}
	if got := workerFiber.State(); got != cordis.StateActive {
		t.Fatalf("want worker state active after the stress, got %s", got)
	}
	if got := registry.Size(); got != 2 {
		t.Fatalf("want registry size 2 (provider and worker), got %d", got)
	}
	if got := ephRuns.Load(); got != rounds {
		t.Fatalf("want %d ephemeral loads, got %d", rounds, got)
	}
	if got := ephW.Load(); got != rounds {
		t.Fatalf("want %d ephemeral disposers run, got %d", rounds, got)
	}

	// Full teardown unwinds the live generation too, and nothing survives it.
	root.Fiber().Dispose()
	if got := workerFiber.State(); got != cordis.StateDisposed {
		t.Fatalf("want worker state disposed after teardown, got %s", got)
	}
	if got := outerW.Load(); got != rounds+1 {
		t.Fatalf("want %d outer disposers after teardown, got %d", rounds+1, got)
	}
	if got := innerW.Load(); got != rounds+1 {
		t.Fatalf("want %d nested disposers after teardown, got %d", rounds+1, got)
	}
	if got := genW.Load(); got != rounds+1 {
		t.Fatalf("want %d generation disposers after teardown, got %d", rounds+1, got)
	}
	if got := registry.Size(); got != 0 {
		t.Fatalf("want registry size 0 after teardown, got %d", got)
	}
	bootsBefore, ticksBefore := boots.Load(), ticks.Load()
	root.Emit("rt-boot", struct{}{})
	root.Emit("rt-tick", struct{}{})
	if got := boots.Load(); got != bootsBefore {
		t.Fatalf("want boot count %d to hold after teardown, got %d", bootsBefore, got)
	}
	if got := ticks.Load(); got != ticksBefore {
		t.Fatalf("want tick count %d to hold after teardown, got %d", ticksBefore, got)
	}
}

// TestIntegrationEffectAdoptionRacingExternalDispose pins the #19 adoption
// race in its harshest ordering: a nested effect has been adopted by an outer
// effect whose body is still running, and the fiber is disposed before either
// body returns. The deferred teardown must unwind every entry exactly once —
// owner before nested, listener before its marker — whichever body unblocks
// first, and a registration attempted after the unload must be rejected
// loudly instead of orphaning its disposer.
func TestIntegrationEffectAdoptionRacingExternalDispose(t *testing.T) {
	root := cordis.New()

	var outerW, innerW, listenerHits atomic.Int32
	outerBodyEntered := make(chan struct{})
	releaseOuterBody := make(chan struct{})
	outerReturned := make(chan struct{})
	innerEntered := make(chan struct{})
	releaseInnerBody := make(chan struct{})
	listenerGone := make(chan struct{})

	plugin := cordis.Define[struct{}]("rt-adopt", func(ctx *cordis.Context, _ struct{}) error {
		go func() {
			defer close(outerReturned)
			ctx.Effect("rt-outer", func() cordis.Disposer {
				close(outerBodyEntered)
				<-releaseOuterBody
				return func() { outerW.Add(1) }
			})
		}()
		<-outerBodyEntered
		go func() {
			ctx.Effect("rt-inner", func() cordis.Disposer {
				// Registered before the listener, so it unwinds after it and
				// its run proves the listener left the bus.
				ctx.OnDispose(func() { close(listenerGone) })
				cordis.On(ctx, "rt-adopt-ev", func(struct{}) { listenerHits.Add(1) })
				close(innerEntered)
				<-releaseInnerBody
				return func() { innerW.Add(1) }
			})
		}()
		<-innerEntered
		return nil
	})
	fiber, err := root.Load(plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}

	// rt-inner is adopted by rt-outer and the listener by rt-inner; the fiber
	// is disposed while both bodies are parked inside their adoptions.
	fiber.Dispose()
	if got := fiber.State(); got != cordis.StateDisposed {
		t.Fatalf("want fiber state disposed, got %s", got)
	}
	if got := len(fiber.Effects()); got != 0 {
		t.Fatalf("want 0 live effects after dispose, got %d", got)
	}

	// Unblock both bodies: the inner one returns first, but its teardown is
	// still owned by the outer effect, so the unwind must cascade from the
	// outer body's return down to the listener — whichever goroutine ends up
	// running each deferred teardown, every disposer runs exactly once.
	close(releaseInnerBody)
	close(releaseOuterBody)
	rtRecv(t, listenerGone, "nested listener teardown")
	rtRecv(t, outerReturned, "outer effect return")
	if got := innerW.Load(); got != 1 {
		t.Fatalf("want 1 inner disposer run, got %d", got)
	}
	if got := outerW.Load(); got != 1 {
		t.Fatalf("want 1 outer disposer run, got %d", got)
	}

	root.Emit("rt-adopt-ev", struct{}{})
	if got := listenerHits.Load(); got != 0 {
		t.Fatalf("want 0 deliveries from the disposed listener, got %d", got)
	}

	// A registration attempted after the fiber died is rejected loudly rather
	// than parked where no unload could ever reach it.
	reason := func() (reason any) {
		defer func() { reason = recover() }()
		fiber.Ctx.Effect("rt-late", func() cordis.Disposer { return func() {} })
		return nil
	}()
	failure, ok := reason.(error)
	if !ok {
		t.Fatalf("want an error panic for a late effect, got %v", reason)
	}
	var framework *cordis.Error
	if !errors.As(failure, &framework) || framework.Code != cordis.ErrInactiveEffect {
		t.Fatalf("want error code %s, got %v", cordis.ErrInactiveEffect, failure)
	}
}

// TestIntegrationEpochRebindUnderConcurrentEmit pins #21 and #20 together: a
// provider that unbinds and re-provides the same service name keeps its fiber
// UID, so only the binding identity can move the dependent's epoch, and every
// committed epoch must run its body exactly once — no skipped generation, no
// repeated one — while concurrent emitters keep dispatching through the bus.
func TestIntegrationEpochRebindUnderConcurrentEmit(t *testing.T) {
	root := cordis.New()
	registry, ok := cordis.Get[cordis.Registry](root, "registry")
	if !ok {
		t.Fatal("want registry service, got none")
	}

	type rtDB struct{ gen int }
	var (
		providerCtx *cordis.Context
		unbind      cordis.Disposer
	)
	provider := cordis.Define[struct{}]("rt-epoch-provider",
		func(ctx *cordis.Context, _ struct{}) error {
			disposer, err := cordis.Provide[*rtDB](ctx, "db", &rtDB{gen: 0})
			if err != nil {
				return err
			}
			providerCtx, unbind = ctx, disposer
			return nil
		})
	if _, err := root.Load(provider, struct{}{}); err != nil {
		t.Fatal(err)
	}

	var runs, boots atomic.Int32
	var mu sync.Mutex
	var resolved []int
	worker := cordis.Define[struct{}]("rt-epoch-worker",
		func(ctx *cordis.Context, _ struct{}) error {
			n := int(runs.Add(1))
			db, ok := cordis.Get[*rtDB](ctx, "db")
			if !ok {
				return fmt.Errorf("generation %d resolved no service", n)
			}
			mu.Lock()
			resolved = append(resolved, db.gen)
			mu.Unlock()
			cordis.OnValue[struct{}](ctx, "rt-query", func(struct{}) any {
				cur, _ := cordis.Get[*rtDB](ctx, "db")
				return cur.gen
			})
			cordis.OnOnce(ctx, "rt-boot", func(struct{}) { boots.Add(1) })
			return nil
		}).WithInject("db")
	workerFiber, err := root.Load(worker, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	root.Emit("rt-boot", struct{}{})

	const rounds, emitters, emitsPerEmitter = 4, 3, 5
	for round := 1; round <= rounds; round++ {
		gun := make(chan struct{})
		var wg sync.WaitGroup
		for e := 0; e < emitters; e++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gun
				for i := 0; i < emitsPerEmitter; i++ {
					root.Emit("rt-query", struct{}{})
				}
			}()
		}
		// The rebind below races the emitters, but it stays the only driver
		// of the worker's transitions, so exactly one load settles from it.
		close(gun)
		unbind()
		next, err := cordis.Provide[*rtDB](providerCtx, "db", &rtDB{gen: round})
		if err != nil {
			t.Fatal(err)
		}
		unbind = next
		wg.Wait()

		answer, bailed := root.Bail[struct{}]("rt-query", struct{}{})
		if !bailed {
			t.Fatalf("want the live generation to answer rt-query, got no bail")
		}
		if got := answer.(int); got != round {
			t.Fatalf("want query answer %d after rebind %d, got %d", round, round, got)
		}
		root.Emit("rt-boot", struct{}{})
	}

	if got := runs.Load(); got != rounds+1 {
		t.Fatalf("want %d worker generations, got %d", rounds+1, got)
	}
	mu.Lock()
	gotResolved := append([]int(nil), resolved...)
	mu.Unlock()
	if len(gotResolved) != rounds+1 {
		t.Fatalf("want %d resolved generations, got %d (%v)",
			rounds+1, len(gotResolved), gotResolved)
	}
	for i, gen := range gotResolved {
		if gen != i {
			t.Fatalf("want resolved generation %d at load %d, got %d (resolved = %v)",
				i, i, gen, gotResolved)
		}
	}
	if got := boots.Load(); got != rounds+1 {
		t.Fatalf("want %d once deliveries (one per generation), got %d", rounds+1, got)
	}
	if got := workerFiber.State(); got != cordis.StateActive {
		t.Fatalf("want worker state active after the rebinds, got %s", got)
	}
	if got := registry.Size(); got != 2 {
		t.Fatalf("want registry size 2 (provider and worker), got %d", got)
	}
	root.Fiber().Dispose()
	if got := registry.Size(); got != 0 {
		t.Fatalf("want registry size 0 after teardown, got %d", got)
	}
}

// TestIntegrationOnceUnwoundByDisposeAndReload pins both unwinding paths of a
// once-listener: an external Dispose of the owning fiber, and a
// dependency-driven reload across a service rebind. A generation that never
// fired must take its listener with it, a generation that fired must retire
// its effect entry, and each generation may deliver at most once.
func TestIntegrationOnceUnwoundByDisposeAndReload(t *testing.T) {
	root := cordis.New()

	// Phase A: external Dispose unwinds a nested, not-yet-fired once-listener.
	var soloFired, ownerW atomic.Int32
	disposeOnly := cordis.Define[struct{}]("rt-solo-a",
		func(ctx *cordis.Context, _ struct{}) error {
			ctx.Effect("rt-solo-owner", func() cordis.Disposer {
				cordis.OnOnce(ctx, "rt-solo", func(struct{}) { soloFired.Add(1) })
				return func() { ownerW.Add(1) }
			})
			return nil
		})
	fiberA, err := root.Load(disposeOnly, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	effects := fiberA.Effects()
	if len(effects) != 1 {
		t.Fatalf("want 1 fiber-level effect, got %d", len(effects))
	}
	if children := effects[0].Children(); len(children) != 1 {
		t.Fatalf("want the once-listener nested under its owner, got %d children", len(children))
	}
	fiberA.Dispose()
	if got := ownerW.Load(); got != 1 {
		t.Fatalf("want 1 owner disposer run, got %d", got)
	}
	root.Emit("rt-solo", struct{}{})
	if got := soloFired.Load(); got != 0 {
		t.Fatalf("want 0 deliveries from the disposed once-listener, got %d", got)
	}

	// Phase B: reloading across a rebind retires the old generation's listener
	// and registers a fresh one; every generation delivers at most once.
	type rtDB struct{ gen int }
	var (
		providerCtx *cordis.Context
		unbind      cordis.Disposer
	)
	provider := cordis.Define[struct{}]("rt-solo-provider",
		func(ctx *cordis.Context, _ struct{}) error {
			disposer, err := cordis.Provide[*rtDB](ctx, "solo-db", &rtDB{gen: 1})
			if err != nil {
				return err
			}
			providerCtx, unbind = ctx, disposer
			return nil
		})
	if _, err := root.Load(provider, struct{}{}); err != nil {
		t.Fatal(err)
	}

	var runs atomic.Int32
	var mu sync.Mutex
	var deliveries []int
	readDeliveries := func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), deliveries...)
	}
	solo := cordis.Define[struct{}]("rt-solo-b",
		func(ctx *cordis.Context, _ struct{}) error {
			runs.Add(1)
			db, ok := cordis.Get[*rtDB](ctx, "solo-db")
			if !ok {
				return errors.New("solo generation resolved no service")
			}
			cordis.OnOnce(ctx, "rt-solo2", func(struct{}) {
				mu.Lock()
				deliveries = append(deliveries, db.gen)
				mu.Unlock()
			})
			return nil
		}).WithInject("solo-db")
	soloFiber, err := root.Load(solo, struct{}{})
	if err != nil {
		t.Fatal(err)
	}

	root.Emit("rt-solo2", struct{}{})
	if got := readDeliveries(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("want deliveries [1] from generation 1, got %v", got)
	}
	if got := len(soloFiber.Effects()); got != 0 {
		t.Fatalf("want 0 live effects after the once fired, got %d", got)
	}

	rebind := func(gen int) {
		t.Helper()
		unbind()
		next, err := cordis.Provide[*rtDB](providerCtx, "solo-db", &rtDB{gen: gen})
		if err != nil {
			t.Fatal(err)
		}
		unbind = next
	}
	rebind(2)
	if got := runs.Load(); got != 2 {
		t.Fatalf("want 2 worker generations after the first rebind, got %d", got)
	}
	if got := len(soloFiber.Effects()); got != 1 {
		t.Fatalf("want 1 live effect after the reload, got %d", got)
	}
	root.Emit("rt-solo2", struct{}{})
	root.Emit("rt-solo2", struct{}{}) // the fired once must not deliver again
	if got := readDeliveries(); len(got) != 2 || got[1] != 2 {
		t.Fatalf("want deliveries [1 2] after generation 2 fired once, got %v", got)
	}

	rebind(3)
	root.Emit("rt-solo2", struct{}{})
	if got := readDeliveries(); len(got) != 3 || got[2] != 3 {
		t.Fatalf("want deliveries [1 2 3] after generation 3 fired, got %v", got)
	}
	if got := soloFiber.State(); got != cordis.StateActive {
		t.Fatalf("want solo state active, got %s", got)
	}
	root.Fiber().Dispose()
	root.Emit("rt-solo2", struct{}{})
	if got := readDeliveries(); len(got) != 3 {
		t.Fatalf("want deliveries to hold at 3 after teardown, got %v", got)
	}
}

// TestIntegrationDoubleLoadRacingExternalDispose pins #27 under the
// double-load semantics: two fibers of one definition share one runtime, and
// an external Dispose racing the loads may neither unregister the runtime
// while the surviving fiber still holds it, nor leave it behind once both
// fibers are gone.
func TestIntegrationDoubleLoadRacingExternalDispose(t *testing.T) {
	root := cordis.New()
	registry, ok := cordis.Get[cordis.Registry](root, "registry")
	if !ok {
		t.Fatal("want registry service, got none")
	}

	var runs, unwound atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	twin := cordis.Define[struct{}]("rt-twin", func(ctx *cordis.Context, _ struct{}) error {
		n := int(runs.Add(1))
		ctx.OnDispose(func() { unwound.Add(1) })
		if n <= 2 {
			entered <- struct{}{}
			<-release
		}
		return nil
	})

	// Load blocks until the bodies unblock, so the fibers are captured from
	// the creation event the runtime publishes before running them.
	twins := make(chan *cordis.Fiber, 2)
	cordis.On[*cordis.PluginEvent](root, "internal/plugin", func(e *cordis.PluginEvent) {
		if e.Fiber.Name() == "rt-twin" {
			select {
			case twins <- e.Fiber:
			default:
			}
		}
	})

	loadDone := make(chan error, 2)
	gun := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-gun
			_, err := root.Load(twin, struct{}{})
			loadDone <- err
		}()
	}
	close(gun)
	rtRecv(t, entered, "first twin body")
	rtRecv(t, entered, "second twin body")
	fiber1 := rtRecv(t, twins, "first twin fiber")
	fiber2 := rtRecv(t, twins, "second twin fiber")

	// One definition, two live fibers: exactly one shared runtime.
	if got := registry.Size(); got != 1 {
		t.Fatalf("want registry size 1 for the shared runtime, got %d", got)
	}
	if names := registry.Plugins(); len(names) != 1 || names[0] != "rt-twin" {
		t.Fatalf("want plugins [rt-twin], got %v", names)
	}

	// External dispose while both bodies are parked: fiber1's teardown is
	// deferred to its loader goroutine, and the runtime must survive through
	// fiber2 rather than vanish beneath it.
	fiber1.Dispose()
	if got := registry.Size(); got != 1 {
		t.Fatalf("want registry size 1 while the second fiber lives, got %d", got)
	}

	close(release)
	if err := rtRecv(t, loadDone, "first load to finish"); err != nil {
		t.Fatal(err)
	}
	if err := rtRecv(t, loadDone, "second load to finish"); err != nil {
		t.Fatal(err)
	}
	if got := fiber1.State(); got != cordis.StateDisposed {
		t.Fatalf("want the disposed twin state disposed, got %s", got)
	}
	if got := fiber2.State(); got != cordis.StateActive {
		t.Fatalf("want the surviving twin state active, got %s", got)
	}
	if got := len(fiber1.Effects()); got != 0 {
		t.Fatalf("want 0 live effects on the disposed twin, got %d", got)
	}
	if got := unwound.Load(); got != 1 {
		t.Fatalf("want 1 disposer run after the racing dispose, got %d", got)
	}

	fiber2.Dispose()
	if got := registry.Size(); got != 0 {
		t.Fatalf("want registry size 0 after both twins are gone, got %d", got)
	}
	if got := unwound.Load(); got != 2 {
		t.Fatalf("want 2 disposers run after both twins are gone, got %d", got)
	}

	// The definition is loadable again once its runtime left the registry.
	third, err := root.Load(twin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Size(); got != 1 {
		t.Fatalf("want registry size 1 for the reloaded twin, got %d", got)
	}
	third.Dispose()
	if got := registry.Size(); got != 0 {
		t.Fatalf("want registry size 0 after the final dispose, got %d", got)
	}
	if got := unwound.Load(); got != 3 {
		t.Fatalf("want 3 disposers run in total, got %d", got)
	}
	root.Fiber().Dispose()
}
