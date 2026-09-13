package loader_test

import (
	"encoding/json"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

// defaultsConfig decodes through its own UnmarshalJSON, which fills in the
// default that a plain field decode would leave empty. The loader must reach
// this method for every config object it is handed - including the empty one,
// which carries no field the plain decoder would notice.
type defaultsConfig struct {
	Mode string `json:"mode"`
}

func (c *defaultsConfig) UnmarshalJSON(data []byte) error {
	type plain defaultsConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.Mode == "" {
		decoded.Mode = "default"
	}
	*c = defaultsConfig(decoded)
	return nil
}

// loadDefaultsEntry composes a one-entry tree whose entry carries raw as its
// config and loads it with a plugin decoding into C.
func loadDefaultsEntry[C any](t *testing.T, raw map[string]any,
	body func(config C)) {
	t.Helper()
	layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "entry", Name: strptr("defaults"), Config: raw,
	}}}
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		t.Fatal(err)
	}
	loadDefaultsTree(t, tree, body)
}

// loadDefaultsTree loads tree with a plugin that decodes into C.
func loadDefaultsTree[C any](t *testing.T, tree *loader.Tree, body func(config C)) {
	t.Helper()
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "defaults",
		cordis.Define[C]("defaults", func(_ *cordis.Context, config C) error {
			body(config)
			return nil
		}))
	if _, err := tree.Load(cordis.New(), registry); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRunsUnmarshalJSONForAnEmptyConfigObject(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{name: "empty object", raw: map[string]any{}},
		{name: "unused key", raw: map[string]any{"unused": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			loadDefaultsEntry(t, tc.raw, func(config defaultsConfig) {
				got = config.Mode
			})
			if got != "default" {
				t.Fatalf("want the target type's UnmarshalJSON to run, got mode %q", got)
			}
		})
	}
}

// TestLoadEmptyAndUnusedConfigsAgree pins the equivalence the empty-object
// short circuit used to break: "config: {}" and "config: {\"unused\": 1}" are
// both an object handed to the target type, so they must decode the same way.
func TestLoadEmptyAndUnusedConfigsAgree(t *testing.T) {
	var empty, unused string
	loadDefaultsEntry(t, map[string]any{}, func(config defaultsConfig) {
		empty = config.Mode
	})
	loadDefaultsEntry(t, map[string]any{"unused": 1}, func(config defaultsConfig) {
		unused = config.Mode
	})
	if empty != unused {
		t.Fatalf("want {} and {unused: 1} to decode alike, got %q and %q", empty, unused)
	}
}

// TestLoadAllocatesPointerConfigForAnEmptyConfigObject covers the other half of
// the short circuit: the caller asked for a pointer config, so an object - even
// an empty one - must produce an allocated value.
func TestLoadAllocatesPointerConfigForAnEmptyConfigObject(t *testing.T) {
	var got *defaultsConfig
	loadDefaultsEntry(t, map[string]any{}, func(config *defaultsConfig) {
		got = config
	})
	if got == nil {
		t.Fatal("want an allocated config for an empty object, got nil")
	}
	if got.Mode != "default" {
		t.Fatalf("want mode \"default\", got %q", got.Mode)
	}
}

// TestLoadLeavesAbsentConfigAtItsZeroValue guards the other direction: an
// absent config is not an empty object. It must stay at the zero value instead
// of being sent through the target type's UnmarshalJSON as JSON null.
func TestLoadLeavesAbsentConfigAtItsZeroValue(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		var got defaultsConfig
		loadDefaultsEntry(t, nil, func(config defaultsConfig) {
			got = config
		})
		if got.Mode != "" {
			t.Fatalf("want the zero value for an absent config, got mode %q", got.Mode)
		}
	})

	t.Run("null", func(t *testing.T) {
		layer, err := loader.ParseLayer("file", []byte(`[
		  {"id": "entry", "name": "defaults", "config": null}
		]`))
		if err != nil {
			t.Fatal(err)
		}
		tree, err := loader.Compose([]loader.Layer{layer})
		if err != nil {
			t.Fatal(err)
		}
		var got defaultsConfig
		loadDefaultsTree(t, tree, func(config defaultsConfig) {
			got = config
		})
		if got.Mode != "" {
			t.Fatalf("want the zero value for a null config, got mode %q", got.Mode)
		}
	})
}
