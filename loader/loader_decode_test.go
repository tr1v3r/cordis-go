package loader_test

import (
	"encoding/json"
	"errors"
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

// validatingConfig rejects an empty mode, so its UnmarshalJSON doubles as
// validation: it only runs when the decode path really hands the config over.
type validatingConfig struct {
	Mode string `json:"mode"`
}

func (c *validatingConfig) UnmarshalJSON(data []byte) error {
	type plain validatingConfig
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.Mode == "" {
		return errors.New("mode must not be empty")
	}
	*c = validatingConfig(decoded)
	return nil
}

// TestLoadFailsWhenUnmarshalJSONRejectsAnEmptyConfigObject pins the loud side
// of the empty-object decode: an empty object is a real decode, so a target
// type that rejects it must fail the load - the old short circuit made the
// rejection silently disappear. An absent config never reaches the type, so it
// still loads.
func TestLoadFailsWhenUnmarshalJSONRejectsAnEmptyConfigObject(t *testing.T) {
	t.Run("empty object", func(t *testing.T) {
		layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
			ID: "entry", Name: strptr("defaults"), Config: map[string]any{},
		}}}
		tree, err := loader.Compose([]loader.Layer{layer})
		if err != nil {
			t.Fatal(err)
		}
		registry := loader.NewRegistry()
		loader.MustRegister(registry, "defaults",
			cordis.Define[validatingConfig]("defaults",
				func(_ *cordis.Context, _ validatingConfig) error { return nil }))
		if _, err := tree.Load(cordis.New(), registry); err == nil {
			t.Fatal("want an error: the target type must see the empty object")
		}
	})

	t.Run("absent", func(t *testing.T) {
		layer := loader.Layer{Label: "base", Entries: []*loader.Patch{{
			ID: "entry", Name: strptr("defaults"),
		}}}
		tree, err := loader.Compose([]loader.Layer{layer})
		if err != nil {
			t.Fatal(err)
		}
		registry := loader.NewRegistry()
		loader.MustRegister(registry, "defaults",
			cordis.Define[validatingConfig]("defaults",
				func(_ *cordis.Context, _ validatingConfig) error { return nil }))
		if _, err := tree.Load(cordis.New(), registry); err != nil {
			t.Fatalf("want an absent config to load, got %v", err)
		}
	})
}

// TestLoadPointerConfigStaysNilWithoutAConfigObject completes the pointer
// matrix: a pointer config is allocated for an empty object but stays nil when
// the entry declares no config at all, because nil and empty are different
// declarations.
func TestLoadPointerConfigStaysNilWithoutAConfigObject(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		var got *defaultsConfig
		loadDefaultsEntry(t, nil, func(config *defaultsConfig) {
			got = config
		})
		if got != nil {
			t.Fatalf("want a nil config for an absent config, got %+v", got)
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
		var got *defaultsConfig
		loadDefaultsTree(t, tree, func(config *defaultsConfig) {
			got = config
		})
		if got != nil {
			t.Fatalf("want a nil config for a null config, got %+v", got)
		}
	})
}
