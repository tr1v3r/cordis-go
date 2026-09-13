package loader_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

type benchmarkConfig struct {
	Value int `json:"value"`
}

// Sinks keep the measured values observable, so the compiler cannot delete the
// work being benchmarked.
var (
	benchmarkLayerSink  loader.Layer
	benchmarkTreeSink   *loader.Tree
	benchmarkNodeSink   *loader.Node
	benchmarkFibersSink []*cordis.Fiber
	benchmarkSizeSink   int
	benchmarkBoolSink   bool
	benchmarkNamesSink  []string
	benchmarkStringSink string
)

// --- Parsing ---------------------------------------------------------------

func BenchmarkParseLayer(b *testing.B) {
	for _, entries := range []int{1, 10, 100} {
		b.Run(strconv.Itoa(entries)+"Entries", func(b *testing.B) {
			data := benchmarkLayerJSON(entries, 0)
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()

			var layer loader.Layer
			var err error
			for b.Loop() {
				layer, err = loader.ParseLayer("benchmark", data)
				if err != nil {
					b.Fatal(err)
				}
			}
			benchmarkLayerSink = layer
			if len(layer.Entries) != entries {
				b.Fatalf("want %d entries, got %d", entries, len(layer.Entries))
			}
		})
	}
}

func BenchmarkParsePatchLayer(b *testing.B) {
	for _, entries := range []int{10, 100} {
		b.Run(strconv.Itoa(entries)+"Entries", func(b *testing.B) {
			data := benchmarkPatchJSON(entries, 0)
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()

			var layer loader.Layer
			var err error
			for b.Loop() {
				layer, err = loader.ParsePatchLayer("benchmark", data)
				if err != nil {
					b.Fatal(err)
				}
			}
			benchmarkLayerSink = layer
			if !layer.Patch || len(layer.Entries) != entries {
				b.Fatalf("want a %d-entry patch layer, got %+v", entries, layer)
			}
		})
	}
}

// BenchmarkLoadLayer splits the file path into its two halves, so the syscall
// cost and the parse cost can be told apart instead of being read as one number.
func BenchmarkLoadLayer(b *testing.B) {
	data := benchmarkLayerJSON(100, 0)
	path := filepath.Join(b.TempDir(), "layer.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		b.Fatal(err)
	}

	b.Run("ReadFileOnly", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			if _, err := os.ReadFile(path); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("ParseOnly", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			if _, err := loader.ParseLayer("benchmark", data); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Combined", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		b.ResetTimer()

		var layer loader.Layer
		var err error
		for b.Loop() {
			layer, err = loader.LoadLayer("benchmark", path)
			if err != nil {
				b.Fatal(err)
			}
		}
		benchmarkLayerSink = layer
		if len(layer.Entries) != 100 {
			b.Fatalf("want 100 entries, got %d", len(layer.Entries))
		}
	})
}

// --- Composition -----------------------------------------------------------

func BenchmarkCompose(b *testing.B) {
	b.Run("Base/10Entries", func(b *testing.B) {
		layer := mustParseLayer(b, benchmarkLayerJSON(10, 0))
		b.ReportAllocs()
		b.ResetTimer()
		composeInLoop(b, []loader.Layer{layer}, 10)
	})

	b.Run("Base/100Entries", func(b *testing.B) {
		layer := mustParseLayer(b, benchmarkLayerJSON(100, 0))
		b.ReportAllocs()
		b.ResetTimer()
		composeInLoop(b, []loader.Layer{layer}, 100)
	})

	b.Run("Patched/100Entries", func(b *testing.B) {
		base := mustParseLayer(b, benchmarkLayerJSON(100, 0))
		patch := mustParsePatchLayer(b, benchmarkPatchJSON(100, 100))
		b.ReportAllocs()
		b.ResetTimer()
		composeInLoop(b, []loader.Layer{base, patch}, 100)
	})

	b.Run("ThreeLayers/100Entries", func(b *testing.B) {
		base := mustParseLayer(b, benchmarkLayerJSON(100, 0))
		first := mustParsePatchLayer(b, benchmarkPatchJSON(100, 100))
		second := mustParsePatchLayer(b, benchmarkPatchJSON(100, 200))
		b.ReportAllocs()
		b.ResetTimer()
		composeInLoop(b, []loader.Layer{base, first, second}, 100)
	})

	b.Run("NestedGroups/100Entries", func(b *testing.B) {
		const (
			groups   = 4
			perGroup = 25
		)
		layer := mustParseLayer(b, benchmarkGroupJSON(groups, perGroup))
		b.ReportAllocs()
		b.ResetTimer()
		// Size counts the group entries themselves, so each group of 25
		// contributes 26 nodes.
		composeInLoop(b, []loader.Layer{layer}, groups*(perGroup+1))
	})

	b.Run("Insert/100Entries", func(b *testing.B) {
		base := mustParseLayer(b, benchmarkLayerJSON(50, 0))
		insert := mustParsePatchLayer(b, benchmarkInsertJSON(50))
		b.ReportAllocs()
		b.ResetTimer()
		composeInLoop(b, []loader.Layer{base, insert}, 100)
	})

	b.Run("Strict/100Entries", func(b *testing.B) {
		base := mustParseLayer(b, benchmarkLayerJSON(100, 0))
		patch := mustParsePatchLayer(b, benchmarkPatchJSON(100, 100))
		layers := []loader.Layer{base, patch}
		b.ReportAllocs()
		b.ResetTimer()

		var tree *loader.Tree
		var err error
		for b.Loop() {
			tree, err = loader.Compose(layers, loader.Strict())
			if err != nil {
				b.Fatal(err)
			}
		}
		benchmarkTreeSink = tree
		if tree.Size() != 100 {
			b.Fatalf("want 100 nodes, got %d", tree.Size())
		}
	})
}

