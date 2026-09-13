package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseJSON is a base layer with two entries: "db" and the group "grp", whose
// child "metrics" lives in the entry-level plugins list.
const baseJSON = `[
  {"id": "db", "name": "database-module", "config": {"path": "data/app.db"}},
  {"id": "grp", "group": true, "plugins": [
    {"id": "metrics", "name": "metrics-module", "config": {"interval": "5s"}}
  ]}
]`

// patchJSON patches the group's nested entry and carries one patch id that
// matches nothing, so `--strict` has something to reject.
const patchJSON = `[
  {"id": "metrics", "config": {"interval": "1s"}},
  {"id": "typo", "config": {"nope": true}}
]`

// matchedJSON is a patch layer whose only id matches the base's nested entry.
const matchedJSON = `[
  {"id": "metrics", "config": {"interval": "2s"}}
]`

// runCapture runs the command line in-process and captures what it wrote.
func runCapture(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// writeLayers puts the two fixture layers in a fresh directory.
func writeLayers(t *testing.T) (base, patch string) {
	t.Helper()
	dir := t.TempDir()
	base = filepath.Join(dir, "base.json")
	patch = filepath.Join(dir, "profile.json")
	for path, data := range map[string]string{base: baseJSON, patch: patchJSON} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}
	return base, patch
}

func TestRunHelpGoesToStdoutWithExitZero(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"dump", "--help"}} {
		code, stdout, stderr := runCapture(t, args...)
		if code != 0 {
			t.Errorf("args %v: want exit 0, got %d", args, code)
		}
		if !strings.HasPrefix(stdout, "usage:") {
			t.Errorf("args %v: want usage on stdout, got %q", args, stdout)
		}
		if stderr != "" {
			t.Errorf("args %v: want empty stderr, got %q", args, stderr)
		}
	}
}

func TestRunUsageErrorsGoToStderrWithExitTwo(t *testing.T) {
	base, _ := writeLayers(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			"unknown command",
			[]string{"compose"},
			`unknown command "compose"`,
		},
		{
			"unknown long flag",
			[]string{"dump", "--nope", base},
			`unknown flag "--nope"`,
		},
		{
			"unknown short flag",
			[]string{"dump", "-patch", base},
			`unknown flag "-patch"`,
		},
		{
			"patch without value",
			[]string{"dump", "--patch", base},
			"--patch requires a path",
		},
		{
			"empty patch value",
			[]string{"dump", "--patch=", base},
			"--patch requires a non-empty path",
		},
		{
			"patch names no file",
			[]string{"dump", "--patch=/tmp/absent.json", base},
			"does not name any config file",
		},
		{
			"no file at all",
			[]string{"dump", "--strict"},
			"requires at least one config file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCapture(t, tc.args...)
			if code != 2 {
				t.Errorf("want exit 2, got %d (stderr %q)", code, stderr)
			}
			if stdout != "" {
				t.Errorf("want empty stdout, got %q", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("want stderr containing %q, got %q", tc.want, stderr)
			}
			if !strings.Contains(stderr, "usage:") {
				t.Errorf("want usage text on stderr, got %q", stderr)
			}
		})
	}
}

func TestRunNoCommandMentionsWhatIsMissing(t *testing.T) {
	// A bare invocation is the one usage error with no arguments to quote, so
	// the message has to name the missing command on its own.
	code, _, stderr := runCapture(t)
	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.HasPrefix(stderr, "cordis: missing command\n") {
		t.Errorf("want a leading %q, got %q", "cordis: missing command", stderr)
	}
}

func TestRunLoadFailuresUseExitOne(t *testing.T) {
	base, patch := writeLayers(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.json")
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		// --strict turns the unmatched "typo" patch id into an error.
		{
			"unmatched strict patch",
			[]string{"dump", "--strict", base, patch},
			`patch id "typo" matched no entry`,
		},
		{"missing file", []string{"dump", missing}, missing},
		{"malformed json", []string{"dump", broken}, "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCapture(t, tc.args...)
			if code != 1 {
				t.Errorf("want exit 1, got %d (stderr %q)", code, stderr)
			}
			if stdout != "" {
				t.Errorf("want empty stdout, got %q", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("want stderr containing %q, got %q", tc.want, stderr)
			}
			if strings.Contains(stderr, "usage:") {
				t.Errorf("want no usage text for a runtime failure, got %q", stderr)
			}
		})
	}
}

func TestRunComposesLayersInOrder(t *testing.T) {
	base, patch := writeLayers(t)
	code, stdout, stderr := runCapture(t, "dump", base, patch)
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr %q)", code, stderr)
	}
	if !strings.HasPrefix(stdout, "# cordis-go config dump\n") {
		t.Errorf("want the dump header first, got %q", stdout)
	}
	if !strings.Contains(stdout, "patched by "+patch) {
		t.Errorf("want the nested patch recorded in the provenance, got %q", stdout)
	}
	wantWarning := `# warning: layer ` + patch + `: patch id "typo" matched no entry`
	if !strings.Contains(stdout, wantWarning) {
		t.Errorf("want warning %q, got %q", wantWarning, stdout)
	}
}

