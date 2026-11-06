package loader_test

import (
	"errors"
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

func TestTreeLoadRollsBackFailureTriggeredByLaterProvider(t *testing.T) {
	root := cordis.New()
	defer root.Fiber().Dispose()
	registry := loader.NewRegistry()
	failure := errors.New("start failed")
	loader.MustRegister(registry, "client", cordis.Define("client",
		func(*cordis.Context, int) error { return failure }).WithInject("db"))
	loader.MustRegister(registry, "server", cordis.Define("server",
		func(ctx *cordis.Context, _ int) error {
			_, err := ctx.Provide("db", 1)
			return err
		}))
	tree := loader.Tree{Nodes: []*loader.Node{{Name: "client"}, {Name: "server"}}}
	before := len(root.Effects())
	fibers, err := tree.Load(root, registry)
	if fibers != nil || !errors.Is(err, failure) {
		t.Fatalf("want nil fibers and startup failure, got %v and %v", fibers, err)
	}
	if _, ok := root.Lookup("db"); ok || len(root.Effects()) != before {
		t.Fatalf("want absent db and %d effects, got found=%v and %d",
			before, ok, len(root.Effects()))
	}
}
