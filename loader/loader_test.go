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
