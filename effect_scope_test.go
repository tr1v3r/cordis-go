package cordis_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

func wantInactiveEffectPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		reason := recover()
		failure, ok := reason.(error)
		if !ok {
			t.Fatalf("want an error panic, got %v", reason)
		}
		wantInactiveEffectError(t, failure)
	}()
	fn()
}

func wantInactiveEffectError(t *testing.T, err error) {
	t.Helper()
	var failure *cordis.Error
	if !errors.As(err, &failure) || failure.Code != cordis.ErrInactiveEffect {
		t.Fatalf("want error code %s, got %v", cordis.ErrInactiveEffect, err)
	}
}

func findEffect(ctx *cordis.Context, label string) *cordis.EffectMeta {
	for _, effect := range ctx.Effects() {
		if effect.Label == label {
			return effect
		}
	}
	return nil
}

func TestEffectScopeSeparatesChildrenFromOriginalContext(t *testing.T) {
	root := cordis.New()
	before := len(root.Effects())
	var outerUnwound, childUnwound, siblingUnwound int

	outer := root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
		scope.OnDispose(func() { childUnwound++ })
		root.OnDispose(func() { siblingUnwound++ })
		return func() { outerUnwound++ }
	})

	meta := findEffect(root, "outer")
	if meta == nil {
		t.Fatal("want the outer effect at fiber level")
	}
	if got := len(meta.Children()); got != 1 {
		t.Fatalf("want 1 explicitly scoped child, got %d", got)
	}
	if got := len(root.Effects()); got != before+2 {
		t.Fatalf("want outer and sibling at fiber level, got %d new effects", got-before)
	}

	outer()
	if outerUnwound != 1 || childUnwound != 1 || siblingUnwound != 0 {
		t.Fatalf("want outer/child/sibling unwinds 1/1/0, got %d/%d/%d",
			outerUnwound, childUnwound, siblingUnwound)
	}
	if got := len(root.Effects()); got != before+1 {
		t.Fatalf("want only the sibling left above baseline, got %d effects", got)
	}

	root.Fiber().Dispose()
	if siblingUnwound != 1 {
		t.Fatalf("want the original-context sibling unwound by the fiber, got %d", siblingUnwound)
	}
}

func TestEffectScopePropagatesThroughForkAndIsolation(t *testing.T) {
	root := cordis.New()
	var isolated *cordis.Context
	var unwound int
	var provideErr error

	outer := root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
		scope.Fork("worker").OnDispose(func() { unwound++ })
		isolated = scope.Isolate("db")
		_, provideErr = isolated.Provide("db", &fakeDB{name: "scoped"})
		return nil
	})
	if provideErr != nil {
		t.Fatal(provideErr)
	}
	if _, ok := isolated.Get[*fakeDB]("db"); !ok {
		t.Fatal("want the scoped isolated service while its owner is live")
	}
	meta := findEffect(root, "outer")
	if meta == nil || len(meta.Children()) != 2 {
		t.Fatalf("want forked disposer and isolated service under outer, got %+v", meta)
	}

	outer()
	if unwound != 1 {
		t.Fatalf("want the forked disposer unwound once, got %d", unwound)
	}
	if _, ok := isolated.Get[*fakeDB]("db"); ok {
		t.Fatal("want the isolated service removed with its explicit owner")
	}
	root.Fiber().Dispose()
}

func TestExpiredEffectScopeRejectsRegistrations(t *testing.T) {
	root := cordis.New()
	defer root.Fiber().Dispose()
	before := len(root.Effects())

	var scope *cordis.Context
	outer := root.Effect("outer", func(current *cordis.Context) cordis.Disposer {
		scope = current
		return nil
	})
	live := len(root.Effects())

	bodyRuns := 0
	wantInactiveEffectPanic(t, func() {
		scope.Effect("late", func(*cordis.Context) cordis.Disposer {
			bodyRuns++
			return nil
		})
	})
	wantInactiveEffectPanic(t, func() { scope.OnDispose(func() { bodyRuns++ }) })
	wantInactiveEffectPanic(t, func() {
		scope.On("late", func(struct{}) { bodyRuns++ })
	})
	wantInactiveEffectPanic(t, func() {
		scope.Fork("late-fork").OnDispose(func() { bodyRuns++ })
	})

	if _, err := scope.Provide("late", &fakeDB{}); err == nil {
		t.Fatal("want an expired-scope Provide error")
	} else {
		wantInactiveEffectError(t, err)
	}
	if _, err := scope.Isolate("db").Provide("db", &fakeDB{}); err == nil {
		t.Fatal("want an expired isolated-scope Provide error")
	} else {
		wantInactiveEffectError(t, err)
	}

	service := &startableService{}
	if _, err := scope.Serve("late-serve", service); err == nil {
		t.Fatal("want an expired-scope Serve error")
	} else {
		wantInactiveEffectError(t, err)
	}
	if service.started {
		t.Fatal("an expired-scope Serve must not call Start")
	}

	pluginRuns := 0
	plugin := cordis.Define[struct{}]("late-plugin", func(*cordis.Context, struct{}) error {
		pluginRuns++
		return nil
	})
	if _, err := scope.Load(plugin, struct{}{}); err == nil {
		t.Fatal("want an expired-scope Load error")
	} else {
		wantInactiveEffectError(t, err)
	}
	injectRuns := 0
	if _, err := cordis.Inject(scope, nil, func(*cordis.Context) error {
		injectRuns++
		return nil
	}); err == nil {
		t.Fatal("want an expired-scope Inject error")
	} else {
		wantInactiveEffectError(t, err)
	}

	root.Emit("late", struct{}{})
	if bodyRuns != 0 || pluginRuns != 0 || injectRuns != 0 {
		t.Fatalf("want no expired-scope user code, got body/plugin/inject %d/%d/%d",
			bodyRuns, pluginRuns, injectRuns)
	}
	if got := len(root.Effects()); got != live {
		t.Fatalf("want failed registrations to leave %d effects, got %d", live, got)
	}
	registry, ok := root.Get[cordis.Registry]("registry")
	if !ok {
		t.Fatal("registry service missing")
	}
	if got := registry.Size(); got != 0 {
		t.Fatalf("want no runtime left by expired loads, got %d", got)
	}

	outer()
	if got := len(root.Effects()); got != before {
		t.Fatalf("want only baseline effects after outer disposal, got %d", got)
	}
}

