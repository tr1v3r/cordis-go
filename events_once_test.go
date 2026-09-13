package cordis_test

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

// holdDispatchers registers a listener that blocks every dispatcher reaching it
// until want of them arrived. Registering it before the listener under test
// guarantees that concurrent dispatchers have all taken their snapshots before
// any of them can reach that listener, so the "two dispatchers hold the same
// once-listener" race is exercised deterministically instead of by chance.
func holdDispatchers(t *testing.T, ctx *cordis.Context, name string, want int) {
	t.Helper()
	var gate sync.WaitGroup
	gate.Add(want)
	cordis.On[string](ctx, name, func(string) {
		gate.Done()
		gate.Wait()
	})
}

// dispatchConcurrently runs dispatch in want goroutines released together.
func dispatchConcurrently(want int, dispatch func()) {
	var start, done sync.WaitGroup
	start.Add(want)
	done.Add(want)
	for range want {
		go func() {
			defer done.Done()
			start.Done()
			start.Wait()
			dispatch()
		}()
	}
	done.Wait()
}

func TestOnceListenerRunsOnceUnderConcurrentEmit(t *testing.T) {
	root := cordis.New()
	var fired atomic.Int32
	holdDispatchers(t, root, "tick", 2)
	cordis.OnOnce[string](root, "tick", func(string) { fired.Add(1) })

	dispatchConcurrently(2, func() { cordis.Emit[string](root, "tick", "x") })

	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 invocation of a once listener, got %d", got)
	}
}

func TestOnceListenerRunsOnceUnderConcurrentBail(t *testing.T) {
	root := cordis.New()
	var fired atomic.Int32
	holdDispatchers(t, root, "tick", 2)
	cordis.OnOnce[string](root, "tick", func(string) { fired.Add(1) })

	dispatchConcurrently(2, func() { cordis.Bail[string](root, "tick", "x") })

	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 invocation of a once listener, got %d", got)
	}
}

func TestOnceListenerRunsOnceUnderConcurrentParallel(t *testing.T) {
	root := cordis.New()
	// Parallel spawns one goroutine per listener, so the dispatchers cannot be
	// gated once they are inside it; racing them repeatedly is what exposes a
	// listener that both of them were allowed to run.
	const rounds = 100
	for round := range rounds {
		var fired atomic.Int32
		cordis.OnOnce[string](root, "tick", func(string) { fired.Add(1) })
		dispatchConcurrently(2, func() {
			if err := cordis.Parallel[string](root, "tick", "x"); err != nil {
				t.Errorf("parallel dispatch: %v", err)
			}
		})
		if got := fired.Load(); got != 1 {
			t.Fatalf("round %d: want 1 invocation of a once listener, got %d", round, got)
		}
	}
}

func TestOnceWaterfallListenerRunsOnceUnderConcurrentWaterfall(t *testing.T) {
	root := cordis.New()
	var fired atomic.Int32
	// Both dispatchers must take their bus snapshots before either of them
	// reaches the once listener: the gate releases exactly when both arrived,
	// and next hands the chain on to the once listener after the gate.
	var gate sync.WaitGroup
	gate.Add(2)
	cordis.OnWaterfall[string](root, "cmd", func(s string, next func(string) any) any {
		gate.Done()
		gate.Wait()
		return next(s)
	})
	cordis.OnWaterfall[string](root, "cmd", func(s string, next func(string) any) any {
		fired.Add(1)
		return next(s)
	}, cordis.WithOnce())

	dispatchConcurrently(2, func() {
		cordis.Waterfall[string](root, "cmd", "x", func(s string) any { return s })
	})

	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 invocation of a once waterfall listener, got %d", got)
	}
}

func TestOnceListenerIsReleasedFromEffectsAfterEmit(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())
	cordis.OnOnce[string](root, "tick", func(string) {})
	if got := len(root.Effects()); got != base+1 {
		t.Fatalf("want %d effects while registered, got %d", base+1, got)
	}

	cordis.Emit[string](root, "tick", "x")

	if got := len(root.Effects()); got != base {
		t.Fatalf("want %d effects after the once listener fired, got %d", base, got)
	}
}

func TestOnceListenerThatPanicsIsRetiredAndNotRunAgain(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())
	var fired atomic.Int32
	cordis.OnOnce[string](root, "tick", func(string) { fired.Add(1); panic("boom") })

	// The dispatch turns the panic into a logged error, and the listener is
	// retired even though its body never completed.
	cordis.Emit[string](root, "tick", "x")
	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 invocation of the panicking once listener, got %d", got)
	}
	if got := len(root.Effects()); got != base {
		t.Fatalf("want %d effects after the panicking once listener ran, got %d", base, got)
	}

	cordis.Emit[string](root, "tick", "y")
	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 invocation after the retry dispatch, got %d", got)
	}
}

