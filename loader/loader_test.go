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

// TestComposeBaseNestedInsertMatchesPatchLayer pins layer parity: the same
// nested-insert shape must create the same entry whether it arrives in a base
// layer or in a patch layer, since the two used to disagree.
func TestComposeBaseNestedInsertMatchesPatchLayer(t *testing.T) {
	fromBase := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
			{Insert: []*loader.Patch{{ID: "c", Name: strptr("db")}}},
		},
	}}}
	baseTree, err := loader.Compose([]loader.Layer{fromBase})
	if err != nil {
		t.Fatal(err)
	}

	fromPatch := []loader.Layer{
		{Label: "base", Entries: []*loader.Patch{{ID: "grp", Group: boolptr(true)}}},
		{Label: "extra", Patch: true, Entries: []*loader.Patch{{
			ID: "grp", Plugins: []*loader.Patch{
				{Insert: []*loader.Patch{{ID: "c", Name: strptr("db")}}},
			},
		}}},
	}
	patchTree, err := loader.Compose(fromPatch)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		tree *loader.Tree
	}{
		{"base", baseTree},
		{"patch", patchTree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := tc.tree.Find("c")
			if node == nil {
				t.Fatal("want the nested insert to create the entry it declares")
			}
			if node.Name != "db" {
				t.Fatalf("want name %q, got %q", "db", node.Name)
			}
		})
	}
	if baseTree.Size() != patchTree.Size() {
		t.Fatalf("want equal tree sizes, base %d vs patch %d",
			baseTree.Size(), patchTree.Size())
	}
}

// TestComposeBaseNestedInsertIsPatchable pins the point of the expansion: an
// entry created by a nested insert in a base layer carries its declared id, so
// a later layer can patch it like any other entry - impossible for the ghost
// node the old code created.
func TestComposeBaseNestedInsertIsPatchable(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
			{Insert: []*loader.Patch{{ID: "c", Name: strptr("db"),
				Config: map[string]any{"path": "a.db"}}}},
		},
	}}}
	profile := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
		ID:     "c",
		Config: map[string]any{"path": "b.db"},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, profile})
	if err != nil {
		t.Fatal(err)
	}
	node := tree.Find("c")
	if node == nil {
		t.Fatal("want the inserted entry to be addressable by id")
	}
	if node.Config["path"] != "b.db" {
		t.Fatalf("want the later patch to replace the config, got %v", node.Config)
	}
	if !reflect.DeepEqual(node.Patched, []string{"profile"}) {
		t.Fatalf("want patched-by [profile], got %v", node.Patched)
	}
}

// TestComposeBaseNestedInsertExpandsAllTargets pins that one nested child may
// insert several entries, and that they become siblings next to the named
// children, in declaration order.
func TestComposeBaseNestedInsertExpandsAllTargets(t *testing.T) {
	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
			{ID: "n", Name: strptr("db")},
			{Insert: []*loader.Patch{
				{ID: "c1", Name: strptr("db")},
				{ID: "c2", Name: strptr("server")},
			}},
		},
	}}}

	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	grp := tree.Find("grp")
	if grp == nil {
		t.Fatal("want the group to exist")
	}
	var got []string
	for _, child := range grp.Children {
		got = append(got, child.ID)
	}
	want := []string{"n", "c1", "c2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want children %v, got %v", want, got)
	}
	if tree.Size() != 4 {
		t.Fatalf("want 4 entries, got %d", tree.Size())
	}
}

// TestComposeBaseNestedInsertExpandsRecursively pins that the expansion applies
// at any depth: an inserted entry's own children may insert too, and those
// entries must be created as well instead of turning into anonymous nodes.
func TestComposeBaseNestedInsertExpandsRecursively(t *testing.T) {
	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
			{Insert: []*loader.Patch{{
				ID: "mid", Name: strptr("db"), Group: boolptr(true),
				Plugins: []*loader.Patch{
					{Insert: []*loader.Patch{{ID: "leaf", Name: strptr("db")}}},
				},
			}}},
		},
	}}}

	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	if tree.Find("leaf") == nil {
		t.Fatal("want the insert inside the inserted entry to expand too")
	}
	mid := tree.Find("mid")
	if mid == nil || len(mid.Children) != 1 || mid.Children[0].ID != "leaf" {
		t.Fatalf("want leaf as the only child of mid, got %+v", mid)
	}
	if tree.Size() != 3 {
		t.Fatalf("want 3 entries, got %d", tree.Size())
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

// TestComposeBaseNestedInsertExpands pins base-layer parity with patch layers:
// a nested entry carrying "insert" creates its entries instead of a ghost node
// with no id and no name, which no patch could address and Load could not
// resolve to a plugin.
func TestComposeBaseNestedInsertExpands(t *testing.T) {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "p",
		cordis.Define[struct{}]("p", func(*cordis.Context, struct{}) error { return nil }))

	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{{
			Insert: []*loader.Patch{{ID: "c", Name: strptr("p")}},
		}},
	}}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	node := tree.Find("c")
	if node == nil {
		t.Fatal("want a nested insert to create the entry it declares")
	}
	if node.Name != "p" {
		t.Fatalf("want the inserted entry to be named %q, got %q", "p", node.Name)
	}
	if tree.Size() != 2 {
		t.Fatalf("want 2 entries, got %d", tree.Size())
	}
	if dump := tree.DumpString(); strings.Contains(dump, `- name: ""`) {
		t.Fatalf("want no anonymous entry in the dump:\n%s", dump)
	}
	fibers, err := tree.Load(cordis.New(), registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(fibers) != 1 {
		t.Fatalf("want 1 loaded entry, got %d", len(fibers))
	}
}

// TestComposeRejectsEntryWithoutIDOrName pins the other half: an entry that can
// be created without an id must carry a name, at any depth and in any layer.
func TestComposeRejectsEntryWithoutIDOrName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch bool
		entry *loader.Patch
		want  string
	}{
		{
			name:  "top level",
			entry: &loader.Patch{Name: strptr("")},
			want:  "layer base: entry requires id or name",
		},
		{
			name: "nested",
			entry: &loader.Patch{ID: "grp", Group: boolptr(true),
				Plugins: []*loader.Patch{{Name: strptr("")}}},
			want: "layer base: entry plugins[0] requires id or name",
		},
		{
			name: "nested insert",
			entry: &loader.Patch{ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{{
				Insert: []*loader.Patch{{Name: strptr("")}},
			}}},
			want: "layer base: entry plugins[0] insert[0] requires id or name",
		},
		{
			name:  "inserted",
			patch: true,
			entry: &loader.Patch{Insert: []*loader.Patch{{Name: strptr("")}}},
			want:  "layer p: inserted entry requires id or name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label := "base"
			if tc.patch {
				label = "p"
			}
			layer := loader.Layer{Label: label, Patch: tc.patch, Entries: []*loader.Patch{tc.entry}}
			_, err := loader.Compose([]loader.Layer{layer})
			if err == nil {
				t.Fatal("want an error: a created entry needs an id or a name")
			}
			if err.Error() != tc.want {
				t.Fatalf("want %q, got %q", tc.want, err)
			}
		})
	}
}