func TestConcurrentRegistrationsThroughLiveEffectScopeBecomeChildren(t *testing.T) {
	root := cordis.New()
	defer root.Fiber().Dispose()

	scopeReady := make(chan *cordis.Context, 1)
	release := make(chan struct{})
	outerReady := make(chan cordis.Disposer, 1)
	go func() {
		outerReady <- root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
			scopeReady <- scope
			<-release
			return nil
		})
	}()
	scope := <-scopeReady

	const registrations = 32
	var wg sync.WaitGroup
	var unwound atomic.Int32
	wg.Add(registrations)
	for range registrations {
		go func() {
			defer wg.Done()
			scope.OnDispose(func() { unwound.Add(1) })
		}()
	}
	wg.Wait()

	meta := findEffect(root, "outer")
	if meta == nil || len(meta.Children()) != registrations {
		got := 0
		if meta != nil {
			got = len(meta.Children())
		}
		t.Fatalf("want %d concurrent children, got %d", registrations, got)
	}
	close(release)
	outer := <-outerReady
	outer()
	if got := unwound.Load(); got != registrations {
		t.Fatalf("want %d concurrent children unwound, got %d", registrations, got)
	}
}

func TestEffectScopeRegistrationRacingBodyReturnNeverEscapes(t *testing.T) {
	for round := 0; round < 100; round++ {
		root := cordis.New()
		before := len(root.Effects())
		scopeReady := make(chan *cordis.Context, 1)
		release := make(chan struct{})
		outerReady := make(chan cordis.Disposer, 1)
		go func() {
			outerReady <- root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
				scopeReady <- scope
				<-release
				return nil
			})
		}()
		scope := <-scopeReady

		gun := make(chan struct{})
		registrationDone := make(chan any, 1)
		var unwound atomic.Int32
		go func() {
			<-gun
			var failure any
			func() {
				defer func() { failure = recover() }()
				scope.OnDispose(func() { unwound.Add(1) })
			}()
			registrationDone <- failure
		}()
		go func() {
			<-gun
			close(release)
		}()
		close(gun)

		outer := <-outerReady
		failure := <-registrationDone
		outer()
		if failure == nil {
			if got := unwound.Load(); got != 1 {
				t.Fatalf("round %d: want an adopted child unwound once, got %d", round, got)
			}
		} else {
			err, ok := failure.(error)
			if !ok {
				t.Fatalf("round %d: want an error panic, got %v", round, failure)
			}
			wantInactiveEffectError(t, err)
			if got := unwound.Load(); got != 0 {
				t.Fatalf("round %d: want a rejected child never run, got %d", round, got)
			}
		}
		if got := len(root.Effects()); got != before {
			t.Fatalf("round %d: want no escaped effect above baseline, got %d", round, got)
		}
		root.Fiber().Dispose()
	}
}

