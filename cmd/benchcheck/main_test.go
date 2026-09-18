package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// header renders the block `go test -bench` prints before a package's results.
func header(goos, goarch, cpu, pkg string) string {
	return "goos: " + goos + "\n" +
		"goarch: " + goarch + "\n" +
		"pkg: " + pkg + "\n" +
		"cpu: " + cpu + "\n"
}

// line renders one result line with the -benchmem fields, as the runner does.
func line(name string, iterations int, nanoseconds float64, bytes, allocs int64) string {
	return fmt.Sprintf("%s-12\t%d\t%.2f ns/op\t%d B/op\t%d allocs/op\n",
		name, iterations, nanoseconds, bytes, allocs)
}

// writeFixture writes one benchmark output and returns its path.
func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
}

// runCapture runs the command line in-process and captures what it wrote.
func runCapture(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// compare runs the tool over two fixtures and returns the exit status.
func compareFixtures(t *testing.T, baseline, current string,
	extra ...string) (int, string, string) {
	t.Helper()
	basePath := writeFixture(t, "baseline.txt", baseline)
	curPath := writeFixture(t, "current.txt", current)
	return runCapture(t, append(extra, basePath, curPath)...)
}

func TestRunHelpGoesToStdoutWithExitZero(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
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
	base := writeFixture(t, "baseline.txt", header("darwin", "arm64", "M3", "pkg")+
		line("BenchmarkA", 10, 10, 0, 0))
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no files", args: nil, want: "want a baseline file"},
		{name: "one file", args: []string{base}, want: "want a baseline file"},
		{name: "unknown flag", args: []string{"--nope", base, base}, want: "nope"},
		{name: "three files", args: []string{base, base, base}, want: "want a baseline file"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			code, _, stderr := runCapture(t, testCase.args...)
			if code != 2 {
				t.Fatalf("want exit 2, got %d (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, testCase.want) {
				t.Fatalf("want %q on stderr, got %q", testCase.want, stderr)
			}
		})
	}
}

func TestRunPassesWhenNothingMoved(t *testing.T) {
	content := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkA", 100, 10, 8, 1) +
		line("BenchmarkB", 100, 100, 16, 2)
	code, stdout, stderr := compareFixtures(t, content, content)
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "benchcheck: OK (2 benchmarks, 0 regressions, 0 new)") {
		t.Fatalf("want the OK summary, got %q", stdout)
	}
	if strings.Contains(stdout, "BenchmarkA") {
		t.Fatalf("want unchanged benchmarks hidden by default, got %q", stdout)
	}
}

func TestRunVerbosePrintsEveryBenchmark(t *testing.T) {
	content := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 10, 0, 0)
	code, stdout, _ := compareFixtures(t, content, content, "-v")
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "BenchmarkA") {
		t.Fatalf("want the benchmark in the table, got %q", stdout)
	}
}

func TestRunFlagsTimeRegression(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 140, 0, 0)
	code, stdout, _ := compareFixtures(t, baseline, current, "-gate-time", "-tolerance=25")
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	const wantSlower = "benchcheck: SLOWER BenchmarkA: 100.00ns -> 140.00ns " +
		"(+40.0% vs median; baseline slowest 100.00ns, tolerance 25%)"
	if !strings.Contains(stdout, wantSlower) {
		t.Fatalf("want the regression line, got %q", stdout)
	}
	if !strings.Contains(stdout, "1 regressions, 0 missing, 0 new") {
		t.Fatalf("want the summary to count it, got %q", stdout)
	}
}

func TestRunAllowsRegressionWithinTolerance(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 120, 0, 0)
	code, _, stderr := compareFixtures(t, baseline, current)
	if code != 0 {
		t.Fatalf("want exit 0 inside the tolerance, got %d (stderr %q)", code, stderr)
	}
}

func TestRunHonoursAnExplicitTolerance(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 115, 0, 0)
	code, _, stderr := compareFixtures(t, baseline, current, "-gate-time", "-tolerance=10")
	if code != 1 {
		t.Fatalf("want exit 1 with a 10%% tolerance, got %d (stderr %q)", code, stderr)
	}
}

// TestRunAllowsRegressionUnderTheFloor pins the absolute guard: 30% is over the
// tolerance, but 0.9ns is under the default floor, so noise on a cheap benchmark
// cannot fail the gate.
func TestRunAllowsRegressionUnderTheFloor(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkCheap", 100, 3, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkCheap", 100, 3.9, 0, 0)
	code, _, stderr := compareFixtures(t, baseline, current)
	if code != 0 {
		t.Fatalf("want exit 0 under the floor, got %d (stderr %q)", code, stderr)
	}
	if _, _, stderr := compareFixtures(t, baseline, current, "-floor=0"); stderr != "" {
		t.Fatalf("want a clean run, got %q", stderr)
	}
}

