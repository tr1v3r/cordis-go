package cordis_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cordis "github.com/tr1v3r/cordis-go"
)

// baseKey is a private context key, so a test value cannot collide with one
// another package stores on the same context.
type baseKey struct{}

func TestWithBaseContextCancelDisposesRoot(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	root := cordis.New(cordis.WithBaseContext(base))

	unwound := make(chan struct{})
	plugin := cordis.Define[struct{}]("worker", func(ctx *cordis.Context, _ struct{}) error {
		ctx.OnDispose(func() { close(unwound) })
		return nil
	})
	fiber, err := cordis.Load(root, plugin, struct{}{})
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case <-unwound:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the base context must unwind loaded plugins")
	}
	waitForState(t, root.Fiber(), cordis.StateDisposed)
	waitForState(t, fiber, cordis.StateDisposed)

	if err := root.Context().Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	select {
	case <-root.Done():
	default:
		t.Fatal("root Done must be closed once the base context is cancelled")
	}
}

func TestWithBaseContextDerivesLifecycleContext(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	base, cancel := context.WithDeadline(
		context.WithValue(context.Background(), baseKey{}, "carried"), deadline)
	defer cancel()

	root := cordis.New(cordis.WithBaseContext(base))
	if got := root.Context().Value(baseKey{}); got != "carried" {
		t.Fatalf("want carried, got %v", got)
	}
	got, ok := root.Context().Deadline()
	if !ok || !got.Equal(deadline) {
		t.Fatalf("want deadline %v, got %v ok=%v", deadline, got, ok)
	}

	seen := ""
	plugin := cordis.Define[struct{}]("reader", func(ctx *cordis.Context, _ struct{}) error {
		seen, _ = ctx.Context().Value(baseKey{}).(string)
		return nil
	})
	if _, err := cordis.Load(root, plugin, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if seen != "carried" {
		t.Fatalf("want carried, got %q", seen)
	}
}

func TestWithBaseContextAlreadyCancelled(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	cancel()

	// New must still build a complete root: the watcher fires while it runs.
	root := cordis.New(cordis.WithBaseContext(base))
	waitForState(t, root.Fiber(), cordis.StateDisposed)
	if _, ok := cordis.Get[cordis.Registry](root, "registry"); ok {
		t.Fatal("built-in services must be released with the root fiber")
	}
}

func TestWithBaseContextReleasedByDispose(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := cordis.New(cordis.WithBaseContext(base))

	if !hasEffect(root, "ctx.WithBaseContext") {
		t.Fatal("the base context watcher must be registered as a root effect")
	}
	root.Fiber().Dispose()
	if hasEffect(root, "ctx.WithBaseContext") {
		t.Fatal("disposing the root must release the base context watcher")
	}
	// The watcher was stopped, so a late cancel must be a no-op rather than a
	// second teardown.
	cancel()
	if state := root.Fiber().State(); state != cordis.StateDisposed {
		t.Fatalf("want disposed, got %s", state)
	}
}

func TestRootWithoutBaseContextRegistersNoWatcher(t *testing.T) {
	root := cordis.New()
	if hasEffect(root, "ctx.WithBaseContext") {
		t.Fatal("a root without a base context must not register a watcher")
	}
	if err := root.Context().Err(); err != nil {
		t.Fatalf("want a live context, got %v", err)
	}
}

func hasEffect(root *cordis.Context, label string) bool {
	for _, meta := range root.Effects() {
		if meta.Label == label {
			return true
		}
	}
	return false
}

// waitForState polls until the fiber reaches want, because a base context is
// observed from the goroutine that cancels it.
func waitForState(t *testing.T, fiber *cordis.Fiber, want cordis.FiberState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fiber.State() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("want %s, got %s", want, fiber.State())
}
