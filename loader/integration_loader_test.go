package loader_test

// Cross-feature integration tests for the loader domain. Each test composes a
// multi-layer tree that combines the scenarios of several fixes at once - a
// base layer whose children come from a nested insert (#28), duplicate ids
// kept by first registration (#24), group demotion and promotion (#23),
// entries that declare insert and fields at once (#26), and the mid-tree load
// failure that must dispose its own fiber and roll back across a group (#22).

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

// svcConfig lets the "svc" plugin record the config the loader handed it.
type svcConfig struct {
	URL string `json:"url"`
}

// taggedConfig names the entry a plugin was loaded for, so a test can assert
// the order loads and rollbacks happen in.
type taggedConfig struct {
	Tag string `json:"tag"`
}

// loadRecorder collects what the registry's plugins observe, in order.
type loadRecorder struct {
	urls   []string
	events []string
}

// newIntegrationRegistry registers the plugins the integration trees use:
// "svc" records its config, "tagged" records load and dispose order, "noop"
// does nothing, and "bad" fails its body with a fixed error.
func newIntegrationRegistry(rec *loadRecorder) *loader.Registry {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "svc",
		cordis.Define[svcConfig]("svc", func(_ *cordis.Context, cfg svcConfig) error {
			rec.urls = append(rec.urls, cfg.URL)
			return nil
		}))
	loader.MustRegister(registry, "tagged",
		cordis.Define[taggedConfig]("tagged", func(ctx *cordis.Context, cfg taggedConfig) error {
			rec.events = append(rec.events, "load:"+cfg.Tag)
			ctx.OnDispose(func() { rec.events = append(rec.events, "dispose:"+cfg.Tag) })
			return nil
		}))
	loader.MustRegister(registry, "noop",
		cordis.Define[struct{}]("noop", func(*cordis.Context, struct{}) error { return nil }))
	loader.MustRegister(registry, "bad",
		cordis.Define[struct{}]("bad", func(*cordis.Context, struct{}) error {
			return errors.New("boom")
		}))
	return registry
}

// TestIntegrationNestedInsertChildrenPatchAcrossLayers composes the scenario
// #28 makes possible: a group whose children come from a nested insert, then a
// later layer that patches one expanded child by id and disables another. The
// expanded children must behave like hand-written ones all the way to Load.
func TestIntegrationNestedInsertChildrenPatchAcrossLayers(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "stack", Group: boolptr(true),
		Plugins: []*loader.Patch{
			{
				Insert: []*loader.Patch{
					{ID: "api", Name: strptr("svc"), Config: map[string]any{"url": "http://a"}},
					{ID: "audit", Name: strptr("svc"), Config: map[string]any{"url": "http://u"}},
				},
			},
			{ID: "store", Name: strptr("svc"), Config: map[string]any{"url": "http://s"}},
		},
	}}}
	profile := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{
		{ID: "api", Config: map[string]any{"url": "http://patched"}},
		{ID: "audit", Disabled: boolptr(true)},
	}}

	tree, err := loader.Compose([]loader.Layer{base, profile})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tree.Warnings); got != 0 {
		t.Fatalf("want 0 warnings, got %d: %v", got, tree.Warnings)
	}
	if got := tree.Size(); got != 4 {
		t.Fatalf("want 4 entries (stack, api, audit, store), got %d", got)
	}
	node := tree.Find("api")
	if node == nil {
		t.Fatal("want the expanded insert child to stay addressable by id, got nil")
	}
	if node.Config["url"] != "http://patched" {
		t.Fatalf("want the later layer to patch the expanded child, got %v", node.Config)
	}
	if node.Name != "svc" {
		t.Fatalf("want the unmentioned name field preserved, got %q", node.Name)
	}
	if node.Source != "base" || !reflect.DeepEqual(node.Patched, []string{"profile"}) {
		t.Fatalf("want source base patched by profile, got source=%q patched=%v",
			node.Source, node.Patched)
	}
	audit := tree.Find("audit")
	if audit == nil || !audit.Disabled {
		t.Fatalf("want the audit child disabled by the patch layer, got %+v", audit)
	}

	dump := tree.DumpString()
	for _, want := range []string{
		"# layers: base -> profile",
		`- id: "stack"  # group  # from base`,
		`- id: "api"  # from base; patched by profile`,
		`config: {"url": "http://patched"}`,
		`- id: "audit"  # from base; patched by profile`,
		"  disabled: true",
	} {
		if !strings.Contains(dump, want) {
			t.Fatalf("want dump to contain %q, got:\n%s", want, dump)
		}
	}

	rec := &loadRecorder{}
	registry := newIntegrationRegistry(rec)
	fibers, err := tree.Load(cordis.New(), registry)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(fibers); got != 2 {
		t.Fatalf("want 2 fibers (api and store, audit disabled), got %d", got)
	}
	wantURLs := []string{"http://patched", "http://s"}
	if !reflect.DeepEqual(rec.urls, wantURLs) {
		t.Fatalf("want configs %v in load order, got %v", wantURLs, rec.urls)
	}
	for _, fiber := range fibers {
		if fiber.State() != cordis.StateActive {
			t.Fatalf("want %s active, got %s", fiber.Name(), fiber.State())
		}
	}
}