// TestRunFlagsAllocationRegression pins that allocations are exact: no
// tolerance and no floor apply to them.
func TestRunFlagsAllocationRegression(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 8, 1)
	code, stdout, _ := compareFixtures(t, baseline, current)
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stdout, "benchcheck: MORE ALLOCS BenchmarkA: 0 -> 1") {
		t.Fatalf("want the allocation regression, got %q", stdout)
	}
}

func TestRunReportsImprovementWithoutFailing(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 50, 0, 0)
	code, stdout, _ := compareFixtures(t, baseline, current, "-v")
	if code != 0 {
		t.Fatalf("want exit 0 for an improvement, got %d", code)
	}
	if !strings.Contains(stdout, "-50.0%") {
		t.Fatalf("want the improvement in the table, got %q", stdout)
	}
}

func TestRunReportsNewBenchmarkWithoutFailing(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkA", 100, 100, 0, 0) +
		line("BenchmarkNew", 100, 10, 0, 0)
	code, stdout, _ := compareFixtures(t, baseline, current)
	if code != 0 {
		t.Fatalf("want exit 0 with a new benchmark, got %d", code)
	}
	if !strings.Contains(stdout, "1 new") {
		t.Fatalf("want the new benchmark counted, got %q", stdout)
	}
}

func TestRunFailsOnMissingBenchmark(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkA", 100, 100, 0, 0) +
		line("BenchmarkGone", 100, 10, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	code, stdout, _ := compareFixtures(t, baseline, current)
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stdout, "benchcheck: MISSING BenchmarkGone") {
		t.Fatalf("want the missing benchmark named, got %q", stdout)
	}
}

func TestRunAllowMissingDowngradesTheFailure(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkA", 100, 100, 0, 0) +
		line("BenchmarkGone", 100, 10, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	code, stdout, _ := compareFixtures(t, baseline, current, "-allow-missing")
	if code != 0 {
		t.Fatalf("want exit 0 with -allow-missing, got %d", code)
	}
	if !strings.Contains(stdout, "allowed by -allow-missing") {
		t.Fatalf("want the downgrade reported, got %q", stdout)
	}
}

func TestRunRefusesCrossPlatformComparison(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	current := header("linux", "amd64", "Xeon", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	code, _, stderr := compareFixtures(t, baseline, current)
	if code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "only compare on one machine") {
		t.Fatalf("want the refusal explained, got %q", stderr)
	}
	if code, _, _ := compareFixtures(t, baseline, current, "-force"); code != 0 {
		t.Fatalf("want -force to allow the comparison, got %d", code)
	}
}

// TestRunAggregatesRepeatsByMedian pins median-over-repeats: the mean of these
// three samples is 130, which would read as a 30% regression against 100.
func TestRunAggregatesRepeatsByMedian(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkA", 100, 90, 0, 0) +
		line("BenchmarkA", 100, 100, 0, 0) +
		line("BenchmarkA", 100, 200, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	code, _, stderr := compareFixtures(t, baseline, current)
	if code != 0 {
		t.Fatalf("want exit 0 for an unchanged median, got %d (stderr %q)", code, stderr)
	}
}

// TestRunGatesOnTheWorstAllocationCount pins the other half of the aggregation:
// allocations are gated on the largest repeat, not the median.
func TestRunGatesOnTheWorstAllocationCount(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkA", 100, 100, 0, 1) +
		line("BenchmarkA", 100, 100, 0, 1) +
		line("BenchmarkA", 100, 100, 0, 4)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 1)
	code, stdout, _ := compareFixtures(t, baseline, current)
	if code != 0 {
		t.Fatalf("want exit 0 when the current count is below the worst repeat, got %d", code)
	}
	if strings.Contains(stdout, "MORE ALLOCS") {
		t.Fatalf("want no allocation regression, got %q", stdout)
	}
}

// TestRunMatchesAcrossGOMAXPROCS pins the -N suffix handling: the baseline was
// recorded on a 4-core setting and the check runs on 12.
func TestRunMatchesAcrossGOMAXPROCS(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		"BenchmarkCompose/Base/10Entries-4\t100\t100.00 ns/op\t8 B/op\t1 allocs/op\n"
	current := header("darwin", "arm64", "M3", "pkg") +
		"BenchmarkCompose/Base/10Entries-12\t100\t100.00 ns/op\t8 B/op\t1 allocs/op\n"
	code, stdout, _ := compareFixtures(t, baseline, current)
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "0 regressions, 0 new") {
		t.Fatalf("want the runs matched, got %q", stdout)
	}
}

