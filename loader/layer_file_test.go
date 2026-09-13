package loader_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

// writeLayerFile writes body into the test's temp dir and returns its path.
func writeLayerFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write layer file: %v", err)
	}
	return path
}

func TestLoadLayerReadsBaseFile(t *testing.T) {
	path := writeLayerFile(t, "base.json", `[
	  {"id": "db", "name": "db", "config": {"path": "a.db"}},
	  {"insert": [{"id": "srv", "name": "server"}]}
	]`)

	layer, err := loader.LoadLayer("base", path)
	if err != nil {
		t.Fatal(err)
	}
	if layer.Label != "base" || layer.Patch {
		t.Fatalf("want a base layer labelled base, got %+v", layer)
	}
	if len(layer.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(layer.Entries))
	}
	if got := layer.Entries[0].Config["path"]; got != "a.db" {
		t.Fatalf("want config path a.db, got %v", got)
	}
	if len(layer.Entries[1].Insert) != 1 {
		t.Fatalf("want the insert list parsed, got %+v", layer.Entries[1])
	}
}

func TestLoadPatchLayerMarksLayerAsPatch(t *testing.T) {
	path := writeLayerFile(t, "profile.json", `[{"id": "db", "config": {"path": "b.db"}}]`)

	layer, err := loader.LoadPatchLayer("profile", path)
	if err != nil {
		t.Fatal(err)
	}
	if layer.Label != "profile" || !layer.Patch {
		t.Fatalf("want a patch layer labelled profile, got %+v", layer)
	}
	if len(layer.Entries) != 1 || layer.Entries[0].ID != "db" {
		t.Fatalf("want the db entry parsed, got %+v", layer.Entries)
	}
	if layer.Entries[0].Name != nil {
		t.Fatalf("an omitted field must stay absent, got name %v", *layer.Entries[0].Name)
	}
}

func TestLoadLayerDefaultsLabelToPath(t *testing.T) {
	path := writeLayerFile(t, "base.json", `[]`)

	layer, err := loader.LoadLayer("", path)
	if err != nil {
		t.Fatal(err)
	}
	if layer.Label != path {
		t.Fatalf("want the path as label, got %q", layer.Label)
	}
	if len(layer.Entries) != 0 {
		t.Fatalf("want an empty layer for [], got %d entries", len(layer.Entries))
	}
}

func TestLoadLayerErrorsNameTheLabel(t *testing.T) {
	_, err := loader.LoadLayer("base", filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist for a missing file, got %v", err)
	}
	if !strings.Contains(err.Error(), "base") {
		t.Fatalf("want the layer label in the error, got %v", err)
	}

	broken := writeLayerFile(t, "broken.json", `[{"id":`)
	_, err = loader.LoadPatchLayer("profile", broken)
	if err == nil {
		t.Fatal("want an error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "profile") {
		t.Fatalf("want the layer label in the error, got %v", err)
	}
}

func TestLoadLayerRejectsNonArrayJSON(t *testing.T) {
	// A layer file is a JSON array of entries; an object is what a plugin config
	// looks like, not a layer.
	path := writeLayerFile(t, "object.json", `{"id": "db"}`)

	layer, err := loader.LoadLayer("base", path)
	if err == nil {
		t.Fatal("want an error for a JSON object layer")
	}
	if !reflect.DeepEqual(layer, loader.Layer{}) {
		t.Fatalf("want a zero layer on error, got %+v", layer)
	}
	if !strings.Contains(err.Error(), "base") {
		t.Fatalf("want the layer label in the error, got %v", err)
	}
}

func TestLayerFilesComposeAndLoad(t *testing.T) {
	var path string
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "db",
		cordis.Define[dbConfig]("db", func(_ *cordis.Context, cfg dbConfig) error {
			path = cfg.Path
			return nil
		}))

	base, err := loader.LoadLayer("base", writeLayerFile(t, "base.json",
		`[{"id": "db", "name": "db", "config": {"path": "base.db"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := loader.LoadPatchLayer("profile", writeLayerFile(t, "profile.json",
		`[{"id": "db", "config": {"path": "profile.db"}}]`))
	if err != nil {
		t.Fatal(err)
	}

	tree, err := loader.Compose([]loader.Layer{base, profile})
	if err != nil {
		t.Fatal(err)
	}
	node := tree.Find("db")
	if node == nil || node.Source != "base" {
		t.Fatalf("unexpected node after composing layer files: %+v", node)
	}
	if !reflect.DeepEqual(node.Patched, []string{"profile"}) {
		t.Fatalf("want the patch layer recorded, got %v", node.Patched)
	}

	root := cordis.New()
	defer root.Fiber().Dispose()
	fibers, err := tree.Load(root, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(fibers) != 1 || fibers[0].State() != cordis.StateActive {
		t.Fatalf("want one active fiber, got %d", len(fibers))
	}
	if path != "profile.db" {
		t.Fatalf("want the patch layer's config to win, got %q", path)
	}
}
