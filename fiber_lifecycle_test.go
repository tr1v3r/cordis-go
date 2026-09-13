package cordis_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cordis "github.com/tr1v3r/cordis-go"
)

func waitFiberState(t *testing.T, f *cordis.Fiber, want cordis.FiberState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.State() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("want fiber state %s, got %s", want, f.State())
}

func providePlugin(name, service string) *cordis.Plugin[struct{}] {
	return cordis.Define[struct{}](name, func(ctx *cordis.Context, _ struct{}) error {
		_, err := cordis.Provide[*struct{}](ctx, service, &struct{}{})
		return err
	})
}

// TestDisposeDuringLoadDoesNotLoseWakeup pins the busy handoff in Dispose: the
// refresh loop must still see the disposal request after a load body finishes.
func TestDisposeDuringLoadDoesNotLoseWakeup(t *testing.T) {
	root := cordis.New()

	var runs atomic.Int32
	entered := make(chan struct{})
	releaseBody := make(chan struct{})

	plugin := cordis.Define[struct{}]("slow", func(ctx *cordis.Context, _ struct{}) error {
		n := runs.Add(1)
		ctx.OnDispose(func() {})
		if n == 2 {
			close(entered)
			<-releaseBody
		}
		return nil
	}).WithInject("gate")

	fiber, err := root.Load(plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	gate1, err := root.Load(providePlugin("gate1", "gate"), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, fiber, cordis.StateActive)

	gate1.Dispose()
	waitFiberState(t, fiber, cordis.StatePending)

	gate2Done := make(chan error, 1)
	go func() {
		_, err := root.Load(providePlugin("gate2", "gate"), struct{}{})
		gate2Done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second load did not start")
	}

	// Block Dispose after it has read busy=true but before it can mark dirty.
	// This widens the handoff window deterministically.
	disposeStarted := make(chan struct{})
	releaseDispose := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDispose) }) }
	defer release()
	var blockOnce sync.Once
	cordis.On[*cordis.PluginEvent](root, "internal/plugin", func(e *cordis.PluginEvent) {
		if e.Fiber != fiber {
			return
		}
		blockOnce.Do(func() {
			close(disposeStarted)
			<-releaseDispose
		})
	})

	disposeDone := make(chan struct{})
	go func() {
		fiber.Dispose()
		close(disposeDone)
	}()

	select {
	case <-disposeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("dispose did not emit internal/plugin")
	}

	close(releaseBody)
	waitFiberState(t, fiber, cordis.StateDisposed)
	if got := len(fiber.Effects()); got != 0 {
		t.Fatalf("effects after dispose = %d, want 0", got)
	}

	release()
	select {
	case <-disposeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Dispose did not return")
	}
	if err := <-gate2Done; err != nil {
		t.Fatal(err)
	}
}

