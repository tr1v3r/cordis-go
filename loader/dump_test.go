package loader_test

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

// TestDumpSizeFindSkipNilNodes pins the degenerate hand-built tree: Load skips
// nil entries, so the read-only walkers have to skip them too instead of
// panicking on a tree the composer would never produce.
func TestDumpSizeFindSkipNilNodes(t *testing.T) {
	tree := &loader.Tree{Nodes: []*loader.Node{
		nil,
		{ID: "grp", Group: true, Children: []*loader.Node{nil, {ID: "inner", Name: "db"}}},
		nil,
	}}
	if got := tree.Size(); got != 2 {
		t.Fatalf("want 2 entries, got %d", got)
	}
	if node := tree.Find("grp"); node == nil {
		t.Fatal("want the group node, got nil")
	}
	if node := tree.Find("inner"); node == nil {
		t.Fatal("want the nested node, got nil")
	}
	if node := tree.Find("missing"); node != nil {
		t.Fatalf("want nil for a missing id, got %+v", node)
	}
	dump := tree.DumpString()
	if !strings.Contains(dump, `- id: "grp"`) || !strings.Contains(dump, `- id: "inner"`) {
		t.Fatalf("want both nodes in the dump, got:\n%s", dump)
	}
	if strings.Contains(dump, "plugins:\n\n") {
		t.Fatalf("want no empty plugins block, got:\n%s", dump)
	}
}

// TestDumpOfOnlyNilNodesIsEmpty covers the same skip policy on its own: a tree
// holding nothing but nil entries is empty, not a panic and not a stray header.
func TestDumpOfOnlyNilNodesIsEmpty(t *testing.T) {
	tree := &loader.Tree{Nodes: []*loader.Node{nil, nil}}
	if got := tree.Size(); got != 0 {
		t.Fatalf("want 0 entries, got %d", got)
	}
	if node := tree.Find("x"); node != nil {
		t.Fatalf("want nil, got %+v", node)
	}
	if dump := tree.DumpString(); !strings.Contains(dump, "# (empty)") {
		t.Fatalf("want the empty marker, got:\n%s", dump)
	}
}

// TestLoadSkipsNilNodesInAHandBuiltTree pins the policy the nil-tolerant
// walkers mirror: Load already skipped nil entries, so a tree a host built by
// hand loads its real entries and ignores the nils instead of failing.
func TestLoadSkipsNilNodesInAHandBuiltTree(t *testing.T) {
	registry := loader.NewRegistry()
	var loaded []string
	loader.MustRegister(registry, "db",
		cordis.Define[struct{}]("db", func(_ *cordis.Context, _ struct{}) error {
			loaded = append(loaded, "db")
			return nil
		}))
	tree := &loader.Tree{Nodes: []*loader.Node{
		nil,
		{ID: "real", Name: "db"},
		nil,
	}}

	fibers, err := tree.Load(cordis.New(), registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(fibers) != 1 {
		t.Fatalf("want 1 fiber for the real entry, got %d", len(fibers))
	}
	if !reflect.DeepEqual(loaded, []string{"db"}) {
		t.Fatalf("want the real entry loaded, got %v", loaded)
	}
}

// TestDumpMarksUnencodableConfigValues pins the dump of a value json.Marshal
// refuses. A hand-built config can hold one, and the dump must say so instead
// of printing a Go-formatted value that reads like JSON but is not.
func TestDumpMarksUnencodableConfigValues(t *testing.T) {
	tree, err := loader.Compose([]loader.Layer{{Label: "base", Entries: []*loader.Patch{{
		ID:   "weird",
		Name: strptr("weird"),
		Config: map[string]any{
			"channel": make(chan int),
			"ratio":   math.Inf(1),
		},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	dump := tree.DumpString()
	for _, want := range []string{"<unencodable chan int:", "<unencodable float64:"} {
		if !strings.Contains(dump, want) {
			t.Fatalf("want %q in the dump, got:\n%s", want, dump)
		}
	}
	if strings.Contains(dump, `": 0x`) || strings.Contains(dump, `": +Inf`) {
		t.Fatalf("want no bare Go-formatted value in the dump, got:\n%s", dump)
	}
}

// TestLoadLayerDefaultsTheLabelBeforeReading pins the read-error path: an empty
// label must fall back to the path before the file is opened, or the error
// names no layer at all and reads as `layer : open ...`.
func TestLoadLayerDefaultsTheLabelBeforeReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	for _, load := range []struct {
		name string
		fn   func(label, path string) (loader.Layer, error)
	}{
		{name: "base", fn: loader.LoadLayer},
		{name: "patch", fn: loader.LoadPatchLayer},
	} {
		t.Run(load.name, func(t *testing.T) {
			_, err := load.fn("", path)
			if err == nil {
				t.Fatal("want an error for a missing file")
			}
			if strings.Contains(err.Error(), "layer :") {
				t.Fatalf("want no empty layer label, got %q", err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("want the path in %q", err)
			}
		})
	}
}

// TestLoadLayerDefaultsTheLabelOnSuccess completes the label fallback: when the
// read succeeds the layer keeps the path as its label, so provenance names the
// file exactly the way the error path does.
func TestLoadLayerDefaultsTheLabelOnSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layer.json")
	if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}

	layer, err := loader.LoadLayer("", path)
	if err != nil {
		t.Fatal(err)
	}
	if layer.Label != path {
		t.Fatalf("want the path as the layer label, got %q", layer.Label)
	}

	patched, err := loader.LoadPatchLayer("", path)
	if err != nil {
		t.Fatal(err)
	}
	if patched.Label != path || !patched.Patch {
		t.Fatalf("want the path as the label and a patch layer, got %q patch=%v",
			patched.Label, patched.Patch)
	}
}
