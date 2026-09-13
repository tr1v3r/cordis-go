package loader_test

import (
	"encoding/json"
	"errors"
	"fmt"
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

// TestComposeKeepsPureInsert is the regression guard for the rejection above:
// an entry carrying nothing but "insert" keeps creating entries, in base layers
// as well as in patch layers.
func TestComposeKeepsPureInsert(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		Insert: []*loader.Patch{{ID: "a", Name: strptr("db")}},
	}}}
	patch := loader.Layer{Label: "p", Patch: true, Entries: []*loader.Patch{{
		Insert: []*loader.Patch{{ID: "b", Name: strptr("db")}},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, patch})
	if err != nil {
		t.Fatal(err)
	}
	if tree.Size() != 2 {
		t.Fatalf("want 2 inserted entries, got %d", tree.Size())
	}
	for _, id := range []string{"a", "b"} {
		if tree.Find(id) == nil {
			t.Fatalf("want inserted entry %q to exist", id)
		}
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

// TestComposeNonGroupChildrenCheckedAfterAllLayers pins why the check runs on
// the finished tree instead of at entry creation: a later layer can still turn
// the entry into a group, and it can also be the layer that adds the children.
func TestComposeNonGroupChildrenCheckedAfterAllLayers(t *testing.T) {
	t.Run("cured by a group patch", func(t *testing.T) {
		base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
			ID: "parent", Name: strptr("db"),
			Plugins: []*loader.Patch{{ID: "child", Name: strptr("db")}},
		}}}
		patch := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
			ID: "parent", Group: boolptr(true),
		}}}

		tree, err := loader.Compose([]loader.Layer{base, patch}, loader.Strict())
		if err != nil {
			t.Fatalf("want the later group patch to cure the entry, got %v", err)
		}
		parent := tree.Find("parent")
		if parent == nil || !parent.Group {
			t.Fatalf("want the patch to make the entry a group, got %+v", parent)
		}
		if len(tree.Warnings) != 0 {
			t.Fatalf("want no warnings, got %v", tree.Warnings)
		}
	})

	t.Run("children added by a later layer", func(t *testing.T) {
		base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
			ID: "parent", Name: strptr("db"),
		}}}
		patch := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
			ID: "parent",
			Plugins: []*loader.Patch{{
				Insert: []*loader.Patch{{ID: "child", Name: strptr("db")}},
			}},
		}}}

		tree, err := loader.Compose([]loader.Layer{base, patch})
		if err != nil {
			t.Fatal(err)
		}
		want := `entry "parent" has plugins but is not a group: they will not be loaded`
		if len(tree.Warnings) != 1 || tree.Warnings[0] != want {
			t.Fatalf("want exactly one warning %q, got %v", want, tree.Warnings)
		}
		if _, err := loader.Compose([]loader.Layer{base, patch}, loader.Strict()); err == nil {
			t.Fatal("strict composition must reject plugins on a non-group entry")
		}
	})

	t.Run("nested below a group", func(t *testing.T) {
		layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
			ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{{
				ID: "mid", Name: strptr("db"),
				Plugins: []*loader.Patch{{ID: "leaf", Name: strptr("db")}},
			}},
		}}}

		tree, err := loader.Compose([]loader.Layer{layer})
		if err != nil {
			t.Fatal(err)
		}
		want := `entry "mid" has plugins but is not a group: they will not be loaded`
		if len(tree.Warnings) != 1 || tree.Warnings[0] != want {
			t.Fatalf("want the warning to name the nested entry, got %v", tree.Warnings)
		}
	})
}

