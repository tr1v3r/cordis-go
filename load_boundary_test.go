package cordis

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLoadRecoversValidatePanic(t *testing.T) {
	root := New()
	plugin := Define[struct{}]("panicking-validator", func(*Context, struct{}) error {
		t.Fatal("want validation to stop before plugin body, got plugin body call")
		return nil
	}).WithValidate(func(*struct{}) error {
		panic("bad config")
	})

	fiber, err := root.Load(plugin, struct{}{})
	if fiber == nil {
		t.Fatal("want a failed fiber, got nil")
	}
	if err == nil {
		t.Fatal("want a validation panic error, got nil")
	}
	if !strings.Contains(err.Error(), "panicking-validator") ||
		!strings.Contains(err.Error(), "bad config") {
		t.Fatalf("want plugin name and panic reason, got %v", err)
	}
	if fiber.State() != StateFailed {
		t.Fatalf("want failed, got %s", fiber.State())
	}
	if len(root.shared.runtimes) != 1 {
		t.Fatalf("want one registered runtime, got %d", len(root.shared.runtimes))
	}

	fiber.Dispose()
	if len(root.shared.runtimes) != 0 {
		t.Fatalf("want no runtime after disposal, got %d", len(root.shared.runtimes))
	}
}

func TestRestartRecoversValidatePanicAndCanRetry(t *testing.T) {
	root := New()
	var validations atomic.Int32
	plugin := Define[struct{}]("retry-validator", func(*Context, struct{}) error {
		return nil
	}).WithValidate(func(*struct{}) error {
		if validations.Add(1) == 2 {
			panic("retry validation failed")
		}
		return nil
	})

	fiber, err := root.Load(plugin, struct{}{})
	if err != nil {
		t.Fatalf("want initial load success, got %v", err)
	}
	if err := fiber.Restart(); err == nil {
		t.Fatal("want a validation panic error, got nil")
	} else if !strings.Contains(err.Error(), "retry-validator") ||
		!strings.Contains(err.Error(), "retry validation failed") {
		t.Fatalf("want plugin name and panic reason, got %v", err)
	}
	if fiber.State() != StateFailed {
		t.Fatalf("want failed, got %s", fiber.State())
	}
	if fiber.busy {
		t.Fatal("want busy false after recovered validation panic, got true")
	}

	if err := fiber.Restart(); err != nil {
		t.Fatalf("want successful retry, got %v", err)
	}
	if fiber.State() != StateActive {
		t.Fatalf("want active, got %s", fiber.State())
	}
	if got := validations.Load(); got != 3 {
		t.Fatalf("want 3 validations, got %d", got)
	}
}

func TestDriveReleasesBusyBeforeRepanicking(t *testing.T) {
	fiber := &Fiber{
		runtime: &runtime{},
		inject:  map[string]struct{}{"dep": {}},
		busy:    true,
	}

	reason := driveRecover(fiber)
	if reason == nil {
		t.Fatal("want unexpected transition panic to escape, got nil")
	}
	if fiber.busy {
		t.Fatal("want busy false after unexpected transition panic, got true")
	}
}

func driveRecover(fiber *Fiber) (reason any) {
	defer func() { reason = recover() }()
	fiber.drive()
	return nil
}

type nonComparableDefinition []string

func (nonComparableDefinition) PluginName() string { return "non-comparable" }
func (nonComparableDefinition) InjectKeys() []string {
	panic("InjectKeys must not run for an invalid definition")
}
func (nonComparableDefinition) ResolveConfig(any) (any, error) { return nil, nil }
func (nonComparableDefinition) Run(*Context, any) error        { return nil }

func TestLoadRejectsNonComparableDefinitionBeforeMethods(t *testing.T) {
	root := New()

	fiber, err := load(root, nonComparableDefinition{"state"}, nil, nil)
	if fiber != nil {
		t.Fatalf("want nil fiber, got %v", fiber)
	}
	if !errors.Is(err, &Error{Code: ErrInvalidPlugin}) {
		t.Fatalf("want ErrInvalidPlugin, got %v", err)
	}
	if !strings.Contains(err.Error(), "comparable pointer") {
		t.Fatalf("want comparability diagnostic, got %v", err)
	}
	if len(root.shared.runtimes) != 0 {
		t.Fatalf("want no registry residue, got %d runtimes", len(root.shared.runtimes))
	}
}

func TestLoadRejectsNilDefinition(t *testing.T) {
	root := New()

	fiber, err := load(root, nil, nil, nil)
	if fiber != nil {
		t.Fatalf("want nil fiber, got %v", fiber)
	}
	if !errors.Is(err, &Error{Code: ErrInvalidPlugin}) {
		t.Fatalf("want ErrInvalidPlugin, got %v", err)
	}
	if len(root.shared.runtimes) != 0 {
		t.Fatalf("want no registry residue, got %d runtimes", len(root.shared.runtimes))
	}
}

func TestLoadRejectsTypedNilPlugin(t *testing.T) {
	root := New()
	var plugin *Plugin[struct{}]

	fiber, err := root.Load(plugin, struct{}{})
	if fiber != nil {
		t.Fatalf("want nil fiber, got %v", fiber)
	}
	if !errors.Is(err, &Error{Code: ErrInvalidPlugin}) {
		t.Fatalf("want ErrInvalidPlugin, got %v", err)
	}
	if len(root.shared.runtimes) != 0 {
		t.Fatalf("want no registry residue, got %d runtimes", len(root.shared.runtimes))
	}
}

func TestInjectRejectsNilBody(t *testing.T) {
	root := New()

	fiber, err := Inject(root, nil, nil)
	if fiber != nil {
		t.Fatalf("want nil fiber, got %v", fiber)
	}
	if !errors.Is(err, &Error{Code: ErrInvalidPlugin}) {
		t.Fatalf("want ErrInvalidPlugin, got %v", err)
	}
	if len(root.shared.runtimes) != 0 {
		t.Fatalf("want no registry residue, got %d runtimes", len(root.shared.runtimes))
	}
}
