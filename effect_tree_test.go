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
		root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
			scope.On("ev", func(struct{}) { hits.Add(1) })
			scope.OnDispose(func() { unwound.Add(1) })
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
		root.Effect("outer", func(outer *cordis.Context) cordis.Disposer {
			outer.OnDispose(func() { order = append(order, "outer-sibling") })
			outer.Effect("inner", func(inner *cordis.Context) cordis.Disposer {
				inner.OnDispose(func() { order = append(order, "inner-child") })
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
	root.Effect("after", func(*cordis.Context) cordis.Disposer {
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
		root.Effect("outer", func(scope *cordis.Context) cordis.Disposer {
			_, provideErr = cordis.Provide[*fakeDB](scope, "db", &fakeDB{name: "temp"})
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

// TestConcurrentEffectRegistrationsStayAtFiberLevel pins explicit ownership:
// overlapping bodies started through the same plugin context are siblings, so
// disposing one cannot unwind the other.
func TestConcurrentEffectRegistrationsStayAtFiberLevel(t *testing.T) {
	root := cordis.New()

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDisposerReady := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondExited := make(chan struct{})

	var (
		firstDisposer    cordis.Disposer
		secondUnwound    atomic.Int32
		thirdUnwound     atomic.Int32
		siblings         bool
		secondAfterFirst int32
	)

	plugin := cordis.Define[struct{}]("racy", func(ctx *cordis.Context, _ struct{}) error {
		go func() {
			firstDisposer = ctx.Effect("first", func(*cordis.Context) cordis.Disposer {
				close(firstEntered)
				<-releaseFirst
				return nil
			})
			close(firstDisposerReady)
		}()
		<-firstEntered

		go func() {
			ctx.Effect("second", func(*cordis.Context) cordis.Disposer {
				close(secondEntered)
				<-releaseSecond
				return func() { secondUnwound.Add(1) }
			})
			close(secondExited)
		}()
		<-secondEntered

		effects := ctx.Effects()
		siblings = len(effects) == 2 && effects[0].Label == "first" &&
			effects[1].Label == "second" && len(effects[0].Children()) == 0 &&
			len(effects[1].Children()) == 0

		close(releaseFirst)
		<-firstDisposerReady
		firstDisposer()
		secondAfterFirst = secondUnwound.Load()
		close(releaseSecond)
		<-secondExited

		ctx.Effect("third", func(*cordis.Context) cordis.Disposer {
			return func() { thirdUnwound.Add(1) }
		})
		return nil
	})

	fiber, err := root.Load(plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !siblings {
		t.Fatalf("want overlapping effects as siblings, got %+v", fiber.Effects())
	}
	if secondAfterFirst != 0 {
		t.Fatalf("want second alive after disposing first, got %d unwinds", secondAfterFirst)
	}
	fiber.Dispose()
	if got := secondUnwound.Load(); got != 1 {
		t.Fatalf("want the second effect unwound once by fiber disposal, got %d", got)
	}
	if got := thirdUnwound.Load(); got != 1 {
		t.Fatalf("want the third effect unwound once by fiber disposal, got %d", got)
	}
	if got := len(fiber.Effects()); got != 0 {
		t.Fatalf("want no live effects, got %d", got)
	}
}
