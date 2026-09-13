package loader_test

import (
	"encoding/json"
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
