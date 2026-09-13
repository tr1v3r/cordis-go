package cordis_test

import (
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

// TestSharedLabelDoesNotAliasServiceName pins the by-name lookup contract: a
// scope label carries one service name, so a context that maps another name
// onto that label must neither read nor overwrite the service registered there.
func TestSharedLabelDoesNotAliasServiceName(t *testing.T) {
	root := cordis.New()
	shared := root.IsolateShared("a", "shared")
	database := &fakeDB{name: "a"}
	if _, err := cordis.Provide[*fakeDB](shared, "a", database); err != nil {
		t.Fatal(err)
	}
	// "b" is isolated onto the label of "a", so both names share one scope.
	alias := shared.IsolateShared("b", "shared")

	if got, ok := cordis.Get[*fakeDB](alias, "b"); ok {
		t.Fatalf("want no service %q, got %v", "b", got)
	}
	if got, ok := alias.Lookup("b"); ok {
		t.Fatalf("want lookup of %q to fail, got %v", "b", got)
	}
	setErr := alias.Set("b", &fakeDB{name: "b"})
	frameworkErr, ok := setErr.(*cordis.Error)
	if !ok || frameworkErr.Code != cordis.ErrServiceMissing {
		t.Fatalf("want SERVICE_MISSING from Set(%q), got %v", "b", setErr)
	}
	// One label still carries one name: providing "b" there stays a collision
	// rather than turning "b" into a second name for the same scope.
	_, err := cordis.Provide[*fakeDB](alias, "b", &fakeDB{name: "b"})
	frameworkErr, ok = err.(*cordis.Error)
	if !ok || frameworkErr.Code != cordis.ErrServiceExists {
		t.Fatalf("want SERVICE_EXISTS from Provide(%q), got %v", "b", err)
	}

	// Set("b") must not have replaced the value of the service named "a", and
	// the shared scope must still serve it under its own name.
	if got, ok := cordis.Get[*fakeDB](shared, "a"); !ok || got != database {
		t.Fatalf("want %v for service %q, got %v ok=%v", database, "a", got, ok)
	}
	if got, ok := cordis.Get[*fakeDB](alias, "a"); !ok || got != database {
		t.Fatalf("want %v for service %q, got %v ok=%v", database, "a", got, ok)
	}
}

// TestInjectIgnoresBindingOfAnotherName pins that dependency resolution is a
// by-name lookup too: a fiber requiring "b" stays pending while the scope it
// resolves in only carries a service named "a".
func TestInjectIgnoresBindingOfAnotherName(t *testing.T) {
	root := cordis.New()
	shared := root.IsolateShared("a", "shared")
	if _, err := cordis.Provide[*fakeDB](shared, "a", &fakeDB{name: "a"}); err != nil {
		t.Fatal(err)
	}
	alias := shared.IsolateShared("b", "shared")

	consumer := cordis.Define[struct{}]("consumer", func(*cordis.Context, struct{}) error {
		return nil
	}).WithInject("b")
	fiber, err := alias.Load(consumer, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if state := fiber.State(); state != cordis.StatePending {
		t.Fatalf("want the consumer to stay pending, got %s", state)
	}
}