// TestComposeNonGroupChildrenWarnAndStrictFails pins the "why is my config not
// applied" half of the contract: Load only recurses into groups, so children of
// a non-group entry never load, and Compose must say so.
func TestComposeNonGroupChildrenWarnAndStrictFails(t *testing.T) {
	const want = `entry "parent" has plugins but is not a group: they will not be loaded`

	for _, tc := range []struct {
		name  string
		entry *loader.Patch
	}{
		{
			name: "id",
			entry: &loader.Patch{ID: "parent", Name: strptr("db"),
				Plugins: []*loader.Patch{{ID: "child", Name: strptr("db")}}},
		},
		{
			name: "name",
			entry: &loader.Patch{Name: strptr("parent"),
				Plugins: []*loader.Patch{{ID: "child", Name: strptr("db")}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layers := []loader.Layer{{Label: "base", Entries: []*loader.Patch{tc.entry}}}
			tree, err := loader.Compose(layers)
			if err != nil {
				t.Fatal(err)
			}
			if len(tree.Warnings) != 1 || tree.Warnings[0] != want {
				t.Fatalf("want exactly one warning %q, got %v", want, tree.Warnings)
			}
			_, err = loader.Compose(layers, loader.Strict())
			if err == nil {
				t.Fatal("want an error: plugins on a non-group entry are never loaded")
			}
			if !strings.Contains(err.Error(), "layer base: "+want) {
				t.Fatalf("want the strict error to carry the layer and the reason, got %q", err)
			}
		})
	}
}

// TestComposeRejectsMixedInsert pins the loud rejection of an entry that
// declares "insert" and something else at once. Such an entry loses one half of
// what it says - the base path drops the entry itself, the patch path drops the
// patch and its warnings - so it has to be a configuration error instead.
func TestComposeRejectsMixedInsert(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch bool
		entry *loader.Patch
		want  string
	}{
		{
			name: "base entry",
			entry: &loader.Patch{ID: "a", Name: strptr("db"),
				Insert: []*loader.Patch{{ID: "b", Name: strptr("db")}}},
			want: `layer base: entry "a" declares both insert and other fields;` +
				` split it into two entries`,
		},
		{
			name:  "patch entry",
			patch: true,
			entry: &loader.Patch{ID: "a", Config: map[string]any{"path": "x"},
				Insert: []*loader.Patch{{ID: "b", Name: strptr("db")}}},
			want: `layer p: entry "a" declares both insert and other fields;` +
				` split it into two entries`,
		},
		{
			name:  "unnamed entry",
			entry: &loader.Patch{Name: strptr(""), Insert: []*loader.Patch{{ID: "b"}}},
			want: `layer base: entry "<unnamed>" declares both insert and other fields;` +
				` split it into two entries`,
		},
		{
			name: "insert with inject",
			entry: &loader.Patch{Inject: &[]string{"db"},
				Insert: []*loader.Patch{{ID: "b", Name: strptr("db")}}},
			want: `layer base: entry "<unnamed>" declares both insert and other fields;` +
				` split it into two entries`,
		},
		{
			name: "nested entry",
			entry: &loader.Patch{ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
				{ID: "b", Insert: []*loader.Patch{{ID: "c"}}},
			}},
			want: `layer base: entry "b" declares both insert and other fields;` +
				` split it into two entries`,
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
				t.Fatal("want an error: an entry cannot insert and declare fields at once")
			}
			if err.Error() != tc.want {
				t.Fatalf("want %q, got %q", tc.want, err)
			}
		})
	}
}

// TestComposeRejectsMixedInsertInLaterLayer pins that validation covers every
// layer in order: a mixed insert in a later layer fails the whole composition
// and yields no tree, even though the earlier layers were valid on their own.
func TestComposeRejectsMixedInsertInLaterLayer(t *testing.T) {
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{ID: "a", Name: strptr("db")}}}
	bad := loader.Layer{Label: "profile", Patch: true, Entries: []*loader.Patch{{
		ID:     "a",
		Label:  strptr("prod"),
		Insert: []*loader.Patch{{ID: "b", Name: strptr("server")}},
	}}}

	tree, err := loader.Compose([]loader.Layer{base, bad})
	if err == nil {
		t.Fatal("want an error: a mixed insert in a later layer must fail the composition")
	}
	if tree != nil {
		t.Fatalf("want no tree from a failed composition, got %d layers", len(tree.Layers))
	}
	want := `layer profile: entry "a" declares both insert and other fields;` +
		` split it into two entries`
	if err.Error() != want {
		t.Fatalf("want %q, got %q", want, err)
	}
}

