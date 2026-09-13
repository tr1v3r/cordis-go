package cordis_test

import (
	"sync"
	"sync/atomic"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

// childEffects counts the fiber-level entries a load registers on its parent.
func childEffects(ctx *cordis.Context) int {
	count := 0
	for _, entry := range ctx.Effects() {
		if entry.Label == "child" {
			count++
		}
	}
	return count
}

func TestLoadDoesNotJoinARunningEffectBody(t *testing.T) {
	root := cordis.New()
	defer root.Fiber().Dispose()

	var unwound atomic.Int32
	plugin := cordis.Define[struct{}]("scoped-child", func(ctx *cordis.Context, _ struct{}) error {
		ctx.OnDispose(func() { unwound.Add(1) })
		return nil
	})

	// Park an effect body on another goroutine, so the fiber's scope marker is
	// held by work this test is not doing.
	entered := make(chan struct{})
	release := make(chan struct{})
	outer := make(chan cordis.Disposer, 1)
	go func() {
		outer <- root.Effect("outer", func() cordis.Disposer {
			close(entered)
			<-release
			return func() {}
		})
	}()
	<-entered

	child, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}

	close(release)
	(<-outer)()

	// The load belongs to the fiber, not to whatever body happened to run while
	// it was attaching: collecting that body must not take the fiber with it.
	if got := child.State(); got != cordis.StateActive {
		t.Fatalf("want the loaded fiber to outlive the effect, got %s", got)
	}
	if got := unwound.Load(); got != 0 {
		t.Fatalf("want no disposer run for the loaded fiber, got %d", got)
	}
	if got := childEffects(root); got != 1 {
		t.Fatalf("want 1 fiber-level child effect, got %d", got)
	}

	child.Dispose()
	if got := unwound.Load(); got != 1 {
		t.Fatalf("want the fiber's own disposer to run on dispose, got %d", got)
	}
}

func TestConcurrentLoadsKeepIndependentLifetimes(t *testing.T) {
	// Two loads of one plugin run on their own goroutines while the scope marker
	// is per fiber, so one load must not adopt the other's lifetime entry.
	for round := 0; round < 25; round++ {
		root := cordis.New()
		plugin := cordis.Define[struct{}]("twin", func(*cordis.Context, struct{}) error {
			return nil
		})

		var wg sync.WaitGroup
		fibers := make([]*cordis.Fiber, 2)
		errs := make([]error, 2)
		gun := make(chan struct{})
		for i := range fibers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-gun
				fibers[i], errs[i] = cordis.Load(root, plugin, struct{}{})
			}(i)
		}
		close(gun)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: load %d: %v", round, i, err)
			}
		}
		if got := childEffects(root); got != 2 {
			t.Fatalf("round %d: want 2 fiber-level child effects, got %d", round, got)
		}

		fibers[0].Dispose()
		if got := fibers[1].State(); got != cordis.StateActive {
			t.Fatalf("round %d: want the surviving twin active, got %s", round, got)
		}
		fibers[1].Dispose()
		root.Fiber().Dispose()
	}
}
