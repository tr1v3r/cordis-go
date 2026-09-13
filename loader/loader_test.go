package loader_test

import (
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
