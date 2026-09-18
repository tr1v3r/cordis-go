package cordis_test

import (
	"errors"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

func TestFrameworkErrorLogsHaveOneCordisPrefix(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *cordis.Context)
	}{
		{
			name: "event listener",
			run: func(_ *testing.T, root *cordis.Context) {
				root.On("broken/event", func(int) { panic("event exploded") })
				root.Emit("broken/event", 1)
			},
		},
		{
			name: "waterfall listener",
			run: func(_ *testing.T, root *cordis.Context) {
				root.OnWaterfall("broken/waterfall", func(string, func(string) any) any {
					panic("waterfall listener exploded")
				})
				root.Waterfall("broken/waterfall", "x", nil)
			},
		},
		{
			name: "waterfall final",
			run: func(_ *testing.T, root *cordis.Context) {
				root.Waterfall("broken/final", "x", func(string) any {
					panic("waterfall final exploded")
				})
			},
		},
		{
			name: "service availability",
			run: func(t *testing.T, root *cordis.Context) {
				if _, err := root.ProvideChecked("broken/service", 1, func() bool {
					panic("service exploded")
				}); err != nil {
					t.Fatalf("want service registration success, got %v", err)
				}
				if _, ok := cordis.Get[int](root, "broken/service"); ok {
					t.Fatal("want a panicking availability check to hide the service, got visible")
				}
			},
		},
		{
			name: "plugin load failure",
			run: func(t *testing.T, root *cordis.Context) {
				fiber, err := root.Load(cordis.Define[struct{}]("broken/plugin",
					func(*cordis.Context, struct{}) error { return errors.New("load exploded") }),
					struct{}{})
				if err == nil {
					t.Fatal("want plugin load failure, got nil")
				}
				fiber.Dispose()
			},
		},
		{
			name: "disposer panic",
			run: func(_ *testing.T, root *cordis.Context) {
				dispose := root.OnDispose(func() { panic("dispose exploded") })
				dispose()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &panicLogRecorder{}
			root := cordis.New(cordis.WithWriter(recorder))
			test.run(t, root)
			wantOneCordisPrefix(t, recorder.snapshot())
		})
	}
}

func wantOneCordisPrefix(t *testing.T, lines []string) {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("want one framework log line, got %d: %q", len(lines), lines)
	}
	if got := strings.Count(lines[0], "cordis:"); got != 1 {
		t.Fatalf("want exactly one cordis prefix, got %d in %q", got, lines[0])
	}
}