// TestDisposeDuringFailingLoadStaysDisposed pins the terminal state: an
// in-flight failing load must not rewrite disposed back to failed.
func TestDisposeDuringFailingLoadStaysDisposed(t *testing.T) {
	root := cordis.New()

	var runs atomic.Int32
	entered := make(chan struct{})
	releaseBody := make(chan struct{})
	boom := errors.New("boom")

	plugin := cordis.Define[struct{}]("failing", func(ctx *cordis.Context, _ struct{}) error {
		n := runs.Add(1)
		ctx.OnDispose(func() {})
		if n == 2 {
			close(entered)
			<-releaseBody
			return boom
		}
		return nil
	}).WithInject("dep")

	fiber, err := root.Load(plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	dep1, err := root.Load(providePlugin("dep1", "dep"), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, fiber, cordis.StateActive)

	dep1.Dispose()
	waitFiberState(t, fiber, cordis.StatePending)

	dep2Done := make(chan error, 1)
	go func() {
		_, err := root.Load(providePlugin("dep2", "dep"), struct{}{})
		dep2Done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second load did not start")
	}

	fiber.Dispose()
	close(releaseBody)
	waitFiberState(t, fiber, cordis.StateDisposed)
	if got := len(fiber.Effects()); got != 0 {
		t.Fatalf("effects after dispose = %d, want 0", got)
	}
	if err := <-dep2Done; err != nil {
		t.Fatal(err)
	}
}

// TestProviderReplacementDuringBusyLoadDoesNotStackEffects pins the load
// precondition: a satisfiable-to-satisfiable epoch change must recycle the old
// generation before running the plugin body again.
func TestProviderReplacementDuringBusyLoadDoesNotStackEffects(t *testing.T) {
	root := cordis.New()

	var runs atomic.Int32
	var mu sync.Mutex
	var effectsAtStart []int
	entered := make(chan struct{})
	releaseBody := make(chan struct{})
	thirdLoadDone := make(chan struct{})

	plugin := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		n := runs.Add(1)
		mu.Lock()
		effectsAtStart = append(effectsAtStart, len(ctx.Effects()))
		mu.Unlock()
		if n == 2 {
			close(entered)
			<-releaseBody
		}
		ctx.OnDispose(func() {})
		if n == 3 {
			close(thirdLoadDone)
		}
		return nil
	}).WithInject("db", "gate")

	consumer, err := root.Load(plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	db1, err := root.Load(providePlugin("db1", "db"), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	gate1, err := root.Load(providePlugin("gate1", "gate"), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, consumer, cordis.StateActive)

	gate1.Dispose()
	waitFiberState(t, consumer, cordis.StatePending)

	gate2Done := make(chan error, 1)
	go func() {
		_, err := root.Load(providePlugin("gate2", "gate"), struct{}{})
		gate2Done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second load did not start")
	}

	db1.Dispose()
	db2, err := root.Load(providePlugin("db2", "db"), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	close(releaseBody)

	select {
	case <-thirdLoadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("third load did not run")
	}
	waitFiberState(t, consumer, cordis.StateActive)

	mu.Lock()
	gotAtStart := append([]int(nil), effectsAtStart...)
	mu.Unlock()
	if len(gotAtStart) != 3 {
		t.Fatalf("effects at load starts = %v, want three loads", gotAtStart)
	}
	if gotAtStart[2] != 0 {
		t.Fatalf("effects at third load start = %d, want 0", gotAtStart[2])
	}
	if got := len(consumer.Effects()); got != 1 {
		t.Fatalf("effects after reload = %d, want 1", got)
	}
	if err := <-gate2Done; err != nil {
		t.Fatal(err)
	}
	_ = db2
}

// TestBusyDisposeReleasesParentHandle pins parent effect slot release on the
// busy disposal path.
func TestBusyDisposeReleasesParentHandle(t *testing.T) {
	root := cordis.New()

	var parentCtx *cordis.Context
	var childFiber *cordis.Fiber
	var runs atomic.Int32
	entered := make(chan struct{})
	releaseBody := make(chan struct{})

	childPlugin := cordis.Define[struct{}]("child", func(ctx *cordis.Context, _ struct{}) error {
		n := runs.Add(1)
		ctx.OnDispose(func() {})
		if n == 2 {
			close(entered)
			<-releaseBody
		}
		return nil
	}).WithInject("gate")

	parentPlugin := cordis.Define[struct{}]("parent", func(ctx *cordis.Context, _ struct{}) error {
		parentCtx = ctx
		child, err := ctx.Load(childPlugin, struct{}{})
		if err != nil {
			return err
		}
		childFiber = child
		return nil
	})
	parentFiber, err := root.Load(parentPlugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if parentCtx == nil || childFiber == nil {
		t.Fatal("parent did not capture its context")
	}

	gate1, err := parentCtx.Load(providePlugin("gate1", "gate"), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, childFiber, cordis.StateActive)

	gate1.Dispose()
	waitFiberState(t, childFiber, cordis.StatePending)

	gate2Done := make(chan error, 1)
	go func() {
		_, err := parentCtx.Load(providePlugin("gate2", "gate"), struct{}{})
		gate2Done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second load did not start")
	}

	before := len(parentFiber.Effects())
	childFiber.Dispose()
	close(releaseBody)
	waitFiberState(t, childFiber, cordis.StateDisposed)
	if got := len(parentFiber.Effects()); got != before-1 {
		t.Fatalf("parent effects after busy dispose = %d, want %d", got, before-1)
	}
	if err := <-gate2Done; err != nil {
		t.Fatal(err)
	}
}

// TestFailedLoadLeavesNoPhantomRuntime pins the registry bookkeeping: a load
// rejected because its parent started unloading must leave no runtime behind.
// Nothing else removes it, so Size and Plugins would report a plugin that has
// no fiber for the rest of the application's life.
func TestFailedLoadLeavesNoPhantomRuntime(t *testing.T) {
	root := cordis.New()
	registry, ok := cordis.Get[cordis.Registry](root, "registry")
	if !ok {
		t.Fatal("registry service missing")
	}

	var parentCtx *cordis.Context
	unloading := make(chan struct{})
	releaseParent := make(chan struct{})
	var blockOnce sync.Once
	parent := cordis.Define[struct{}]("parent", func(ctx *cordis.Context, _ struct{}) error {
		parentCtx = ctx
		ctx.OnDispose(func() {
			// Block the first unload so the parent stays in StateUnloading
			// until the test releases it: that is the window Load must survive.
			blockOnce.Do(func() {
				close(unloading)
				<-releaseParent
			})
		})
		return nil
	})
	parentFiber, err := root.Load(parent, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if parentCtx == nil {
		t.Fatal("parent did not capture its context")
	}

	restarted := make(chan struct{})
	go func() {
		defer close(restarted)
		if err := parentFiber.Restart(); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-unloading:
	case <-time.After(2 * time.Second):
		t.Fatal("parent did not start unloading")
	}
	waitFiberState(t, parentFiber, cordis.StateUnloading)

	child := cordis.Define[struct{}]("child", func(*cordis.Context, struct{}) error { return nil })
	if _, err := parentCtx.Load(child, struct{}{}); err == nil {
		t.Fatal("want a load on an unloading parent to fail, got nil")
	}
	if names := registry.Plugins(); len(names) != 1 || names[0] != "parent" {
		t.Fatalf("want plugins [parent] after the failed load, got %v", names)
	}
	if size := registry.Size(); size != 1 {
		t.Fatalf("want registry size 1 after the failed load, got %d", size)
	}

	close(releaseParent)
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("parent restart did not finish")
	}
	parentFiber.Dispose()
	if size := registry.Size(); size != 0 {
		t.Fatalf("want registry size 0 after dispose, got %d", size)
	}
}

// TestSetGetConcurrent exercises concurrent Set and Get calls. It is most
// useful under the race detector.
func TestSetGetConcurrent(t *testing.T) {
	root := cordis.New()
	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "v0"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			if _, ok := cordis.Get[*fakeDB](root, "db"); ok {
				continue
			}
			t.Error("db disappeared")
			return
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			if err := root.Set("db", &fakeDB{name: "v"}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
}
