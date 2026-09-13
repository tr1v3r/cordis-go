package cordis_test

import (
	"sync/atomic"
	"testing"
	"time"

	cordis "github.com/tr1v3r/cordis-go"
)

// TestProviderRebindingDuringBusyLoadReloadsDependent pins the epoch identity: a
// provider that releases its registration and provides the same name again keeps
// its UID, so only the binding identity can tell the dependent that the object it
// resolved has been replaced.
func TestProviderRebindingDuringBusyLoadReloadsDependent(t *testing.T) {
	root := cordis.New()
	type rebindDB struct{ gen int }

	var (
		unbind      cordis.Disposer
		providerCtx *cordis.Context
	)
	provider := cordis.Define[struct{}]("provider", func(ctx *cordis.Context, _ struct{}) error {
		disposer, err := cordis.Provide[*rebindDB](ctx, "db", &rebindDB{gen: 1})
		if err != nil {
			return err
		}
		providerCtx, unbind = ctx, disposer
		return nil
	})
	if _, err := root.Load(provider, struct{}{}); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var (
		runs        atomic.Int32
		consumerCtx *cordis.Context
	)
	consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		consumerCtx = ctx
		if runs.Add(1) == 2 {
			close(entered)
			<-release
		}
		return nil
	}).WithInject("db")
	consumerFiber, err := root.Load(consumer, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, consumerFiber, cordis.StateActive)

	restarted := make(chan error, 1)
	go func() { restarted <- consumerFiber.Restart() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the restart did not reach the second load")
	}

	// The provider replaces its own binding while the dependent's body runs.
	unbind()
	if _, err := cordis.Provide[*rebindDB](providerCtx, "db", &rebindDB{gen: 2}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-restarted; err != nil {
		t.Fatal(err)
	}

	waitFiberState(t, consumerFiber, cordis.StateActive)
	if got := runs.Load(); got != 3 {
		t.Fatalf("want a third load against the rebound service, got %d runs", got)
	}
	service, ok := cordis.Get[*rebindDB](consumerCtx, "db")
	if !ok || service.gen != 2 {
		t.Fatalf("want the rebound service (gen 2), got %+v (ok=%v)", service, ok)
	}
}
