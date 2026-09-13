package loader_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/tr1v3r/cordis-go/loader"
)

func TestPatchLookupTracksCurrentDFSOrderAfterInsertion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layers func() []loader.Layer
		patch  string
	}{
		{
			name: "same layer",
			layers: func() []loader.Layer {
				return []loader.Layer{
					groupABase(),
					{
						Label: "profile", Patch: true,
						Entries: []*loader.Patch{
							insertRootDuplicate(),
							insertNestedDuplicate(),
							{ID: "dup", Config: map[string]any{"value": "patched"}},
						},
					},
				}
			},
			patch: "profile",
		},
		{
			name: "later layer",
			layers: func() []loader.Layer {
				return []loader.Layer{
					groupABase(),
					{Label: "root", Patch: true, Entries: []*loader.Patch{
						insertRootDuplicate(),
					}},
					{Label: "structure", Patch: true, Entries: []*loader.Patch{
						insertNestedDuplicate(),
					}},
					{Label: "final", Patch: true, Entries: []*loader.Patch{
						{ID: "dup", Config: map[string]any{"value": "patched"}},
					}},
				}
			},
			patch: "final",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layers := tc.layers()
			tree, err := loader.Compose(layers)
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}

			found := tree.Find("dup")
			if found == nil {
				t.Fatal("want the nested duplicate, got nil")
			}
			if found.Label != "nested" {
				t.Fatalf("want the DFS-first nested node, got label %q", found.Label)
			}
			if got := found.Config["value"]; got != "patched" {
				t.Fatalf("want nested config patched, got %v", got)
			}
			if len(found.Patched) != 1 || found.Patched[0] != tc.patch {
				t.Fatalf("want provenance patched by %q, got %v", tc.patch, found.Patched)
			}

			rootDuplicate := tree.Nodes[1]
			if rootDuplicate.Label != "root" {
				t.Fatalf("want the creation-first duplicate at the root, got %q",
					rootDuplicate.Label)
			}
			if got := rootDuplicate.Config["value"]; got != "root" {
				t.Fatalf("want root duplicate untouched, got %v", got)
			}
			if len(rootDuplicate.Patched) != 0 {
				t.Fatalf("want root duplicate unpatched, got provenance %v", rootDuplicate.Patched)
			}

			if len(tree.Warnings) != 1 || !strings.Contains(tree.Warnings[0],
				`duplicate entry id "dup"`) {
				t.Fatalf("want one duplicate warning, got %v", tree.Warnings)
			}
			if _, err := loader.Compose(tc.layers(), loader.Strict()); err == nil ||
				!strings.Contains(err.Error(), `duplicate entry id "dup"`) {
				t.Fatalf("want Strict to reject the duplicate, got %v", err)
			}
		})
	}
}

func groupABase() loader.Layer {
	return loader.Layer{Label: "base", Entries: []*loader.Patch{{
		ID: "a", Group: boolptr(true),
	}}}
}

func insertRootDuplicate() *loader.Patch {
	return &loader.Patch{Insert: []*loader.Patch{{
		ID: "dup", Name: strptr("svc"), Label: strptr("root"),
		Config: map[string]any{"value": "root"},
	}}}
}

func insertNestedDuplicate() *loader.Patch {
	return &loader.Patch{
		ID: "a",
		Plugins: []*loader.Patch{{Insert: []*loader.Patch{{
			ID: "mid", Group: boolptr(true), Plugins: []*loader.Patch{{
				ID: "dup", Name: strptr("svc"), Label: strptr("nested"),
				Config: map[string]any{"value": "nested"},
			}},
		}}}},
	}
}

func TestDumpDistinguishesEmptyConfigFromAbsentConfig(t *testing.T) {
	tree, err := loader.Compose([]loader.Layer{{Label: "base", Entries: []*loader.Patch{
		{ID: "empty", Name: strptr("svc"), Config: map[string]any{}},
		{ID: "absent", Name: strptr("svc")},
	}}})
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}

	dump := tree.DumpString()
	if !strings.Contains(dump, "  config: {}\n") {
		t.Fatalf("want literal empty config object in dump, got:\n%s", dump)
	}
	if got := strings.Count(dump, "  config:"); got != 1 {
		t.Fatalf("want exactly one config field, got %d in:\n%s", got, dump)
	}
	absent := tree.Find("absent")
	if absent == nil || absent.Config != nil {
		t.Fatalf("want absent config to remain nil, got %+v", absent)
	}
	empty := tree.Find("empty")
	if empty == nil || empty.Config == nil || len(empty.Config) != 0 {
		t.Fatalf("want empty config to remain a non-nil empty map, got %+v", empty)
	}
}

func TestZeroValueRegistrySupportsConcurrentRegistration(t *testing.T) {
	var registry loader.Registry
	const registrations = 16
	plugins := make([]string, registrations)
	for i := range plugins {
		plugins[i] = fmt.Sprintf("plugin-%02d", i)
	}

	start := make(chan struct{})
	errs := make(chan error, registrations)
	var writers sync.WaitGroup
	for _, name := range plugins {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			errs <- loader.Register(&registry, name, noopPlugin(name))
		}()
	}
	close(start)
	for range registrations {
		registry.Has("plugin-00")
		_ = registry.Names()
	}
	writers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("want no error, got %v", err)
		}
	}

	if got := len(registry.Names()); got != registrations {
		t.Fatalf("want %d registered names, got %d", registrations, got)
	}
	for _, name := range plugins {
		if got := registry.Has(name); !got {
			t.Fatalf("want Has(%q) true, got %v", name, got)
		}
	}
	if err := loader.Register(&registry, plugins[0], noopPlugin("duplicate")); err == nil ||
		!strings.Contains(err.Error(), "already registered") {
		t.Fatalf("want duplicate registration error, got %v", err)
	}
}
