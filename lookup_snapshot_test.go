package cordis_test

import (
	"sync/atomic"
	"testing"

	"github.com/tr1v3r/cordis-go"
)

// TestLookupPinnedBindingSurvivesProviderUnload pins the documented split
// between the dependency snapshot and the live registry: a dependent keeps the
// binding it loaded against while its own generation runs, even after its
// provider unloads, and loses it once its own transition settles.
func TestLookupPinnedBindingSurvivesProviderUnload(t *testing.T) {
	root := cordis.New()
	provider := cordis.Define[struct{}]("provider", func(ctx *cordis.Context, _ struct{}) error {
		_, err := cordis.Provide[*fakeDB](ctx, "db", &fakeDB{name: "pinned"})
		return err
	})
	providerFiber, err := cordis.Load(root, provider, struct{}{})
	if err != nil {
		t.Fatal(err)
	}

	var pinned *fakeDB
	var pinnedOK bool
	consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		// Dispose the provider from inside this fiber's own transition: the
		// rerun the disposal asks for is deferred, so the snapshot is what the
		// dependent still resolves against.
		providerFiber.Dispose()
		pinned, pinnedOK = cordis.Get[*fakeDB](ctx, "db")
		return nil
	}).WithInject("db")

	consumerFiber, err := cordis.Load(root, consumer, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !pinnedOK || pinned == nil || pinned.name != "pinned" {
		t.Fatalf("want the pinned binding during the transition, got %v ok=%v", pinned, pinnedOK)
	}
	if got, ok := consumerFiber.Ctx.Lookup("db"); ok || got != nil {
		t.Fatalf("want the settled fiber to lose the binding, got %v ok=%v", got, ok)
	}
}

// TestPinnedBindingStillHonorsAvailabilityCheck pins the other half of the
// documented snapshot contract: the pinned path ignores provider activity but
// still re-checks the availability predicate, so a pinned binding hides while
// its check fails and reappears once the check passes.
func TestPinnedBindingStillHonorsAvailabilityCheck(t *testing.T) {
	root := cordis.New()
	var ready atomic.Bool
	ready.Store(true)
	provider := cordis.Define[struct{}]("provider", func(ctx *cordis.Context, _ struct{}) error {
		_, err := cordis.ProvideChecked[*fakeDB](ctx, "db", &fakeDB{name: "checked"},
			ready.Load)
		return err
	})
	if _, err := cordis.Load(root, provider, struct{}{}); err != nil {
		t.Fatal(err)
	}

	var okWhileDown, okWhileUp bool
	consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		// The provider stays active the whole time, so both reads resolve
		// through the snapshot the injection pinned; the predicate must be
		// what decides them.
		ready.Store(false)
		_, okWhileDown = cordis.Get[*fakeDB](ctx, "db")
		ready.Store(true)
		_, okWhileUp = cordis.Get[*fakeDB](ctx, "db")
		return nil
	}).WithInject("db")

	if _, err := cordis.Load(root, consumer, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if okWhileDown {
		t.Fatal("want a failing availability check to hide even a pinned binding")
	}
	if !okWhileUp {
		t.Fatal("want the pinned binding to resolve once the check passes")
	}
}