// TestIntegrationDuplicateIDFromExpandedInsertStrictFails crosses #28 with
// #24: the duplicate id only exists because a nested insert expanded. The
// first registration keeps the id in warning mode, Find and the patch agree on
// the same node, the shadowed entry stays untouched, and Strict turns the
// warning into the exact same text as an error.
func TestIntegrationDuplicateIDFromExpandedInsertStrictFails(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{
			ID: "g", Group: boolptr(true),
			Plugins: []*loader.Patch{
				{Insert: []*loader.Patch{{ID: "dup", Name: strptr("svc"),
					Config: map[string]any{"url": "inner"}}}},
			},
		},
		{ID: "dup", Name: strptr("svc"), Config: map[string]any{"url": "outer"}},
	}}
	patch := loader.Layer{Label: "p", Patch: true, Entries: []*loader.Patch{{
		ID: "dup", Config: map[string]any{"url": "patched"},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, patch})
	if err != nil {
		t.Fatal(err)
	}
	want := `layer base: duplicate entry id "dup"`
	if got := len(tree.Warnings); got != 1 || tree.Warnings[0] != want {
		t.Fatalf("want exactly one warning %q, got %v", want, tree.Warnings)
	}
	node := tree.Find("dup")
	if node == nil {
		t.Fatal("want the expanded child to keep the id, got nil")
	}
	if node.Config["url"] != "patched" {
		t.Fatalf("want the patch to reach the node Find returns, got %v", node.Config)
	}
	if shadow := shadowNode(tree, node); shadow == nil || shadow.Config["url"] != "outer" {
		t.Fatalf("want the shadowed top-level entry untouched at outer, got %+v", shadow)
	}
	if dump := tree.DumpString(); !strings.Contains(dump, "# warning: "+want) {
		t.Fatalf("want dump to carry the warning %q, got:\n%s", want, dump)
	}

	_, err = loader.Compose([]loader.Layer{base, patch}, loader.Strict())
	if err == nil {
		t.Fatal("want strict composition to reject the duplicate id")
	}
	if err.Error() != want {
		t.Fatalf("want %q, got %q", want, err)
	}
}

// TestIntegrationPatchRetargetsGroupness crosses #23 with layering: an entry
// declared as a group by the base layer can be demoted by a later patch, which
// must warn at Compose and fail at Load, and a non-group entry carrying
// children can be promoted in time for its children to load.
func TestIntegrationPatchRetargetsGroupness(t *testing.T) {
	t.Run("patch demotes a group", func(t *testing.T) {
		rec := &loadRecorder{}
		base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
			ID: "app", Name: strptr("noop"), Group: boolptr(true),
			Plugins: []*loader.Patch{{ID: "child", Name: strptr("noop")}},
		}}}
		demote := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
			ID: "app", Group: boolptr(false),
		}}}

		tree, err := loader.Compose([]loader.Layer{base, demote})
		if err != nil {
			t.Fatal(err)
		}
		want := `entry "app" has plugins but is not a group: they will not be loaded`
		if got := len(tree.Warnings); got != 1 || tree.Warnings[0] != want {
			t.Fatalf("want exactly one warning %q, got %v", want, tree.Warnings)
		}
		_, err = loader.Compose([]loader.Layer{base, demote}, loader.Strict())
		if err == nil {
			t.Fatal("want strict composition to reject the demoted entry")
		}
		wantStrict := "layer base: " + want
		if err.Error() != wantStrict {
			t.Fatalf("want %q, got %q", wantStrict, err)
		}

		_, err = tree.Load(cordis.New(), newIntegrationRegistry(rec))
		if err == nil {
			t.Fatal("want Load to refuse the demoted entry's children")
		}
		wantLoad := `loader: entry "app" (noop): plugins require group: true`
		if err.Error() != wantLoad {
			t.Fatalf("want %q, got %q", wantLoad, err)
		}
		if got := rec.events; len(got) != 0 {
			t.Fatalf("want no plugin to load, got %v", got)
		}
	})

	t.Run("patch promotes a non-group entry", func(t *testing.T) {
		base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
			ID: "app", Name: strptr("noop"),
			Plugins: []*loader.Patch{
				{Insert: []*loader.Patch{{ID: "child", Name: strptr("noop")}}},
			},
		}}}
		promote := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
			ID: "app", Group: boolptr(true),
		}}}

		tree, err := loader.Compose([]loader.Layer{base, promote})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(tree.Warnings); got != 0 {
			t.Fatalf("want the promoted entry to compose clean, got %v", tree.Warnings)
		}
		node := tree.Find("app")
		if node == nil || !node.Group {
			t.Fatalf("want the patch to promote the entry to a group, got %+v", node)
		}
		fibers, err := tree.Load(cordis.New(), newIntegrationRegistry(&loadRecorder{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(fibers); got != 1 {
			t.Fatalf("want the promoted entry's child to load, got %d fibers", got)
		}
	})
}

