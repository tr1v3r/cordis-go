package cordis_test

import (
	"bytes"
	"strings"
	"testing"

	cordis "github.com/tr1v3r/cordis-go"
)

func TestLoggerWritesEnabledLevels(t *testing.T) {
	var out bytes.Buffer
	root := cordis.New(cordis.WithWriter(&out), cordis.WithLevel(cordis.LevelDebug))
	defer root.Fiber().Dispose()

	app := root.Logger("app")
	if app.Name() != "app" {
		t.Fatalf("want the given logger name, got %q", app.Name())
	}
	app.Debug("debug %d", 1)
	app.Info("info")
	app.Warn("warn %s", "x")
	app.Error("error")

	for _, want := range []string{
		"[debug] app: debug 1",
		"[info] app: info",
		"[warn] app: warn x",
		"[error] app: error",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("want %q in the log output, got %q", want, out.String())
		}
	}
}

func TestLoggerDropsLevelsBelowTheThreshold(t *testing.T) {
	var out bytes.Buffer
	root := cordis.New(cordis.WithWriter(&out), cordis.WithLevel(cordis.LevelWarn))
	defer root.Fiber().Dispose()

	app := root.Logger("app")
	app.Debug("hidden")
	app.Info("hidden")
	if out.Len() != 0 {
		t.Fatalf("want nothing below the level, got %q", out.String())
	}

	app.Warn("shown")
	if !strings.Contains(out.String(), "[warn] app: shown") {
		t.Fatalf("want the line at the level, got %q", out.String())
	}
}

func TestLoggerAtSilentLevelWritesNothing(t *testing.T) {
	var out bytes.Buffer
	root := cordis.New(cordis.WithWriter(&out), cordis.WithLevel(cordis.LevelSilent))
	defer root.Fiber().Dispose()

	root.Logger("app").Error("quiet")
	if out.Len() != 0 {
		t.Fatalf("want a silent logger to write nothing, got %q", out.String())
	}
}

func TestLoggerNamesComeFromTheFiber(t *testing.T) {
	var out bytes.Buffer
	root := cordis.New(cordis.WithWriter(&out), cordis.WithLevel(cordis.LevelInfo))
	defer root.Fiber().Dispose()

	if name := root.Logger().Name(); name != "root" {
		t.Fatalf("want the root fiber name, got %q", name)
	}

	// A plugin logs under the name it was defined with, so a host can tell
	// entries apart without the plugin naming itself.
	plugin := cordis.Define[struct{}]("worker", func(ctx *cordis.Context, _ struct{}) error {
		ctx.Logger().Info("loaded")
		return nil
	})
	if _, err := cordis.Load(root, plugin, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[info] worker: loaded") {
		t.Fatalf("want the plugin name in the log line, got %q", out.String())
	}
}

func TestLoggerServiceProvidesNamedLoggers(t *testing.T) {
	var out bytes.Buffer
	root := cordis.New(cordis.WithWriter(&out), cordis.WithLevel(cordis.LevelInfo))
	defer root.Fiber().Dispose()

	service, ok := cordis.Get[*cordis.LoggerService](root, "logger")
	if !ok {
		t.Fatal("want the built-in logger service")
	}
	service.Logger("plugin").Info("from the service")
	if !strings.Contains(out.String(), "[info] plugin: from the service") {
		t.Fatalf("want the named logger to write, got %q", out.String())
	}
}

func TestLoggerWithoutWriterDiscardsOutput(t *testing.T) {
	// A host that supplies no writer gets io.Discard, not a nil dereference.
	root := cordis.New(cordis.WithWriter(nil))
	defer root.Fiber().Dispose()

	root.Logger("app").Error("dropped")
}

func TestLevelString(t *testing.T) {
	for level, want := range map[cordis.Level]string{
		cordis.LevelDebug:  "debug",
		cordis.LevelInfo:   "info",
		cordis.LevelWarn:   "warn",
		cordis.LevelError:  "error",
		cordis.LevelSilent: "silent",
		cordis.Level(99):   "silent",
	} {
		if got := level.String(); got != want {
			t.Fatalf("want %q for level %d, got %q", want, level, got)
		}
	}
}