// TestRunReadsMemoryFieldsAfterThroughput pins the field scan: -benchtime with
// SetBytes inserts an MB/s pair between B/op and allocs/op.
func TestRunReadsMemoryFieldsAfterThroughput(t *testing.T) {
	const throughput = "BenchmarkParseLayer/1Entries-12\t100\t923.40 ns/op\t" +
		"58.48 MB/s\t1161 B/op\t16 allocs/op\n"
	baseline := header("darwin", "arm64", "M3", "pkg/loader") + throughput
	current := header("darwin", "arm64", "M3", "pkg/loader") +
		"BenchmarkParseLayer/1Entries-12\t100\t923.40 ns/op\t58.48 MB/s\t1161 B/op\t32 allocs/op\n"
	code, stdout, _ := compareFixtures(t, baseline, current)
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(stdout, "MORE ALLOCS BenchmarkParseLayer/1Entries: 16 -> 32") {
		t.Fatalf("want the counts read past MB/s, got %q", stdout)
	}
}

func TestRunRejectsAFileWithoutResults(t *testing.T) {
	empty := header("darwin", "arm64", "M3", "pkg") + "ok  \tpkg\t0.1s\n"
	good := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	code, _, stderr := compareFixtures(t, empty, good)
	if code != 2 {
		t.Fatalf("want exit 2 for a baseline without results, got %d", code)
	}
	if !strings.Contains(stderr, "holds no benchmark results") {
		t.Fatalf("want the empty baseline explained, got %q", stderr)
	}
}

func TestRunReportsAMissingFile(t *testing.T) {
	good := writeFixture(t, "good.txt",
		header("darwin", "arm64", "M3", "pkg")+line("BenchmarkA", 100, 100, 0, 0))
	code, _, stderr := runCapture(t, filepath.Join(t.TempDir(), "absent.txt"), good)
	if code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "absent.txt") {
		t.Fatalf("want the path in the error, got %q", stderr)
	}
}

// TestRunRejectsOneNameInTwoPackages pins the ambiguity guard: the report keys
// benchmarks by name, so the same name in two packages must not merge silently.
func TestRunRejectsOneNameInTwoPackages(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkSame", 100, 100, 0, 0) +
		header("darwin", "arm64", "M3", "pkg/loader") +
		line("BenchmarkSame", 100, 100, 0, 0)
	good := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	code, _, stderr := compareFixtures(t, baseline, good)
	if code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "rename one of them") {
		t.Fatalf("want the ambiguity explained, got %q", stderr)
	}
}

func TestRunIgnoresNonResultLines(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkA", 100, 100, 0, 0) +
		"PASS\nok  \tpkg\t12.3s\n" +
		"--- FAIL: BenchmarkBroken\n"
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	code, stdout, stderr := compareFixtures(t, baseline, current)
	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr %q, stdout %q)", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "1 benchmarks") {
		t.Fatalf("want only the result line counted, got %q", stdout)
	}
}

func TestNormalizeNameStripsOnlyTheTrailingCounter(t *testing.T) {
	for _, testCase := range []struct {
		name string
		want string
	}{
		{name: "BenchmarkA-12", want: "BenchmarkA"},
		{name: "BenchmarkCompose/Base/10Entries-12", want: "BenchmarkCompose/Base/10Entries"},
		{name: "BenchmarkEventEmit/100Listeners-4", want: "BenchmarkEventEmit/100Listeners"},
		{name: "BenchmarkA", want: "BenchmarkA"},
		{name: "BenchmarkEmit/100Listeners", want: "BenchmarkEmit/100Listeners"},
	} {
		if got := normalizeName(testCase.name); got != testCase.want {
			t.Errorf("normalizeName(%q): want %q, got %q", testCase.name, testCase.want, got)
		}
	}
}

func TestMedian(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		values []float64
		want   float64
	}{
		{name: "odd count", values: []float64{90, 200, 100}, want: 100},
		{name: "even count", values: []float64{10, 20}, want: 15},
		{name: "single", values: []float64{7}, want: 7},
		{name: "empty", values: nil, want: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := median(testCase.values); got != testCase.want {
				t.Fatalf("want %v, got %v", testCase.want, got)
			}
		})
	}
}

