package cordis_test

import (
	"errors"
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

// TestSharedLabelSetAndReadsStayNameChecked completes the read/write matrix
// around one shared label: the name guard must reject only the colliding name,
// so MustGet panics on it while Set of the owner name keeps swapping the value
// in place, and a third untouched name neither reads nor writes through the
// busy label.
func TestSharedLabelSetAndReadsStayNameChecked(t *testing.T) {
	root := cordis.New()
	shared := root.IsolateShared("a", "shared")
	database := &fakeDB{name: "a"}
	if _, err := cordis.Provide[*fakeDB](shared, "a", database); err != nil {
		t.Fatal(err)
	}
	alias := shared.IsolateShared("b", "shared")

	// MustGet is the third read path: it reports the colliding name as a
	// typed SERVICE_MISSING panic instead of returning the service of "a".
	func() {
		defer func() {
			reason := recover()
			err, ok := reason.(*cordis.Error)
			if !ok || err.Code != cordis.ErrServiceMissing {
				t.Fatalf("want SERVICE_MISSING panic from MustGet(%q), got %v", "b", reason)
			}
		}()
		cordis.MustGet[*fakeDB](alias, "b")
		t.Fatal("want MustGet to panic on the colliding name")
	}()

	// Set of the name the label carries must still work and swap in place.
	replacement := &fakeDB{name: "a2"}
	if err := shared.Set("a", replacement); err != nil {
		t.Fatalf("want Set of the owner name to succeed, got %v", err)
	}
	if got, ok := cordis.Get[*fakeDB](alias, "a"); !ok || got != replacement {
		t.Fatalf("want %v for %q after Set, got %v ok=%v", replacement, "a", got, ok)
	}

	// A third name resolves in its own default scope, untouched by the label.
	if _, ok := alias.Lookup("c"); ok {
		t.Fatalf("want lookup of the untouched name %q to fail, got the service", "c")
	}
	third := &fakeDB{name: "c"}
	if _, err := cordis.Provide[*fakeDB](root, "c", third); err != nil {
		t.Fatal(err)
	}
	if err := root.Set("c", &fakeDB{name: "c2"}); err != nil {
		t.Fatalf("want Set of the untouched name to succeed, got %v", err)
	}
	if got, ok := cordis.Get[*fakeDB](root, "c"); !ok || got == third {
		t.Fatalf("want the replaced service for %q, got %v ok=%v", "c", got, ok)
	}

	// None of the operations above leaked across names or scopes.
	if _, ok := cordis.Get[*fakeDB](alias, "b"); ok {
		t.Fatalf("want no service %q after the other operations, got one", "b")
	}
	if _, ok := cordis.Get[*fakeDB](root, "a"); ok {
		t.Fatal("want the shared-label service to stay invisible to the root scope, got it")
	}
}

// TestInjectResolvesOwnerNameOnSharedLabel pins the positive side of by-name
// injection: from a context that isolates another name onto the label, a fiber
// requiring the owner name still loads against the service that owns it.
func TestInjectResolvesOwnerNameOnSharedLabel(t *testing.T) {
	root := cordis.New()
	shared := root.IsolateShared("a", "shared")
	database := &fakeDB{name: "a"}
	if _, err := cordis.Provide[*fakeDB](shared, "a", database); err != nil {
		t.Fatal(err)
	}
	alias := shared.IsolateShared("b", "shared")

	var got *fakeDB
	consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		service, ok := cordis.Get[*fakeDB](ctx, "a")
		if !ok {
			return errors.New("want the injected service to resolve inside the consumer")
		}
		got = service
		return nil
	}).WithInject("a")
	fiber, err := alias.Load(consumer, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if state := fiber.State(); state != cordis.StateActive {
		t.Fatalf("want the consumer injecting the owner name to go active, got %s", state)
	}
	if got != database {
		t.Fatalf("want %v, got %v", database, got)
	}
}
