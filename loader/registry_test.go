package loader_test

import (
	"reflect"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

// noopPlugin is a plugin that does nothing, for tests that only care about the
// registry's bookkeeping.
func noopPlugin(name string) *cordis.Plugin[struct{}] {
	return cordis.Define[struct{}](name, func(*cordis.Context, struct{}) error { return nil })
}

func TestRegistryHasAndNames(t *testing.T) {
	registry := loader.NewRegistry()
	if names := registry.Names(); len(names) != 0 {
		t.Fatalf("want no names in a fresh registry, got %v", names)
	}
	if registry.Has("db") {
		t.Fatal("want Has false before registration")
	}

	loader.MustRegister(registry, "server", noopPlugin("server"))
	loader.MustRegister(registry, "db", noopPlugin("db"))

	if !registry.Has("db") || registry.Has("absent") {
		t.Fatalf("want Has true only for registered names, got db=%v absent=%v",
			registry.Has("db"), registry.Has("absent"))
	}
	if names := registry.Names(); !reflect.DeepEqual(names, []string{"db", "server"}) {
		t.Fatalf("want sorted names [db server], got %v", names)
	}
}

func TestRegisterRejectsInvalidArguments(t *testing.T) {
	if err := loader.Register(nil, "db", noopPlugin("db")); err == nil {
		t.Fatal("want an error for a nil registry")
	}
	if err := loader.Register(loader.NewRegistry(), "", noopPlugin("db")); err == nil {
		t.Fatal("want an error for an empty name")
	}

	var nilPlugin *cordis.Plugin[struct{}]
	if err := loader.Register(loader.NewRegistry(), "db", nilPlugin); err == nil {
		t.Fatal("want an error for a nil plugin")
	}

	registry := loader.NewRegistry()
	if err := loader.Register(registry, "db", noopPlugin("db")); err != nil {
		t.Fatal(err)
	}
	err := loader.Register(registry, "db", noopPlugin("db"))
	if err == nil {
		t.Fatal("want an error for a duplicate name")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("want a duplicate-registration error, got %v", err)
	}
	if !registry.Has("db") {
		t.Fatal("a rejected registration must leave the first one in place")
	}
}

func TestMustRegisterPanicsOnDuplicateName(t *testing.T) {
	registry := loader.NewRegistry()
	loader.MustRegister(registry, "db", noopPlugin("db"))

	defer func() {
		if recover() == nil {
			t.Fatal("want MustRegister to panic when the name is taken")
		}
	}()
	// MustRegister is meant for package init blocks, where a conflicting name is
	// a programming error rather than something a caller can handle.
	loader.MustRegister(registry, "db", noopPlugin("db"))
}
