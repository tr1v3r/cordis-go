package cordis_test

import (
	"reflect"
	"sync/atomic"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

// TestPanicInEffectBodyUnwindsNestedEffects pins effect-tree reachability: the
// effects an effect body registered before panicking must still unwind, and
// must not stay registered behind the fiber.
func TestPanicInEffectBodyUnwindsNestedEffects(t *testing.T) {
	root := cordis.New()
	before := len(root.Effects()) // the three built-in services

	var (
		unwound atomic.Int32
		hits    atomic.Int32
	)
	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		root.Effect("outer", func() cordis.Disposer {
			root.On("ev", func(struct{}) { hits.Add(1) })
			root.OnDispose(func() { unwound.Add(1) })
			panic("boom")
		})
		return false
	}()
	if !panicked {
		t.Fatal("want the effect body panic to propagate")
	}

	if got := unwound.Load(); got != 1 {
		t.Fatalf("want the nested disposer to run once, got %d", got)
	}
	if got := len(root.Effects()); got != before {
		t.Fatalf("want effects back at %d, got %d", before, got)
	}
	root.Emit("ev", struct{}{})
	if got := hits.Load(); got != 0 {
		t.Fatalf("want the nested listener to be gone, got %d deliveries", got)
	}
}

// TestNestedEffectPanicUnwindsWholeCascade pins the nested panic path: a body
// that panics detaches its entry from its parent and unwinds the effects it had
// already adopted, and the enclosing body's own panic then unwinds its remaining
// children too, innermost first. Nothing may stay attached behind the fiber and
// the fiber must keep accepting reachable effects afterwards.
func TestNestedEffectPanicUnwindsWholeCascade(t *testing.T) {
	root := cordis.New()
	before := len(root.Effects()) // the three built-in services

	var order []string
	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		root.Effect("outer", func() cordis.Disposer {
			root.OnDispose(func() { order = append(order, "outer-sibling") })
			root.Effect("inner", func() cordis.Disposer {
				root.OnDispose(func() { order = append(order, "inner-child") })
				panic("boom")
			})
			return func() {}
		})
		return false
	}()
	if !panicked {
		t.Fatal("want the effect body panic to propagate")
	}

	want := []string{"inner-child", "outer-sibling"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("want %v, got %v", want, order)
	}
	if got := len(root.Effects()); got != before {
		t.Fatalf("want effects back at %d, got %d", before, got)
	}

	// An effect registered after the cascade must be reachable: disposal still
	// unwinds it exactly once.
	unwound := 0
	root.Effect("after", func() cordis.Disposer {
		return func() { unwound++ }
	})
	root.Fiber().Dispose()
	if unwound != 1 {
		t.Fatalf("want the post-cascade effect unwound once by disposal, got %d", unwound)
	}
}

// TestServiceProvidedInPanickingBodyReleasesName pins the registry consequence
// of the panic unwind: a service provided by a body that panics must not keep
// its name occupied, because the unwind reaches the Provide disposer the body
// never returned.
func TestServiceProvidedInPanickingBodyReleasesName(t *testing.T) {
	root := cordis.New()

	var provideErr error
	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		root.Effect("outer", func() cordis.Disposer {
			_, provideErr = cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "temp"})
			panic("boom")
		})
		return false
	}()
	if !panicked {
		t.Fatal("want the effect body panic to propagate")
	}
	if provideErr != nil {
		t.Fatalf("want the provide inside the body to succeed, got %v", provideErr)
	}

	// The name is free again: providing it once more must not collide with the
	// binding of the aborted generation.
	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "kept"}); err != nil {
		t.Fatalf("want the name released by the unwind, got %v", err)
	}
	root.Fiber().Dispose()
}

// TestConcurrentEffectRegistrationKeepsEffectsReachable pins the effect-tree
// invariant under concurrent registration: an effect registered after two
// overlapping bodies must still be unwound by the fiber's disposal, even though
// one of those bodies restored the enclosing scope while the other still ran.
func TestConcurrentEffectRegistrationKeepsEffectsReachable(t *testing.T) {
	root := cordis.New()

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDisposerReady := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondExited := make(chan struct{})

	var (
		firstDisposer cordis.Disposer
		thirdUnwound  atomic.Int32
	)

	plugin := cordis.Define[struct{}]("racy", func(ctx *cordis.Context, _ struct{}) error {
		// The first body blocks, so the fiber's current effect is E1.
		go func() {
			firstDisposer = ctx.Effect("first", func() cordis.Disposer {
				close(firstEntered)
				<-releaseFirst
				return func() {}
			})
			close(firstDisposerReady)
		}()
		<-firstEntered

		// A second body starts while E1 is open, then blocks inside it.
		go func() {
			ctx.Effect("second", func() cordis.Disposer {
				close(secondEntered)
				<-releaseSecond
				return func() {}
			})
			close(secondExited)
		}()
		<-secondEntered

		// The first body returns and its effect is disposed while the second
		// body still runs: the second body must not restore the dead E1.
		close(releaseFirst)
		<-firstDisposerReady
		firstDisposer()
		close(releaseSecond)
		<-secondExited

		ctx.Effect("third", func() cordis.Disposer {
			return func() { thirdUnwound.Add(1) }
		})
		return nil
	})

	fiber, err := root.Load(plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	fiber.Dispose()
	if got := thirdUnwound.Load(); got != 1 {
		t.Fatalf("want the third effect unwound once by disposal, got %d (effects = %d)",
			got, len(fiber.Effects()))
	}
	if got := len(fiber.Effects()); got != 0 {
		t.Fatalf("want no live effects, got %d", got)
	}
}
