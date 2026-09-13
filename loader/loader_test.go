package loader_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

type dbConfig struct {
	Path string `json:"path"`
}

func strptr(value string) *string { return &value }
func boolptr(value bool) *bool    { return &value }

func TestComposePatchReplacesWholeConfig(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID:     "db",
		Name:   strptr("db"),
		Config: map[string]any{"path": "base.db", "legacy": true},
	}}}
	profile := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
		ID:     "db",
		Config: map[string]any{"path": "profile.db"},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, profile})
	if err != nil {
		t.Fatal(err)
	}
	node := tree.Find("db")
	if node == nil {
		t.Fatal("db entry missing")
	}
	if len(node.Config) != 1 || node.Config["path"] != "profile.db" {
		t.Fatalf("patch must replace the whole config, got %v", node.Config)
	}
	if node.Name != "db" {
		t.Fatalf("omitted fields must be preserved, got name %q", node.Name)
	}
	if node.Source != "base" || !reflect.DeepEqual(node.Patched, []string{"profile"}) {
		t.Fatalf("unexpected provenance: source=%q patched=%v", node.Source, node.Patched)
	}
	if !reflect.DeepEqual(tree.Layers, []string{"base", "profile"}) {
		t.Fatalf("unexpected layers: %v", tree.Layers)
	}
}

func TestComposeUnmatchedIDWarnsAndStrictFails(t *testing.T) {
	layers := []loader.Layer{
		{Label: "patch", Patch: true, Entries: []*loader.Patch{{ID: "missing"}}},
	}

	tree, err := loader.Compose(layers)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Warnings) != 1 || !strings.Contains(tree.Warnings[0], "missing") {
		t.Fatalf("want a warning about the unmatched id, got %v", tree.Warnings)
	}

	if _, err := loader.Compose(layers, loader.Strict()); err == nil {
		t.Fatal("strict composition must fail on an unmatched id")
	}
}

func TestComposeInsertAddsEntries(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "a", Name: strptr("db"),
	}}}
	extra := loader.Layer{Label: "extra", Patch: true, Entries: []*loader.Patch{{
		Insert: []*loader.Patch{{ID: "b", Name: strptr("server")}},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, extra})
	if err != nil {
		t.Fatal(err)
	}
	if tree.Size() != 2 {
		t.Fatalf("want 2 entries, got %d", tree.Size())
	}
	if node := tree.Find("b"); node == nil || node.Source != "extra" {
		t.Fatalf("inserted entry missing or misattributed: %+v", node)
	}
}

func TestTreeLoadDecodesConfigAndGroups(t *testing.T) {
	registry := loader.NewRegistry()
	var path string
	loader.MustRegister(registry, "db",
		cordis.Define[dbConfig]("db", func(_ *cordis.Context, cfg dbConfig) error {
			path = cfg.Path
			return nil
		}))
	loader.MustRegister(registry, "noop",
		cordis.Define[struct{}]("noop", func(*cordis.Context, struct{}) error {
			return nil
		}))

	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
			{ID: "db", Name: strptr("db"), Config: map[string]any{"path": "grouped.db"}},
		}},
		{ID: "off", Name: strptr("noop"), Disabled: boolptr(true)},
	}}

	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.New()
	fibers, err := tree.Load(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(fibers) != 1 {
		t.Fatalf("disabled entries must be skipped, got %d fibers", len(fibers))
	}
	if path != "grouped.db" {
		t.Fatalf("config not decoded, got %q", path)
	}
	if fibers[0].State() != cordis.StateActive {
		t.Fatalf("want active, got %s", fibers[0].State())
	}
}

func TestTreeLoadAppliesEntryInject(t *testing.T) {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "consumer",
		cordis.Define[struct{}]("consumer", func(*cordis.Context, struct{}) error {
			return nil
		}))

	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "consumer", Name: strptr("consumer"), Inject: &[]string{"db"},
	}}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}

	root := cordis.New()
	fibers, err := tree.Load(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if fibers[0].State() != cordis.StatePending {
		t.Fatalf("entry-level inject must gate loading, got %s", fibers[0].State())
	}
	if _, err := cordis.Provide[*dbConfig](root, "db", &dbConfig{Path: "x"}); err != nil {
		t.Fatal(err)
	}
	if fibers[0].State() != cordis.StateActive {
		t.Fatalf("want active after dependency appeared, got %s", fibers[0].State())
	}
}

func TestTreeLoadRejectsUnknownPlugin(t *testing.T) {
	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{ID: "x", Name: strptr("nope")}}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Load(cordis.New(), loader.NewRegistry()); err == nil {
		t.Fatal("want an error for an unknown plugin")
	}
}