func composeInLoop(b *testing.B, layers []loader.Layer, want int) {
	var tree *loader.Tree
	var err error
	for b.Loop() {
		tree, err = loader.Compose(layers)
		if err != nil {
			b.Fatal(err)
		}
	}
	benchmarkTreeSink = tree
	if tree.Size() != want {
		b.Fatalf("want %d nodes, got %d", want, tree.Size())
	}
}

// --- Tree inspection -------------------------------------------------------

func BenchmarkTreeLookup(b *testing.B) {
	tree := mustCompose(b, benchmarkLayerJSON(100, 0))

	b.Run("Find/Hit", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		var node *loader.Node
		for b.Loop() {
			node = tree.Find("plugin-50")
		}
		benchmarkNodeSink = node
		if node == nil {
			b.Fatal("want the composed node, got nil")
		}
	})

	b.Run("Find/Miss", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		var node *loader.Node
		for b.Loop() {
			node = tree.Find("missing")
		}
		benchmarkNodeSink = node
		if node != nil {
			b.Fatalf("want nil, got %+v", node)
		}
	})

	b.Run("Size", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		var size int
		for b.Loop() {
			size = tree.Size()
		}
		benchmarkSizeSink = size
		if size != 100 {
			b.Fatalf("want 100, got %d", size)
		}
	})
}