// TestComposeRejectsMixedInsertInsideInsertTarget covers the recursion into an
// entry's "insert" list: an inserted entry is an entry too, so it must not
// declare its own fields and insert at once either.
func TestComposeRejectsMixedInsertInsideInsertTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch bool
		entry *loader.Patch
	}{
		{
			name: "base",
			entry: &loader.Patch{Insert: []*loader.Patch{{
				ID: "b", Insert: []*loader.Patch{{ID: "c"}},
			}}},
		},
		{
			name:  "patch",
			patch: true,
			entry: &loader.Patch{Insert: []*loader.Patch{{
				ID:     "b",
				Config: map[string]any{"path": "x"},
				Insert: []*loader.Patch{{ID: "c"}},
			}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer := loader.Layer{
				Label: "base", Patch: tc.patch, Entries: []*loader.Patch{tc.entry},
			}
			_, err := loader.Compose([]loader.Layer{layer})
			if err == nil {
				t.Fatal("want an error: an inserted entry cannot insert and declare fields too")
			}
			want := `layer base: entry "b" declares both insert and other fields;` +
				` split it into two entries`
			if err.Error() != want {
				t.Fatalf("want %q, got %q", want, err)
			}
		})
	}
}

// TestComposeRejectsMixedInsertPerDeclaringField pins that every field of a
// normal entry conflicts with "insert": the composer cannot honour both halves,
// so no single field may sneak past the rejection.
func TestComposeRejectsMixedInsertPerDeclaringField(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entry     *loader.Patch
		wantLabel string
	}{
		{
			name:      "id",
			entry:     &loader.Patch{ID: "a", Insert: []*loader.Patch{{ID: "x"}}},
			wantLabel: "a",
		},
		{
			name:      "name",
			entry:     &loader.Patch{Name: strptr("db"), Insert: []*loader.Patch{{ID: "x"}}},
			wantLabel: "db",
		},
		{
			name:      "label",
			entry:     &loader.Patch{Label: strptr("prod"), Insert: []*loader.Patch{{ID: "x"}}},
			wantLabel: "<unnamed>",
		},
		{
			name:      "disabled",
			entry:     &loader.Patch{Disabled: boolptr(true), Insert: []*loader.Patch{{ID: "x"}}},
			wantLabel: "<unnamed>",
		},
		{
			name:      "group",
			entry:     &loader.Patch{Group: boolptr(true), Insert: []*loader.Patch{{ID: "x"}}},
			wantLabel: "<unnamed>",
		},
		{
			name:      "inject",
			entry:     &loader.Patch{Inject: &[]string{"db"}, Insert: []*loader.Patch{{ID: "x"}}},
			wantLabel: "<unnamed>",
		},
		{
			name: "config",
			entry: &loader.Patch{Config: map[string]any{"path": "x"},
				Insert: []*loader.Patch{{ID: "x"}}},
			wantLabel: "<unnamed>",
		},
		{
			name: "plugins",
			entry: &loader.Patch{Plugins: []*loader.Patch{{ID: "child"}},
				Insert: []*loader.Patch{{ID: "x"}}},
			wantLabel: "<unnamed>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layer := loader.Layer{Label: "base", Entries: []*loader.Patch{tc.entry}}
			_, err := loader.Compose([]loader.Layer{layer})
			if err == nil {
				t.Fatalf("want an error: field %s plus insert must be rejected", tc.name)
			}
			want := fmt.Sprintf("layer base: entry %q declares both insert and other fields; "+
				"split it into two entries", tc.wantLabel)
			if err.Error() != want {
				t.Fatalf("want %q, got %q", want, err)
			}
		})
	}
}

