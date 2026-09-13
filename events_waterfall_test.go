package cordis_test

import (
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

func TestWaterfallSettlesOnceWhenListenerPanicsAfterNext(t *testing.T) {
	root := cordis.New()
	finals := 0
	cordis.OnWaterfall[string](root, "cmd", func(s string, next func(string) any) any {
		next(s + "-a")
		panic("boom")
	})

	got := cordis.Waterfall[string](root, "cmd", "x", func(s string) any {
		finals++
		return "final:" + s
	})

	if finals != 1 {
		t.Fatalf("want 1 call to final, got %d", finals)
	}
	if got != "final:x-a" {
		t.Fatalf("want final:x-a, got %v", got)
	}
}

func TestWaterfallSettlesOnceWhenListenerCallsNextTwice(t *testing.T) {
	root := cordis.New()
	finals := 0
	var first, second any
	cordis.OnWaterfall[string](root, "cmd", func(s string, next func(string) any) any {
		first = next(s + "-a")
		second = next(s + "-b")
		return first
	})

	got := cordis.Waterfall[string](root, "cmd", "x", func(s string) any {
		finals++
		return "final:" + s
	})

	if finals != 1 {
		t.Fatalf("want 1 call to final, got %d", finals)
	}
	if first != "final:x-a" || second != "final:x-a" {
		t.Fatalf("want both next calls to return final:x-a, got %v and %v", first, second)
	}
	if got != "final:x-a" {
		t.Fatalf("want final:x-a, got %v", got)
	}
}

func TestWaterfallKeepsChainWhenListenerPanicsBeforeNext(t *testing.T) {
	root := cordis.New()
	finals := 0
	cordis.OnWaterfall[string](root, "cmd", func(string, func(string) any) any {
		panic("boom")
	})
	cordis.OnWaterfall[string](root, "cmd", func(s string, next func(string) any) any {
		return next(s + "-b")
	})

	got := cordis.Waterfall[string](root, "cmd", "x", func(s string) any {
		finals++
		return "final:" + s
	})

	if finals != 1 {
		t.Fatalf("want 1 call to final, got %d", finals)
	}
	if got != "final:x-b" {
		t.Fatalf("want final:x-b, got %v", got)
	}

	cordis.OnWaterfall[string](root, "solo", func(string, func(string) any) any {
		panic("boom")
	})
	if got := cordis.Waterfall[string](root, "solo", "y",
		func(s string) any { return "final:" + s }); got != "final:y" {
		t.Fatalf("want final:y, got %v", got)
	}
}