func BenchmarkTreeDump(b *testing.B) {
	for _, entries := range []int{10, 100} {
		b.Run(strconv.Itoa(entries)+"Entries", func(b *testing.B) {
			base := mustParseLayer(b, benchmarkLayerJSON(entries, 0))
			patch := mustParsePatchLayer(b, benchmarkPatchJSON(entries, entries))
			tree, err := loader.Compose([]loader.Layer{base, patch})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				if err := tree.Dump(io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkTreeDumpString(b *testing.B) {
	tree := mustCompose(b, benchmarkLayerJSON(100, 0))
	b.ReportAllocs()
	b.ResetTimer()

	var dump string
	for b.Loop() {
		dump = tree.DumpString()
	}
	benchmarkStringSink = dump
	if !strings.Contains(dump, `- id: "plugin-50"`) {
		b.Fatal("want the composed entries in the dump")
	}
}

// --- Loading ---------------------------------------------------------------

func BenchmarkTreeLoad(b *testing.B) {
	for _, entries := range []int{1, 10, 100} {
		b.Run(strconv.Itoa(entries)+"Plugins", func(b *testing.B) {
			base := mustParseLayer(b, benchmarkLayerJSON(entries, 0))
			tree, err := loader.Compose([]loader.Layer{base})
			if err != nil {
				b.Fatal(err)
			}
			registry, loads := benchmarkRegistry()
			b.ReportAllocs()
			b.ResetTimer()

			var fibers []*cordis.Fiber
			for b.Loop() {
				root := cordis.New()
				fibers, err = tree.Load(root, registry)
				if err != nil {
					b.Fatal(err)
				}
				root.Fiber().Dispose()
			}
			benchmarkFibersSink = fibers
			if want := b.N * entries; loads() != want {
				b.Fatalf("want %d plugin loads, got %d", want, loads())
			}
		})
	}
}

// BenchmarkTreeLoadGroups measures the group path, where every group forks a
// child context for its subtree instead of loading a plugin itself.
func BenchmarkTreeLoadGroups(b *testing.B) {
	const (
		groups   = 4
		perGroup = 25
	)
	layer := mustParseLayer(b, benchmarkGroupJSON(groups, perGroup))
	tree, err := loader.Compose([]loader.Layer{layer})
	if err != nil {
		b.Fatal(err)
	}
	registry, loads := benchmarkRegistry()
	b.ReportAllocs()
	b.ResetTimer()

	var fibers []*cordis.Fiber
	for b.Loop() {
		root := cordis.New()
		fibers, err = tree.Load(root, registry)
		if err != nil {
			b.Fatal(err)
		}
		root.Fiber().Dispose()
	}
	benchmarkFibersSink = fibers
	if want := b.N * groups * perGroup; loads() != want {
		b.Fatalf("want %d plugin loads, got %d", want, loads())
	}
}

// --- Registry --------------------------------------------------------------

func BenchmarkRegistryRegister(b *testing.B) {
	plugin := cordis.Define[benchmarkConfig]("noop",
		func(*cordis.Context, benchmarkConfig) error { return nil })
	b.ReportAllocs()
	for b.Loop() {
		registry := loader.NewRegistry()
		if err := loader.Register(registry, "noop", plugin); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRegistryLookup(b *testing.B) {
	plugin := cordis.Define[benchmarkConfig]("noop",
		func(*cordis.Context, benchmarkConfig) error { return nil })
	registry := loader.NewRegistry()
	if err := loader.Register(registry, "noop", plugin); err != nil {
		b.Fatal(err)
	}

	b.Run("Has/Hit", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		var ok bool
		for b.Loop() {
			ok = registry.Has("noop")
		}
		benchmarkBoolSink = ok
		if !ok {
			b.Fatal("want the registered plugin")
		}
	})

	b.Run("Has/Miss", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		var ok bool
		for b.Loop() {
			ok = registry.Has("missing")
		}
		benchmarkBoolSink = ok
		if ok {
			b.Fatal("want no plugin for a missing name")
		}
	})
}

func BenchmarkRegistryNames(b *testing.B) {
	registry := loader.NewRegistry()
	for index := range 100 {
		plugin := cordis.Define[benchmarkConfig]("noop",
			func(*cordis.Context, benchmarkConfig) error { return nil })
		if err := loader.Register(registry, "plugin-"+strconv.Itoa(index), plugin); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()

	var names []string
	for b.Loop() {
		names = registry.Names()
	}
	benchmarkNamesSink = names
	if len(names) != 100 || names[0] != "plugin-0" {
		b.Fatalf("want 100 sorted names, got %d starting at %q",
			len(names), names[0])
	}
}

// --- Fixtures --------------------------------------------------------------

// benchmarkRegistry registers one plugin name whose body counts its loads, and
// returns the number of loads seen so far.
func benchmarkRegistry() (*loader.Registry, func() int) {
	registry := loader.NewRegistry()
	loads := 0
	plugin := cordis.Define[benchmarkConfig]("noop",
		func(*cordis.Context, benchmarkConfig) error {
			loads++
			return nil
		})
	if err := loader.Register(registry, "noop", plugin); err != nil {
		panic(err)
	}
	return registry, func() int { return loads }
}

func mustParseLayer(b *testing.B, data []byte) loader.Layer {
	b.Helper()
	layer, err := loader.ParseLayer("base", data)
	if err != nil {
		b.Fatal(err)
	}
	return layer
}

func mustParsePatchLayer(b *testing.B, data []byte) loader.Layer {
	b.Helper()
	layer, err := loader.ParsePatchLayer("profile", data)
	if err != nil {
		b.Fatal(err)
	}
	return layer
}

func mustCompose(b *testing.B, data []byte) *loader.Tree {
	b.Helper()
	tree, err := loader.Compose([]loader.Layer{mustParseLayer(b, data)})
	if err != nil {
		b.Fatal(err)
	}
	return tree
}

// benchmarkLayerJSON renders entries base plugin entries, offsetting the config
// value so a patched layer can prove which value won.
func benchmarkLayerJSON(entries, offset int) []byte {
	var output strings.Builder
	output.WriteByte('[')
	for index := range entries {
		if index > 0 {
			output.WriteByte(',')
		}
		fmt.Fprintf(&output,
			`{"id":"plugin-%d","name":"noop","config":{"value":%d}}`,
			index, index+offset)
	}
	output.WriteByte(']')
	return []byte(output.String())
}

func benchmarkPatchJSON(entries, offset int) []byte {
	var output strings.Builder
	output.WriteByte('[')
	for index := range entries {
		if index > 0 {
			output.WriteByte(',')
		}
		fmt.Fprintf(&output, `{"id":"plugin-%d","config":{"value":%d}}`,
			index, index+offset)
	}
	output.WriteByte(']')
	return []byte(output.String())
}

// benchmarkInsertJSON renders a patch layer that only inserts new entries.
func benchmarkInsertJSON(entries int) []byte {
	var output strings.Builder
	output.WriteString(`[{"insert":[`)
	for index := range entries {
		if index > 0 {
			output.WriteByte(',')
		}
		fmt.Fprintf(&output,
			`{"id":"inserted-%d","name":"noop","config":{"value":%d}}`,
			index, index)
	}
	output.WriteString(`]}]`)
	return []byte(output.String())
}

// benchmarkGroupJSON renders groups, each holding perGroup plugin entries.
func benchmarkGroupJSON(groups, perGroup int) []byte {
	var output strings.Builder
	output.WriteByte('[')
	for group := range groups {
		if group > 0 {
			output.WriteByte(',')
		}
		fmt.Fprintf(&output, `{"id":"group-%d","group":true,"plugins":[`, group)
		for index := range perGroup {
			if index > 0 {
				output.WriteByte(',')
			}
			fmt.Fprintf(&output,
				`{"id":"plugin-%d-%d","name":"noop","config":{"value":%d}}`,
				group, index, index)
		}
		output.WriteString(`]}`)
	}
	output.WriteByte(']')
	return []byte(output.String())
}
