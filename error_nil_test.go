package cordis_test

import (
	"errors"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

func TestErrorIsIgnoresNilErrors(t *testing.T) {
	root := cordis.New()
	if _, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "a"}); err != nil {
		t.Fatalf("first provide: %v", err)
	}
	_, err := cordis.Provide[*fakeDB](root, "db", &fakeDB{name: "b"})
	if err == nil {
		t.Fatal("want an error for a duplicate service")
	}

	// errors.Is does not recover, so a panic raised here reaches the caller's
	// error handling. A target built from an optional field or a table entry can
	// be left nil, and a nil *Error can be handed in as the error itself; both
	// must report "no match" rather than dereference.
	var want *cordis.Error
	if errors.Is(err, want) {
		t.Fatal("a nil target must not match")
	}
	var nilErr *cordis.Error
	if errors.Is(nilErr, &cordis.Error{Code: cordis.ErrServiceExists}) {
		t.Fatal("a nil error must not match")
	}
}