// TestTreeLoadAfterFailureLeavesAUsableContext pins the recovery half of
// all-or-nothing: a host that fixes the configuration and loads again on the
// same context must not inherit leftovers from the failed attempt.
func TestTreeLoadAfterFailureLeavesAUsableContext(t *testing.T) {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "ok",
		cordis.Define[struct{}]("ok", func(*cordis.Context, struct{}) error { return nil }))
	loader.MustRegister(registry, "bad",
		cordis.Define[struct{}]("bad", func(*cordis.Context, struct{}) error {
			return errors.New("boom")
		}))

	failing := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{ID: "a", Name: strptr("bad")},
	}}
	tree, err := loader.Compose([]loader.Layer{failing})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.New()
	if _, err := tree.Load(root, registry); err == nil {
		t.Fatal("want an error")
	}

	fixed := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{ID: "a", Name: strptr("ok")},
	}}
	tree, err = loader.Compose([]loader.Layer{fixed})
	if err != nil {
		t.Fatal(err)
	}
	fibers, err := tree.Load(root, registry)
	if err != nil {
		t.Fatalf("want the context usable after a failed load, got %v", err)
	}
	if len(fibers) != 1 {
		t.Fatalf("want one fiber after the reload, got %d", len(fibers))
	}
	if fibers[0].State() != cordis.StateActive {
		t.Fatalf("want the reloaded fiber active, got %s", fibers[0].State())
	}
	reg, ok := cordis.Get[cordis.Registry](root, "registry")
	if !ok {
		t.Fatal("the registry service is missing")
	}
	if got := reg.Plugins(); !reflect.DeepEqual(got, []string{"ok"}) {
		t.Fatalf("want only the reloaded plugin live, got %v", got)
	}
}