func TestRunPatchFlagForcesPatchModeForTheFirstFile(t *testing.T) {
	// The first file is the base layer by position; --patch flips it to a patch
	// layer. The patch id "typo" (present in patchJSON) is the observable proof:
	// as a base layer it creates no entry and warns, as a patch layer it matches
	// nothing and warns about the patch instead.
	_, patch := writeLayers(t)

	code, stdout, _ := runCapture(t, "dump", patch)
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "# layers: "+patch+"\n") {
		t.Errorf("want the file listed as the only layer, got %q", stdout)
	}
	if strings.Contains(stdout, "patch id") {
		t.Errorf("want no patch warning while the file is a base layer, got %q", stdout)
	}
	if !strings.Contains(stdout, `- id: "typo"`) {
		t.Errorf("want the entry created by the base layer, got %q", stdout)
	}

	code, stdout, _ = runCapture(t, "dump", "--patch="+patch, patch)
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	want := `# warning: layer ` + patch + `: patch id "typo" matched no entry`
	if !strings.Contains(stdout, want) {
		t.Errorf("want %q once the flag forces patch mode, got %q", want, stdout)
	}
	if strings.Contains(stdout, `- id: "typo"`) {
		t.Errorf("want no entry created by a patch layer, got %q", stdout)
	}
}

func TestRunPatchJSONSuffixMakesTheFirstFileAPatchLayer(t *testing.T) {
	// The documented suffix rule: a first file named *.patch.json is a patch
	// layer without any flag, while the same content without the suffix is a
	// base layer that creates entries. The patch ids in the file are the
	// observable proof either way.
	dir := t.TempDir()
	solo := filepath.Join(dir, "solo.patch.json")
	if err := os.WriteFile(solo, []byte(patchJSON), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", solo, err)
	}

	code, stdout, stderr := runCapture(t, "dump", solo)
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("want empty stderr on success, got %q", stderr)
	}
	if layerList := "# layers: " + solo + "\n"; !strings.Contains(stdout, layerList) {
		t.Errorf("want the file listed as the only layer %q, got %q", layerList, stdout)
	}
	for _, id := range []string{"metrics", "typo"} {
		want := `# warning: layer ` + solo + `: patch id "` + id + `" matched no entry`
		if !strings.Contains(stdout, want) {
			t.Errorf("want patch warning %q, got %q", want, stdout)
		}
	}
	if strings.Contains(stdout, `- id: "typo"`) {
		t.Errorf("want no entry created by a patch layer, got %q", stdout)
	}
}

func TestRunPatchFlagIsPositionAndRepetitionInsensitive(t *testing.T) {
	// --patch must force patch mode wherever the flag appears on the command
	// line, and repeated flags must not change how any file is read, so
	// equivalent command lines must produce identical dumps.
	base, patch := writeLayers(t)

	// The flag after the file forces the same patch-only compose as before it.
	_, wantOut, _ := runCapture(t, "dump", "--patch="+patch, patch)
	code, stdout, stderr := runCapture(t, "dump", patch, "--patch="+patch)
	if code != 0 {
		t.Fatalf("want exit 0 with the flag after the file, got %d (stderr %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("want empty stderr on success, got %q", stderr)
	}
	if stdout != wantOut {
		t.Errorf("want the same dump with the flag after the file, got %q want %q",
			stdout, wantOut)
	}

	// A flag naming an already patch-position file must change nothing.
	_, wantOut, _ = runCapture(t, "dump", base, patch)
	code, stdout, stderr = runCapture(t, "dump", base, "--patch="+patch, patch)
	if code != 0 {
		t.Fatalf("want exit 0 with the flag on a later file, got %d (stderr %q)", code, stderr)
	}
	if stdout != wantOut {
		t.Errorf("want the plain two-layer dump when the flag names the later file, got %q want %q",
			stdout, wantOut)
	}

	// Repeated flags with distinct values must compose identically wherever
	// they are interleaved with the files. Forcing the first file leaves the
	// compose without a base layer, so both spellings dump two patch layers.
	_, wantOut, _ = runCapture(t, "dump", "--patch="+base, "--patch="+patch, base, patch)
	code, stdout, stderr = runCapture(t, "dump", "--patch="+base, base, "--patch="+patch, patch)
	if code != 0 {
		t.Fatalf("want exit 0 with repeated flags, got %d (stderr %q)", code, stderr)
	}
	if stdout != wantOut {
		t.Errorf("want the same dump with repeated flags interleaved, got %q want %q",
			stdout, wantOut)
	}
	if layers := "# layers: " + base + " -> " + patch + "\n"; !strings.Contains(stdout, layers) {
		t.Errorf("want both forced files listed as layers %q, got %q", layers, stdout)
	}
}

func TestRunStrictOnlyFailsOnUnmatchedPatchIDs(t *testing.T) {
	// --strict widens unmatched patch ids into errors; it must keep composing
	// cleanly when nothing is unmatched, whether there are no patch layers at
	// all or only matching ones.
	base, _ := writeLayers(t)
	matched := filepath.Join(t.TempDir(), "matched.json")
	if err := os.WriteFile(matched, []byte(matchedJSON), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", matched, err)
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no patch layers", []string{"dump", "--strict", base}, ""},
		{"only matching patch ids", []string{"dump", "--strict", base, matched},
			"patched by " + matched},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCapture(t, tc.args...)
			if code != 0 {
				t.Fatalf("want exit 0, got %d (stderr %q)", code, stderr)
			}
			if stderr != "" {
				t.Errorf("want empty stderr, got %q", stderr)
			}
			if strings.Contains(stdout, "# warning") {
				t.Errorf("want no warnings under --strict, got %q", stdout)
			}
			if tc.want != "" && !strings.Contains(stdout, tc.want) {
				t.Errorf("want strict to keep the applied patch, want %q, got %q",
					tc.want, stdout)
			}
		})
	}
}