// TestRunIgnoreExcludesBenchmarksFromTheGate pins -ignore: the benchmark is still
// measured and reported, but it cannot fail the check.
func TestRunIgnoreExcludesBenchmarksFromTheGate(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkLoadLayer/Combined", 100, 100, 0, 0) +
		line("BenchmarkParseLayer", 100, 100, 0, 1)
	current := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkLoadLayer/Combined", 100, 300, 0, 0) +
		line("BenchmarkParseLayer", 100, 100, 0, 1)
	if code, _, _ := compareFixtures(t, baseline, current, "-gate-time"); code != 1 {
		t.Fatalf("want the unignored regression to fail, got %d", code)
	}
	code, stdout, stderr := compareFixtures(t, baseline, current,
		"-gate-time", "-ignore=BenchmarkLoadLayer/Combined")
	if code != 0 {
		t.Fatalf("want exit 0 with the regression ignored, got %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "1 benchmarks measured but not gated by -ignore") {
		t.Fatalf("want the ignore reported, got %q", stdout)
	}
}

// TestRunIgnoreLeavesOtherBenchmarksGated pins that -ignore is a prefix filter
// and not a blanket switch: a regression elsewhere still fails the check.
func TestRunIgnoreLeavesOtherBenchmarksGated(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkLoadLayer/Combined", 100, 100, 0, 0) +
		line("BenchmarkParseLayer", 100, 100, 0, 1)
	current := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkLoadLayer/Combined", 100, 300, 0, 0) +
		line("BenchmarkParseLayer", 100, 100, 0, 2)
	code, stdout, _ := compareFixtures(t, baseline, current,
		"-gate-time", "-ignore=BenchmarkLoadLayer/Combined")
	if code != 1 {
		t.Fatalf("want exit 1 for the unignored allocation regression, got %d", code)
	}
	if !strings.Contains(stdout, "MORE ALLOCS BenchmarkParseLayer: 1 -> 2") {
		t.Fatalf("want the unignored regression reported, got %q", stdout)
	}
}

// TestRunIgnoreKeepsMatchingBenchmarksVisible pins that an ignored benchmark is
// still printed by -v, so ignoring one does not mean losing it.
func TestRunIgnoreKeepsMatchingBenchmarksVisible(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkNoisy", 100, 100, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkNoisy", 100, 300, 0, 0)
	code, stdout, _ := compareFixtures(t, baseline, current, "-ignore=BenchmarkNoisy", "-v")
	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "BenchmarkNoisy") || !strings.Contains(stdout, "+200.0%") {
		t.Fatalf("want the ignored benchmark and its delta in the table, got %q", stdout)
	}
}

// TestRunDefaultToleranceIsFifty pins the shipped default and why it is loose: a
// laptop drifts over a whole measurement window, so a tighter band reports
// regressions that are the machine, not the code.
func TestRunDefaultToleranceIsFifty(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		current float64
		want    int
	}{
		{name: "inside the band", current: 140, want: 0},
		{name: "outside the band", current: 160, want: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
			current := header("darwin", "arm64", "M3", "pkg") +
				line("BenchmarkA", 100, testCase.current, 0, 0)
			code, _, stderr := compareFixtures(t, baseline, current, "-gate-time")
			if code != testCase.want {
				t.Fatalf("want exit %d, got %d (stderr %q)", testCase.want, code, stderr)
			}
		})
	}
}

// TestRunGatesAgainstTheWorstBaselineRepeat pins the self-calibrating band: a
// baseline whose own repeats differ by 100% must not treat a current run inside
// that spread as a regression.
func TestRunGatesAgainstTheWorstBaselineRepeat(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") +
		line("BenchmarkA", 100, 100, 0, 0) +
		line("BenchmarkA", 100, 100, 0, 0) +
		line("BenchmarkA", 100, 200, 0, 0)
	inside := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 240, 0, 0)
	code, _, stderr := compareFixtures(t, baseline, inside, "-gate-time", "-tolerance=25")
	if code != 0 {
		t.Fatalf("want exit 0 inside the baseline spread, got %d (stderr %q)", code, stderr)
	}
	outside := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 260, 0, 0)
	code, stdout, _ := compareFixtures(t, baseline, outside, "-gate-time", "-tolerance=25")
	if code != 1 {
		t.Fatalf("want exit 1 beyond the baseline spread, got %d", code)
	}
	if !strings.Contains(stdout, "baseline slowest 200.00ns") {
		t.Fatalf("want the slowest repeat named as the reference, got %q", stdout)
	}
}

// TestRunReportsTimeRegressionWithoutGating pins the shipped default: a time
// change is printed and counted, but only allocations fail the check unless
// -gate-time says otherwise.
func TestRunReportsTimeRegressionWithoutGating(t *testing.T) {
	baseline := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 100, 0, 0)
	current := header("darwin", "arm64", "M3", "pkg") + line("BenchmarkA", 100, 300, 0, 0)
	code, stdout, stderr := compareFixtures(t, baseline, current)
	if code != 0 {
		t.Fatalf("want exit 0 without -gate-time, got %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "benchcheck: SLOWER BenchmarkA") ||
		!strings.Contains(stdout, "[not gated: pass -gate-time to fail on time]") {
		t.Fatalf("want the time change reported and marked, got %q", stdout)
	}
	if !strings.Contains(stdout, "benchcheck: OK (1 benchmarks, 0 regressions, 0 new)") {
		t.Fatalf("want an OK verdict, got %q", stdout)
	}
	if !strings.Contains(stdout, "BenchmarkA") {
		t.Fatalf("want the change in the table, got %q", stdout)
	}
}
