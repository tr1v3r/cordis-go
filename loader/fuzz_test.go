package loader_test

import (
	"testing"

	"github.com/tr1v3r/cordis-go/loader"
)

// FuzzCompose drives the pure configuration path with arbitrary bytes: both
// layer kinds are parsed, the stack is composed, walked and dumped. Plugin
// loading is left out on purpose - a fuzzed tree names plugins no registry can
// know, so Load would stop at the first entry and decode nothing.
//
// Beyond not panicking, the fuzzed input must keep two invariants: appending a
// patch layer never shrinks the tree, and every node that carries an id stays
// findable by it.
func FuzzCompose(f *testing.F) {
	for _, seed := range [][]string{
		{`[]`},
		{`[{"id":"db","name":"db","config":{"path":"a.db","n":9007199254740993,"r":1.5}}]`},
		{`[{"id":"grp","group":true,"plugins":[{"id":"inner","name":"db"}]}]`},
		{`[{"insert":[{"id":"srv","name":"db"}]}]`, `true`},
		{`[{"id":"db","name":"db"}]`, `[{"id":"db","config":{"path":"b.db"}}]`},
		{`[{"id":"dup"},{"id":"dup"}]`},
		{`[{"id":"a","label":" 外文 ","disabled":true,"inject":["db","db"]}]`},
		{`[{"id":"x","config":{"deep":{"list":[1,2,{"k":"v"}],"nil":null,"s":"he said \"hi\""}}}]`},
		{`[{}]`}, {`{}`}, {`null`}, {`[{"id":`}, {`[0]`}, {`[""]`},
	} {
		f.Add([]byte(seed[0]), len(seed) > 1 && seed[1] == "true")
	}

	f.Fuzz(func(t *testing.T, data []byte, asPatch bool) {
		base, err := loader.ParseLayer("fuzz-base", data)
		if err != nil {
			return
		}
		patch, err := loader.ParsePatchLayer("fuzz-patch", data)
		if err != nil {
			return
		}
		layers := []loader.Layer{base}
		if asPatch {
			layers = append(layers, patch)
		}
		tree, err := loader.Compose(layers)
		if err != nil {
			return
		}
		if tree.Size() < baseTreeSize(t, data) {
			t.Fatalf("want the composed tree no smaller than the base tree, got %d < %d",
				tree.Size(), baseTreeSize(t, data))
		}
		for _, id := range nodeIDs(tree.Nodes, nil) {
			if tree.Find(id) == nil {
				t.Fatalf("node %q is not findable in a tree of %d nodes", id, tree.Size())
			}
		}
		_ = tree.DumpString()
	})
}

// nodeIDs collects the ids of a node tree, empty ones included.
func nodeIDs(nodes []*loader.Node, out []string) []string {
	for _, node := range nodes {
		if node == nil {
			continue
		}
		out = append(out, node.ID)
		out = nodeIDs(node.Children, out)
	}
	return out
}

// baseTreeSize composes the base layer alone to measure what the patch stack
// must not shrink below.
func baseTreeSize(t *testing.T, data []byte) int {
	t.Helper()
	base, err := loader.ParseLayer("fuzz-base", data)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := loader.Compose([]loader.Layer{base})
	if err != nil {
		t.Fatal(err)
	}
	return tree.Size()
}