func TestEffectScopeOwnsLoadedFiberButNotItsPluginContext(t *testing.T) {
	root := cordis.New()
	defer root.Fiber().Dispose()
	var childUnwound atomic.Int32
	plugin := cordis.Define[struct{}]("scoped-load", func(ctx *cordis.Context, _ struct{}) error {
		ctx.OnDispose(func() { childUnwound.Add(1) })
		return nil
	})

	var child *cordis.Fiber
	var loadErr error
	outer := root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
		child, loadErr = scope.Load(plugin, struct{}{})
		return nil
	})
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := child.State(); got != cordis.StateActive {
		t.Fatalf("want loaded child active, got %s", got)
	}
	meta := findEffect(root, "outer")
	if meta == nil || len(meta.Children()) != 1 || meta.Children()[0].Label != "child" {
		t.Fatalf("want one child lifetime under outer, got %+v", meta)
	}
	if got := len(child.Effects()); got != 1 {
		t.Fatalf("want the plugin context reset to its own fiber with 1 effect, got %d", got)
	}

	outer()
	if got := child.State(); got != cordis.StateDisposed {
		t.Fatalf("want scoped child disposed with outer, got %s", got)
	}
	if got := childUnwound.Load(); got != 1 {
		t.Fatalf("want the child plugin effect unwound once, got %d", got)
	}
	registry, ok := root.Get[cordis.Registry]("registry")
	if !ok {
		t.Fatal("registry service missing")
	}
	if got := registry.Size(); got != 0 {
		t.Fatalf("want scoped child removed from registry, got %d runtimes", got)
	}
}

func TestNestedDisposerWaitsForOwnerUnwindToFinish(t *testing.T) {
	root := cordis.New()
	defer root.Fiber().Dispose()
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	outerDone := make(chan struct{})
	innerDone := make(chan struct{})

	var inner cordis.Disposer
	var provideErr error
	outer := root.Effect("outer", func(outerScope *cordis.Context) cordis.Disposer {
		inner = outerScope.Effect("inner", func(innerScope *cordis.Context) cordis.Disposer {
			_, provideErr = innerScope.Provide("scoped-db", &fakeDB{name: "scoped"})
			return func() {
				close(cleanupStarted)
				<-releaseCleanup
			}
		})
		return nil
	})
	if provideErr != nil {
		t.Fatal(provideErr)
	}

	go func() {
		outer()
		close(outerDone)
	}()
	<-cleanupStarted
	go func() {
		inner()
		close(innerDone)
	}()

	select {
	case <-innerDone:
		t.Fatal("nested disposer returned before the owner unwind finished")
	default:
	}
	if _, ok := root.Get[*fakeDB]("scoped-db"); !ok {
		t.Fatal("want the nested service live while its owner disposer is blocked")
	}

	close(releaseCleanup)
	<-outerDone
	<-innerDone
	if _, ok := root.Get[*fakeDB]("scoped-db"); ok {
		t.Fatal("want the nested service gone when both disposers return")
	}
}

func TestOwnerDisposerWaitsForNestedHandleToFinish(t *testing.T) {
	root := cordis.New()
	defer root.Fiber().Dispose()
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	outerDone := make(chan struct{})
	innerDone := make(chan struct{})

	var inner cordis.Disposer
	var provideErr error
	outer := root.Effect("outer", func(outerScope *cordis.Context) cordis.Disposer {
		inner = outerScope.Effect("inner", func(innerScope *cordis.Context) cordis.Disposer {
			_, provideErr = innerScope.Provide("scoped-db", &fakeDB{name: "scoped"})
			return func() {
				close(cleanupStarted)
				<-releaseCleanup
			}
		})
		return nil
	})
	if provideErr != nil {
		t.Fatal(provideErr)
	}

	go func() {
		inner()
		close(innerDone)
	}()
	<-cleanupStarted
	go func() {
		outer()
		close(outerDone)
	}()

	select {
	case <-outerDone:
		t.Fatal("owner disposer returned before the nested handle finished")
	default:
	}
	if _, ok := root.Get[*fakeDB]("scoped-db"); !ok {
		t.Fatal("want the nested service live while its disposer is blocked")
	}

	close(releaseCleanup)
	<-innerDone
	<-outerDone
	if _, ok := root.Get[*fakeDB]("scoped-db"); ok {
		t.Fatal("want the nested service gone when both disposers return")
	}
}

type blockingScopedService struct {
	started chan struct{}
	release chan struct{}
	stops   atomic.Int32
}

func (s *blockingScopedService) Start() error {
	close(s.started)
	<-s.release
	return nil
}

func (s *blockingScopedService) Stop() error {
	s.stops.Add(1)
	return nil
}

func TestServeUnwindsWhenOwnerDisposesDuringStart(t *testing.T) {
	root := cordis.New()
	defer root.Fiber().Dispose()
	service := &blockingScopedService{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	serveDone := make(chan error, 1)
	outerReady := make(chan cordis.Disposer, 1)

	go func() {
		outerReady <- root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
			go func() {
				_, err := scope.Serve("server", service)
				serveDone <- err
			}()
			<-service.started
			return nil
		})
	}()

	outer := <-outerReady
	outer()
	close(service.release)
	if err := <-serveDone; err != nil {
		t.Fatalf("want Serve to finish after deferred teardown, got %v", err)
	}
	if got := service.stops.Load(); got != 1 {
		t.Fatalf("want Stop called once after Start completed, got %d", got)
	}
	if _, ok := root.Get[*blockingScopedService]("server"); ok {
		t.Fatal("want service unregistered after its owner disposed")
	}
}
