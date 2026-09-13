package cordis_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

// duplicateProvide returns the framework error produced by providing a service
// name twice in the same scope.
func duplicateProvide(t *testing.T) error {
	t.Helper()
	root := cordis.New()
	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "a"}); err != nil {
		t.Fatalf("first provide: %v", err)
	}
	_, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "b"})
	if err == nil {
		t.Fatal("want an error for a duplicate service")
	}
	return err
}

func TestErrorRendersCodeAndMessage(t *testing.T) {
	err := duplicateProvide(t)

	var codeErr *cordis.Error
	if !errors.As(err, &codeErr) || codeErr.Code != cordis.ErrServiceExists {
		t.Fatalf("want SERVICE_EXISTS, got %v", err)
	}
	rendered := err.Error()
	if !strings.HasPrefix(rendered, "SERVICE_EXISTS: ") {
		t.Fatalf("want the code and message rendered, got %q", rendered)
	}
	if !strings.Contains(rendered, "already provided") {
		t.Fatalf("want the message rendered, got %q", rendered)
	}

	// A framework error without a message renders as its bare code.
	if got := (&cordis.Error{Code: cordis.ErrInvalidPlugin}).Error(); got != "INVALID_PLUGIN" {
		t.Fatalf("want the bare code, got %q", got)
	}
}

func TestErrorMatchesByCode(t *testing.T) {
	err := duplicateProvide(t)

	if !errors.Is(err, &cordis.Error{Code: cordis.ErrServiceExists}) {
		t.Fatalf("want errors.Is to match the code, got %v", err)
	}
	if errors.Is(err, &cordis.Error{Code: cordis.ErrServiceMissing}) {
		t.Fatal("a target with a different code must not match")
	}
	if !errors.Is(fmt.Errorf("boot: %w", err), &cordis.Error{Code: cordis.ErrServiceExists}) {
		t.Fatal("want errors.Is to match through a wrapping error")
	}
	if errors.Is(err, errors.New("SERVICE_EXISTS")) {
		t.Fatal("a target that is not a framework error must not match")
	}
}