func TestOnceListenerDisposerIsIdempotentAfterFire(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())
	var fired atomic.Int32
	dispose := cordis.OnOnce[string](root, "tick", func(string) { fired.Add(1) })

	cordis.Emit[string](root, "tick", "x")
	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 invocation of a once listener, got %d", got)
	}

	// Firing already ran the cleanup through releaseOnce, so the caller's
	// disposer must stay a no-op instead of resurrecting the listener.
	dispose()
	dispose()
	if got := len(root.Effects()); got != base {
		t.Fatalf("want %d effects after firing and disposing, got %d", base, got)
	}
	cordis.Emit[string](root, "tick", "y")
	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 invocation after disposal, got %d", got)
	}
}

func TestOnceWaterfallListenerIsReleasedFromEffects(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())
	cordis.OnWaterfall[string](root, "cmd", func(s string, next func(string) any) any {
		return next(s)
	}, cordis.WithOnce())

	cordis.Waterfall[string](root, "cmd", "x", func(s string) any { return s })

	if got := len(root.Effects()); got != base {
		t.Fatalf("want %d effects after the once listener fired, got %d", base, got)
	}
}

func TestOnceWaterfallListenerThatPanicsIsStillReleased(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())
	finals := 0
	cordis.OnWaterfall[string](root, "cmd", func(string, func(string) any) any {
		panic("boom")
	}, cordis.WithOnce())

	// The panic must not keep the effect entry: the listener is retired like on
	// every other dispatch path, and the chain continues to final.
	got := cordis.Waterfall[string](root, "cmd", "x", func(s string) any {
		finals++
		return "final:" + s
	})

	if finals != 1 {
		t.Fatalf("want 1 call to final, got %d", finals)
	}
	if got != "final:x" {
		t.Fatalf("want final:x, got %v", got)
	}
	if effects := len(root.Effects()); effects != base {
		t.Fatalf("want %d effects after the panicking once listener ran, got %d", base, effects)
	}
}

func TestConcurrentOnceRegistrationLeavesNoStaleEffects(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())

	// A dispatcher that keeps snapshotting the bus while listeners register is
	// what makes it possible to dispatch a once-listener after it entered the
	// bus but before its registration could store the cleanup.
	var stop atomic.Bool
	var emitter sync.WaitGroup
	emitter.Add(1)
	go func() {
		defer emitter.Done()
		for !stop.Load() {
			cordis.Emit[string](root, "tick", "x")
		}
	}()

	const rounds = 1000
	for range rounds {
		cordis.OnOnce[string](root, "tick", func(string) {})
	}
	stop.Store(true)
	emitter.Wait()

	// A registration that no dispatcher saw yet still fires here; a listener
	// that already fired must not be reported any more.
	for range 4 {
		cordis.Emit[string](root, "tick", "drain")
	}
	if got := len(root.Effects()); got != base {
		t.Fatalf("want %d effects after %d concurrent registrations, got %d", base, rounds, got)
	}
}

func TestOnceListenerKeepsPrependSemantics(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())
	var order []string
	cordis.On[string](root, "tick", func(string) { order = append(order, "on") })
	cordis.OnOnce[string](root, "tick", func(string) { order = append(order, "once") },
		cordis.Prepend())

	cordis.Emit[string](root, "tick", "x")
	cordis.Emit[string](root, "tick", "x")

	want := []string{"once", "on", "on"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("want %v, got %v", want, order)
	}
	if got := len(root.Effects()); got != base+1 {
		t.Fatalf("want %d effects after the prepended once listener fired, got %d", base+1, got)
	}
}

func TestOnceListenerKeepsGlobalSemantics(t *testing.T) {
	root := cordis.New()
	scoped := root.Isolate("db")
	base := len(scoped.Effects())
	var fired atomic.Int32
	cordis.OnOnce[string](scoped, "tick", func(string) { fired.Add(1) }, cordis.Global())

	cordis.EmitScoped[string](root, "db", "tick", "x")

	if got := fired.Load(); got != 1 {
		t.Fatalf("want 1 scoped dispatch to a global once listener, got %d", got)
	}
	if got := len(scoped.Effects()); got != base {
		t.Fatalf("want %d effects after the global once listener fired, got %d", base, got)
	}
}

func TestPlainListenerIsNotReleasedByDispatch(t *testing.T) {
	root := cordis.New()
	base := len(root.Effects())
	fired := 0
	dispose := cordis.On[string](root, "tick", func(string) { fired++ })

	cordis.Emit[string](root, "tick", "a")
	cordis.Emit[string](root, "tick", "b")
	if fired != 2 {
		t.Fatalf("want 2 dispatches, got %d", fired)
	}
	if got := len(root.Effects()); got != base+1 {
		t.Fatalf("want %d effects while registered, got %d", base+1, got)
	}

	dispose()
	cordis.Emit[string](root, "tick", "c")
	if fired != 2 {
		t.Fatalf("want 2 dispatches after disposal, got %d", fired)
	}
	if got := len(root.Effects()); got != base {
		t.Fatalf("want %d effects after disposal, got %d", base, got)
	}
}
