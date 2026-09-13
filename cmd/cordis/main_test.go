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