// TestTreeLoadDisposesTheFailedEntry pins all-or-nothing: the entry that failed
// owns a fiber like any other, so the rollback has to dispose it too instead of
// leaving its effect slot and its runtime behind.
func TestTreeLoadDisposesTheFailedEntry(t *testing.T) {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "ok",
		cordis.Define[struct{}]("ok", func(*cordis.Context, struct{}) error { return nil }))
	loader.MustRegister(registry, "bad",
		cordis.Define[struct{}]("bad", func(*cordis.Context, struct{}) error {
			return errors.New("boom")
		}))

	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{ID: "a", Name: strptr("ok")},
		{ID: "b", Name: strptr("bad")},
	}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}

	root := cordis.New()
	before := len(root.Effects())
	if _, err := tree.Load(root, registry); err == nil {
		t.Fatal("want an error")
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

// TestTreeLoadFailedEntryInsideGroupRollsBack pins all-or-nothing across the
// group boundary: the failure happens outside the group while the group has
// already loaded a fiber, so the rollback must reach through the recursion and
// the error must name the entry that actually failed.
func TestTreeLoadFailedEntryInsideGroupRollsBack(t *testing.T) {
	boom := errors.New("boom")
	registry := loader.NewRegistry()
	var events []string
	loader.MustRegister(registry, "ok",
		cordis.Define[struct{}]("ok", func(ctx *cordis.Context, _ struct{}) error {
			events = append(events, "load ok")
			ctx.OnDispose(func() { events = append(events, "dispose ok") })
			return nil
		}))
	loader.MustRegister(registry, "bad",
		cordis.Define[struct{}]("bad", func(*cordis.Context, struct{}) error { return boom }))

	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{
		{ID: "grp", Group: boolptr(true), Plugins: []*loader.Patch{
			{ID: "inner", Name: strptr("ok")},
		}},
		{ID: "outer", Name: strptr("bad")},
	}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}

	root := cordis.New()
	before := len(root.Effects())
	_, err = tree.Load(root, registry)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("want the plugin's own error, got %v", err)
	}
	if want := `entry "outer" (bad)`; !strings.Contains(err.Error(), want) {
		t.Fatalf("want the error to name the failed entry %q, got %q", want, err)
	}
	if !reflect.DeepEqual(events, []string{"load ok", "dispose ok"}) {
		t.Fatalf("want the group's fiber rolled back too, got %v", events)
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

// TestTreeLoadRejectsNonGroupChildren pins the loud half: loading a tree whose
// entry has children without being a group must fail instead of loading the
// parent and dropping the children.
func TestTreeLoadRejectsNonGroupChildren(t *testing.T) {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "db",
		cordis.Define[struct{}]("db", func(*cordis.Context, struct{}) error { return nil }))

	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "parent", Name: strptr("db"),
		Plugins: []*loader.Patch{{ID: "child", Name: strptr("db")}},
	}}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tree.Load(cordis.New(), registry)
	if err == nil {
		t.Fatal("want an error instead of loading the parent and dropping its children")
	}
	if !strings.Contains(err.Error(), "group: true") {
		t.Fatalf("want the error to point at group: true, got %q", err)
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

// TestLoadKeepsNegativeLargeJSONIntegersExact covers the sign side of the
// float64 trap, through the patch-layer entry point: -9007199254740993 rounds
// in a float64 just as its positive twin does, and the composed config must
// hold the exact literal as a json.Number for hosts reading the maps directly.
func TestLoadKeepsNegativeLargeJSONIntegersExact(t *testing.T) {
	const want = int64(-9007199254740993)
	base := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "neg", Name: strptr("numbers"),
	}}}
	patch, err := loader.ParsePatchLayer("file", []byte(`[
	  {"id": "neg", "config": {"count": -9007199254740993}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := loader.Compose([]loader.Layer{base, patch})
	if err != nil {
		t.Fatal(err)
	}

	stored, ok := tree.Find("neg").Config["count"].(json.Number)
	if !ok {
		t.Fatalf("want a json.Number in the composed config, got %T",
			tree.Find("neg").Config["count"])
	}
	if stored.String() != "-9007199254740993" {
		t.Fatalf("want the exact literal -9007199254740993, got %s", stored)
	}

	var got numbersConfig
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "numbers",
		cordis.Define[numbersConfig]("numbers", func(_ *cordis.Context, cfg numbersConfig) error {
			got = cfg
			return nil
		}))
	if _, err := tree.Load(cordis.New(), registry); err != nil {
		t.Fatal(err)
	}
	if got.Count != want {
		t.Fatalf("want count %d, got %d", want, got.Count)
	}
}

// TestLoadDecodesFractionalLiteralsByTargetType pins what exact decoding means
// for literals that are not integers: a float64 field keeps taking them, while
// an int64 field now refuses the literal instead of accepting a value the file
// never wrote - the documented behavior change that came with exactness.
func TestLoadDecodesFractionalLiteralsByTargetType(t *testing.T) {
	layer, err := loader.ParseLayer("file", []byte(`[
	  {"id": "frac", "name": "floats", "config": {"ratio": 1.0, "scaled": 2e2}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}

	type floatsConfig struct {
		Ratio  float64 `json:"ratio"`
		Scaled float64 `json:"scaled"`
	}
	var got floatsConfig
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "floats",
		cordis.Define[floatsConfig]("floats", func(_ *cordis.Context, cfg floatsConfig) error {
			got = cfg
			return nil
		}))
	if _, err := tree.Load(cordis.New(), registry); err != nil {
		t.Fatal(err)
	}
	if got.Ratio != 1.0 || got.Scaled != 200 {
		t.Fatalf("want ratio 1 and scaled 200, got %+v", got)
	}

	loader.MustRegister(registry, "numbers",
		cordis.Define[numbersConfig]("numbers", func(_ *cordis.Context, _ numbersConfig) error {
			return nil
		}))
	fractional, err := loader.ParseLayer("file", []byte(`[
	  {"id": "int", "name": "numbers", "config": {"count": 1.0}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	fractionalTree, err := loader.Compose([]loader.Layer{fractional})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fractionalTree.Load(cordis.New(), registry); err == nil {
		t.Fatal("want an error: a fractional literal must not decode into an int64 field")
	}
}

type numbersConfig struct {
	Count  int64            `json:"count"`
	List   []int64          `json:"list"`
	Limits map[string]int64 `json:"limits"`
}

// TestLoadKeepsLargeJSONIntegers pins the layer parse path against float64: a
// JSON integer beyond 2^53 must reach the plugin config exactly as written,
// and the dump must print that value instead of the rounded one.
func TestLoadKeepsLargeJSONIntegers(t *testing.T) {
	const want = int64(9007199254740993)
	layer, err := loader.ParseLayer("file", []byte(`[
	  {"id": "big", "name": "numbers", "config": {
	    "count": 9007199254740993,
	    "list": [9007199254740993],
	    "limits": {"max": 9007199254740993}
	  }}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}

	var got numbersConfig
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "numbers",
		cordis.Define[numbersConfig]("numbers", func(_ *cordis.Context, cfg numbersConfig) error {
			got = cfg
			return nil
		}))
	if _, err := tree.Load(cordis.New(), registry); err != nil {
		t.Fatal(err)
	}
	if got.Count != want {
		t.Fatalf("want count %d, got %d", want, got.Count)
	}
	if len(got.List) != 1 || got.List[0] != want {
		t.Fatalf("want list [%d], got %v", want, got.List)
	}
	if got.Limits["max"] != want {
		t.Fatalf("want max %d, got %d", want, got.Limits["max"])
	}

	dump := tree.DumpString()
	if !strings.Contains(dump, "9007199254740993") {
		t.Fatalf("want the dump to keep 9007199254740993, got:\n%s", dump)
	}
	if strings.Contains(dump, "9007199254740992") {
		t.Fatalf("want no float64-rounded value in the dump, got:\n%s", dump)
	}
}

// TestLoadKeepsOrdinaryJSONScalars guards the number handling from the other
// side: strings, booleans, floats and null must keep decoding as before.
func TestLoadKeepsOrdinaryJSONScalars(t *testing.T) {
	type scalarsConfig struct {
		Label   string  `json:"label"`
		Enabled bool    `json:"enabled"`
		Ratio   float64 `json:"ratio"`
		Missing *string `json:"missing"`
	}
	layer, err := loader.ParseLayer("file", []byte(`[
	  {"id": "s", "name": "scalars", "config": {
	    "label": "db", "enabled": true, "ratio": 1.5, "missing": null
	  }}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}

	var got scalarsConfig
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "scalars",
		cordis.Define[scalarsConfig]("scalars", func(_ *cordis.Context, cfg scalarsConfig) error {
			got = cfg
			return nil
		}))
	if _, err := tree.Load(cordis.New(), registry); err != nil {
		t.Fatal(err)
	}
	if got.Label != "db" || !got.Enabled || got.Ratio != 1.5 || got.Missing != nil {
		t.Fatalf("want label db, enabled true, ratio 1.5, missing nil, got %+v", got)
	}
}

// TestParseLayerRejectsTrailingData keeps the strictness of the plain
// json.Unmarshal the number handling replaced: a layer file is exactly one
// array, and a second value or trailing junk must not be read as a prefix.
func TestParseLayerRejectsTrailingData(t *testing.T) {
	for _, data := range []string{"[] []", "[] junk", "[]}"} {
		if _, err := loader.ParseLayer("file", []byte(data)); err == nil {
			t.Fatalf("want an error for %q, got none", data)
		}
	}
	for _, data := range []string{"[]", "  []\n"} {
		if _, err := loader.ParseLayer("file", []byte(data)); err != nil {
			t.Fatalf("want no error for %q, got %v", data, err)
		}
	}
}
