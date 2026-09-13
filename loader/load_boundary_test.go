package loader_test

import (
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

func TestTreeLoadRollsBackValidatePanic(t *testing.T) {
	registry := loader.NewRegistry()
	applied := 0
	disposed := 0
	loader.MustRegister(registry, "good",
		cordis.Define[struct{}]("good", func(ctx *cordis.Context, _ struct{}) error {
			applied++
			ctx.OnDispose(func() { disposed++ })
			return nil
		}))
	loader.MustRegister(registry, "bad",
		cordis.Define[struct{}]("bad", func(*cordis.Context, struct{}) error {
			t.Fatal("want validation to stop before plugin body, got plugin body call")
			return nil
		}).WithValidate(func(*struct{}) error {
			panic("invalid config")
		}))

	tree, err := loader.Compose([]loader.Layer{{Label: "base", Entries: []*loader.Patch{
		{ID: "good", Name: strptr("good")},
		{ID: "bad", Name: strptr("bad")},
	}}})
	if err != nil {
		t.Fatalf("want composition success, got %v", err)
	}
	root := cordis.New()
	before := len(root.Effects())

	fibers, err := tree.Load(root, registry)
	if fibers != nil {
		t.Fatalf("want nil fibers after rollback, got %v", fibers)
	}
	if err == nil || !strings.Contains(err.Error(), `entry "bad" (bad)`) ||
		!strings.Contains(err.Error(), "invalid config") {
		t.Fatalf("want the validation panic attributed to bad, got %v", err)
	}
	if applied != 1 || disposed != 1 {
		t.Fatalf("want the good plugin applied and disposed once, got %d/%d", applied, disposed)
	}
	if got := len(root.Effects()); got != before {
		t.Fatalf("want effects back at %d after rollback, got %d", before, got)
	}
	reg, ok := cordis.Get[cordis.Registry](root, "registry")
	if !ok {
		t.Fatal("want the registry service after rollback, got none")
	}
	if got := reg.Plugins(); len(got) != 0 {
		t.Fatalf("want no plugin with live fibers after rollback, got %v", got)
	}
}
