package cordis_test

import (
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