// TestIntegrationMixedInsertRejectedInLayerStack crosses #26 with #28: the
// offending entry sits nested inside a patch layer's group, and the very same
// stack composes once the entry is split into a pure insert plus a sibling.
func TestIntegrationMixedInsertRejectedInLayerStack(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "g", Group: boolptr(true),
		Plugins: []*loader.Patch{{ID: "web", Name: strptr("noop")}},
	}}}
	mixed := loader.Layer{Label: "overrides", Patch: true, Entries: []*loader.Patch{{
		ID: "g", Plugins: []*loader.Patch{
			{ID: "web", Insert: []*loader.Patch{{ID: "w", Name: strptr("noop")}}},
		},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, mixed})
	if err == nil {
		t.Fatal("want the mixed entry to fail composition")
	}
	want := `layer overrides: entry "web" declares both insert and other fields;` +
		` split it into two entries`
	if err.Error() != want {
		t.Fatalf("want %q, got %q", want, err)
	}
	if tree != nil {
		t.Fatalf("want no tree from a failed composition, got %+v", tree)
	}
	_, err = loader.Compose([]loader.Layer{base, mixed}, loader.Strict())
	if err == nil || err.Error() != want {
		t.Fatalf("want the same rejection under Strict, got %v", err)
	}

	split := loader.Layer{Label: "split", Entries: []*loader.Patch{{
		ID: "g", Group: boolptr(true),
		Plugins: []*loader.Patch{
			{Insert: []*loader.Patch{{ID: "w", Name: strptr("noop")}}},
			{ID: "web", Name: strptr("noop")},
		},
	}}}
	fixed := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
		ID: "web", Disabled: boolptr(true),
	}}}

	fixedTree, err := loader.Compose([]loader.Layer{split, fixed})
	if err != nil {
		t.Fatal(err)
	}
	if got := fixedTree.Size(); got != 3 {
		t.Fatalf("want 3 entries after the split, got %d", got)
	}
	fibers, err := fixedTree.Load(cordis.New(), newIntegrationRegistry(&loadRecorder{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(fibers); got != 1 {
		t.Fatalf("want only w to load (web disabled by the patch), got %d fibers", got)
	}
}

// TestIntegrationMidTreeFailureRollsBackAcrossGroup crosses #22 with #28: the
// failing entry sits inside a group whose sibling was created by a nested
// insert. The failed entry's own fiber must be disposed, the rollback must
// unwind the earlier entries across the group boundary in reverse order, and
// the entry after the failure must never load.
func TestIntegrationMidTreeFailureRollsBackAcrossGroup(t *testing.T) {
	rec := &loadRecorder{}
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{ID: "early", Name: strptr("tagged"), Config: map[string]any{"tag": "early"}},
		{
			ID: "g", Group: boolptr(true), Label: strptr("mid"),
			Plugins: []*loader.Patch{
				{Insert: []*loader.Patch{{ID: "mid", Name: strptr("tagged"),
					Config: map[string]any{"tag": "mid"}}}},
				{ID: "bad", Name: strptr("bad")},
			},
		},
		{ID: "after", Name: strptr("tagged"), Config: map[string]any{"tag": "after"}},
	}}
	tree, err := loader.Compose([]loader.Layer{base})
	if err != nil {
		t.Fatal(err)
	}

	root := cordis.New()
	before := len(root.Effects())
	fibers, err := tree.Load(root, newIntegrationRegistry(rec))
	if err == nil {
		t.Fatal("want the failing entry to fail the whole load")
	}
	want := `loader: entry "bad" (bad): boom`
	if err.Error() != want {
		t.Fatalf("want %q, got %q", want, err)
	}
	if fibers != nil {
		t.Fatalf("want no fibers from a failed load, got %d", len(fibers))
	}
	wantEvents := []string{"load:early", "load:mid", "dispose:mid", "dispose:early"}
	if !reflect.DeepEqual(rec.events, wantEvents) {
		t.Fatalf("want rollback in reverse order %v, got %v", wantEvents, rec.events)
	}
	if got := len(root.Effects()); got != before {
		t.Fatalf("want effects back at %d after rollback, got %d", before, got)
	}
	reg, ok := cordis.Get[cordis.Registry](root, "registry")
	if !ok {
		t.Fatal("the registry service is missing")
	}
	if got := reg.Plugins(); len(got) != 0 {
		t.Fatalf("want no plugin with live fibers after rollback, got %v", got)
	}
}
