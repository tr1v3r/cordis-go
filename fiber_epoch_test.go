package cordis_test

import (
	"sync/atomic"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

// TestTransientUnavailabilityDoesNotWedgeFiber pins the load precondition: a
// dependency that looks unavailable exactly when load checks it must leave the
// fiber able to load, not committed to an epoch whose body never ran.
func TestTransientUnavailabilityDoesNotWedgeFiber(t *testing.T) {
	root := cordis.New()

	// Probes in order: resolution (available), load's own liveness check
	// (unavailable), then available again.
	var probes atomic.Int32
	check := func() bool { return probes.Add(1) != 2 }
	if _, err := root.ProvideChecked("db", struct{}{}, check); err != nil {
		t.Fatal(err)
	}

	var runs atomic.Int32
	consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		runs.Add(1)
		ctx.OnDispose(func() {})
		return nil
	}).WithInject("db")

	fiber, err := root.Load(consumer, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if got := fiber.State(); got != cordis.StateActive {
		t.Fatalf("want active, got %s (body runs = %d, effects = %d)",
			got, runs.Load(), len(fiber.Effects()))
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("want one body run, got %d", got)
	}
	if got := len(fiber.Effects()); got != 1 {
		t.Fatalf("want one effect after the reload, got %d", got)
	}
}

// TestTransientUnavailabilityDuringReloadDoesNotWedgeFiber is the same pin for
// the Restart path, where the previous generation is already unloaded when the
// precondition fails.
func TestTransientUnavailabilityDuringReloadDoesNotWedgeFiber(t *testing.T) {
	root := cordis.New()

	var probes, failOn atomic.Int32
	check := func() bool { return probes.Add(1) != failOn.Load() }
	if _, err := root.ProvideChecked("db", struct{}{}, check); err != nil {
		t.Fatal(err)
	}

	var runs atomic.Int32
	consumer := cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		runs.Add(1)
		ctx.OnDispose(func() {})
		return nil
	}).WithInject("db")

	fiber, err := root.Load(consumer, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	waitFiberState(t, fiber, cordis.StateActive)

	// Let the reload resolve the dependency and fail only its liveness check.
	failOn.Store(probes.Load() + 2)
	if err := fiber.Restart(); err != nil {
		t.Fatal(err)
	}
	failOn.Store(0)

	waitFiberState(t, fiber, cordis.StateActive)
	if got := runs.Load(); got != 2 {
		t.Fatalf("want the body to run again, got %d runs", got)
	}
	if got := len(fiber.Effects()); got != 1 {
		t.Fatalf("want one effect after the reload, got %d", got)
	}
}