func TestDumpReportsProvenance(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "db", Name: strptr("db"), Config: map[string]any{"path": "base.db"},
	}}}
	profile := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
		ID: "db", Config: map[string]any{"path": "profile.db"},
	}}}
	tree, err := loader.Compose([]loader.Layer{base, profile})
	if err != nil {
		t.Fatal(err)
	}
	dump := tree.DumpString()
	for _, want := range []string{"# layers: base -> profile",
		"# from base; patched by profile", `config: {"path": "profile.db"}`} {
		if !strings.Contains(dump, want) {
			t.Fatalf("dump missing %q:\n%s", want, dump)
		}
	}
}

func TestParseLayerJSON(t *testing.T) {
	layer, err := loader.ParseLayer("file", []byte(`[
	  {"id": "db", "name": "db", "config": {"path": "a.db"}},
	  {"insert": [{"id": "srv", "name": "server"}]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(layer.Entries) != 2 {
		t.Fatalf("want 2 patches, got %d", len(layer.Entries))
	}
	if layer.Entries[0].Config["path"] != "a.db" {
		t.Fatalf("config not parsed: %v", layer.Entries[0].Config)
	}
	if len(layer.Entries[1].Insert) != 1 {
		t.Fatal("insert not parsed")
	}
}

func TestComposePatchesNestedEntryByID(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
			{ID: "inner", Name: strptr("db"), Config: map[string]any{"path": "a.db"}},
		},
	}}}
	profile := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
		ID: "inner", Config: map[string]any{"path": "b.db"},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, profile})
	if err != nil {
		t.Fatal(err)
	}
	node := tree.Find("inner")
	if node == nil {
		t.Fatal("nested entry must be addressable by id")
	}
	if node.Config["path"] != "b.db" {
		t.Fatalf("nested patch not applied: %v", node.Config)
	}
	if node.Source != "base" || !reflect.DeepEqual(node.Patched, []string{"profile"}) {
		t.Fatalf("unexpected provenance: %+v", node)
	}
}

func TestTreeLoadRollsBackOnError(t *testing.T) {
	registry := loader.NewRegistry()
	var events []string
	loader.MustRegister(registry, "db",
		cordis.Define[struct{}]("db", func(ctx *cordis.Context, _ struct{}) error {
			events = append(events, "load")
			ctx.OnDispose(func() { events = append(events, "unload") })
			return nil
		}))

	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{ID: "a", Name: strptr("db")},
		{ID: "b", Name: strptr("unknown")},
	}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Load(cordis.New(), registry); err == nil {
		t.Fatal("want an error")
	}
	if !reflect.DeepEqual(events, []string{"load", "unload"}) {
		t.Fatalf("partial load must be rolled back, got %v", events)
	}
}

func TestTreeLoadFailsOnFailedEntry(t *testing.T) {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "bad",
		cordis.Define[struct{}]("bad", func(*cordis.Context, struct{}) error {
			return errors.New("boom")
		}))
	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{ID: "a", Name: strptr("bad")}}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Load(cordis.New(), registry); err == nil {
		t.Fatal("a failed plugin body must fail the whole load")
	}
}

func TestGroupInjectGatesChildren(t *testing.T) {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "child",
		cordis.Define[struct{}]("child", func(*cordis.Context, struct{}) error {
			return nil
		}))
	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "grp", Group: boolptr(true), Inject: &[]string{"db"},
		Plugins: []*loader.Patch{{ID: "child", Name: strptr("child")}},
	}}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.New()
	fibers, err := tree.Load(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if fibers[0].State() != cordis.StatePending {
		t.Fatalf("group inject must gate its subtree, got %s", fibers[0].State())
	}
	if _, err := cordis.Provide[*dbConfig](root, "db", &dbConfig{Path: "x"}); err != nil {
		t.Fatal(err)
	}
	if fibers[0].State() != cordis.StateActive {
		t.Fatalf("want active after the dependency appeared, got %s", fibers[0].State())
	}
}

// TestComposeBaseDuplicateIDKeepsFirst pins the policy for the case the old
// applyBase check policed: a base layer repeating a top-level id used to fail
// composition outright, while the same document reordered composed silently.
// Both orders now warn and keep the first entry, and Strict() stays loud.
func TestComposeBaseDuplicateIDKeepsFirst(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{ID: "x", Name: strptr("db"), Label: strptr("first"),
			Config: map[string]any{"path": "first.db"}},
		{ID: "x", Name: strptr("db"), Label: strptr("second"),
			Config: map[string]any{"path": "second.db"}},
	}}
	patch := loader.Layer{Label: "p", Patch: true, Entries: []*loader.Patch{{
		ID: "x", Config: map[string]any{"path": "patched.db"},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, patch})
	if err != nil {
		t.Fatal(err)
	}
	want := `layer base: duplicate entry id "x"`
	if len(tree.Warnings) != 1 || tree.Warnings[0] != want {
		t.Fatalf("want exactly one warning %q, got %v", want, tree.Warnings)
	}
	node := tree.Find("x")
	if node == nil {
		t.Fatal("the entry that kept the id must stay addressable")
	}
	if node.Label != "first" {
		t.Fatalf("want the first entry to keep the id, got label %q", node.Label)
	}
	if node.Config["path"] != "patched.db" {
		t.Fatalf("want the patch to reach the node Find returns, got %v", node.Config)
	}
	shadow := shadowNode(tree, node)
	if shadow == nil {
		t.Fatal("want both entries in the tree, got only one")
	}
	if shadow.Config["path"] != "second.db" {
		t.Fatalf("want the shadowed entry untouched at second.db, got %v", shadow.Config)
	}
	if tree.Size() != 2 {
		t.Fatalf("want both entries counted, got %d nodes", tree.Size())
	}
	if _, err := loader.Compose([]loader.Layer{base, patch}, loader.Strict()); err == nil {
		t.Fatal("strict composition must reject a duplicate id")
	}
}

func TestPatchInsertRejectsDuplicateID(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{ID: "a", Name: strptr("db")}}}
	patch := loader.Layer{Label: "p", Patch: true, Entries: []*loader.Patch{{
		Insert: []*loader.Patch{
			{ID: "b", Name: strptr("db")},
			{ID: "b", Name: strptr("db")},
		},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, patch})
	if err != nil {
		t.Fatal(err)
	}
	if tree.Size() != 2 {
		t.Fatalf("duplicate insert must be skipped, got %d nodes", tree.Size())
	}
	if len(tree.Warnings) != 1 || !strings.Contains(tree.Warnings[0], "duplicate") {
		t.Fatalf("want a duplicate-id warning, got %v", tree.Warnings)
	}
	if _, err := loader.Compose([]loader.Layer{base, patch}, loader.Strict()); err == nil {
		t.Fatal("strict composition must reject a duplicate id")
	}
}

// TestComposeNestedDuplicateIDKeepsFirst pins the invariant that a patch and
// Find agree on which node an id names. A blindly overwritten index sent the
// patch to the last node registered while Find returned the first one in DFS
// order, and swapping the two entries turned the same document into a hard
// error.
func TestComposeNestedDuplicateIDKeepsFirst(t *testing.T) {
	topLevel := func() *loader.Patch {
		return &loader.Patch{ID: "x", Name: strptr("db"), Label: strptr("outer"),
			Config: map[string]any{"path": "outer.db"}}
	}
	group := func() *loader.Patch {
		return &loader.Patch{ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
			{ID: "x", Name: strptr("db"), Label: strptr("inner"),
				Config: map[string]any{"path": "inner.db"}},
		}}
	}

	for _, tc := range []struct {
		name       string
		entries    []*loader.Patch
		wantLabel  string
		shadowPath string
	}{
		{
			name:       "top level entry first",
			entries:    []*loader.Patch{topLevel(), group()},
			wantLabel:  "outer",
			shadowPath: "inner.db",
		},
		{
			name:       "nested entry first",
			entries:    []*loader.Patch{group(), topLevel()},
			wantLabel:  "inner",
			shadowPath: "outer.db",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := loader.Layer{Label: "base", Entries: tc.entries}
			patch := loader.Layer{Label: "p", Patch: true, Entries: []*loader.Patch{{
				ID: "x", Config: map[string]any{"path": "patched.db"},
			}}}

			tree, err := loader.Compose([]loader.Layer{base, patch})
			if err != nil {
				t.Fatal(err)
			}
			want := `layer base: duplicate entry id "x"`
			if len(tree.Warnings) != 1 || tree.Warnings[0] != want {
				t.Fatalf("want exactly one warning %q, got %v", want, tree.Warnings)
			}
			node := tree.Find("x")
			if node == nil {
				t.Fatal("the entry that kept the id must stay addressable")
			}
			if node.Label != tc.wantLabel {
				t.Fatalf("want the first entry to keep the id (label %q), got %q",
					tc.wantLabel, node.Label)
			}
			if node.Config["path"] != "patched.db" {
				t.Fatalf("want the patch to reach the node Find returns, got %v", node.Config)
			}
			shadow := shadowNode(tree, node)
			if shadow == nil {
				t.Fatal("want both entries in the tree, got only one")
			}
			if shadow.Config["path"] != tc.shadowPath {
				t.Fatalf("want the shadowed entry untouched at %q, got %v",
					tc.shadowPath, shadow.Config)
			}
			if _, err := loader.Compose([]loader.Layer{base, patch}, loader.Strict()); err == nil {
				t.Fatal("strict composition must reject a duplicate id")
			}
		})
	}
}

// shadowNode returns the other node carrying the same id as keeper, walking the
// tree in DFS order instead of trusting the index under test.
func shadowNode(tree *loader.Tree, keeper *loader.Node) *loader.Node {
	var walk func(nodes []*loader.Node) *loader.Node
	walk = func(nodes []*loader.Node) *loader.Node {
		for _, node := range nodes {
			if node.ID == keeper.ID && node != keeper {
				return node
			}
			if found := walk(node.Children); found != nil {
				return found
			}
		}
		return nil
	}
	return walk(tree.Nodes)
}
