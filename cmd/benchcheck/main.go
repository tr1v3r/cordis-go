// Command benchcheck compares two `go test -bench` outputs and fails on a
// regression.
//
// It is the gate behind `make benchcheck`: `make baseline` records the committed
// baseline, the check runs the same command with the same settings, and this
// tool reports what moved.
//
//	benchcheck benchmarks/baseline-darwin-arm64.txt current.txt
//	benchcheck -tolerance=10 -v baseline.txt current.txt
//	benchcheck --help
//
// Benchmark numbers are only comparable on one machine, so the two files must
// agree on the `goos:` / `goarch:` header `go test` prints; -force overrides
// that refusal when the comparison is deliberate.
//
// Repeats from -count=N are aggregated per benchmark: the median ns/op absorbs
// scheduling noise, and the largest allocs/op is what gets gated, because a
// count that moves between repeats is worth reporting on its own.
//
// A time regression must clear both -tolerance (a percentage) and -floor (an
// absolute number of nanoseconds), so noise on a 3ns benchmark cannot fail the
// gate while a real regression on a 100ns one still does. Allocations are exact:
// any increase fails, because the count is deterministic at a normal benchtime.
//
// A benchmark that only exists in the current run is reported as new and does
// not fail - adding coverage is not a regression - while one that only exists in
// the baseline fails, because a silently dropped benchmark is how a performance
// suite rots. -allow-missing downgrades that to a warning.
//
// The exit status is part of the interface: 0 when nothing regressed, 1 on a
// regression or a missing benchmark, 2 on a usage or parse error.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options holds the comparison knobs.
type options struct {
	tolerance    float64
	floor        float64
	ignore       []string
	gateTime     bool
	force        bool
	allowMissing bool
	verbose      bool
}

// sample is one result line from `go test -bench`.
type sample struct {
	name     string
	pkg      string
	nsPerOp  float64
	bytesOp  int64
	allocsOp int64
}

// environment is the machine the numbers belong to, as `go test -bench` reports
// it. Comparing across these is meaningless, which is why it is checked.
type environment struct {
	goos   string
	goarch string
	cpu    string
}

func (e environment) platform() string {
	switch {
	case e.goos == "" && e.goarch == "":
		return "unknown platform"
	case e.cpu == "":
		return e.goos + "/" + e.goarch
	default:
		return e.goos + "/" + e.goarch + " (" + e.cpu + ")"
	}
}

// result is one benchmark aggregated over its repeats.
type result struct {
	name     string
	nsPerOp  float64 // median
	bestNs   float64 // fastest repeat
	worstNs  float64 // slowest repeat
	allocsOp int64   // largest repeat
	repeats  int
}

// benchmarkFile is a parsed `go test -bench` output.
type benchmarkFile struct {
	env     environment
	results map[string]*result
	order   []string
}

// collector accumulates the repeats of one benchmark while the file is read.
type collector struct {
	pkg    string
	times  []float64
	allocs []int64
}

// verdict is what happened to one benchmark between the two runs.
type verdict string

// The verdicts, ordered by how loudly the report has to say them.
const (
	verdictSlower    verdict = "SLOWER"
	verdictMoreAlloc verdict = "MORE ALLOCS"
	verdictMissing   verdict = "missing"
	verdictNew       verdict = "new"
	verdictFaster    verdict = "faster"
	verdictSame      verdict = "same"
)

// change is the comparison of one benchmark across the two runs.
type change struct {
	name    string
	base    *result
	cur     *result
	verdict verdict
	timePct float64
	// ignored marks a benchmark that -ignore excludes from the gate. It is still
	// measured and printed, because an ignored benchmark that nobody looks at is
	// the same as a deleted one.
	ignored bool
}

func run(args []string, out, errOut io.Writer) int {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			usage(out)
			return 0
		}
	}

	settings := options{tolerance: 50, floor: 1}
	var ignored string
	flags := flag.NewFlagSet("benchcheck", flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.Usage = func() { usage(errOut) }
	flags.Float64Var(&settings.tolerance, "tolerance", settings.tolerance,
		"percentage a time regression must exceed, over the baseline's slowest "+
			"repeat, to fail the check")
	flags.Float64Var(&settings.floor, "floor", settings.floor,
		"nanoseconds a time regression must exceed to fail the check")
	flags.StringVar(&ignored, "ignore", "",
		"comma-separated benchmark name prefixes to measure but not gate")
	flags.BoolVar(&settings.gateTime, "gate-time", false,
		"fail on a time regression; off by default because a shared machine "+
			"drifts more than any useful threshold")
	flags.BoolVar(&settings.force, "force", false,
		"compare files from different platforms instead of refusing")
	flags.BoolVar(&settings.allowMissing, "allow-missing", false,
		"warn instead of failing when a baseline benchmark is gone")
	flags.BoolVar(&settings.verbose, "v", false,
		"print every benchmark, not only the ones that changed")
	// flag already reported the problem and the usage text.
	if err := flags.Parse(args); err != nil {
		return 2
	}
	settings.ignore = splitList(ignored)

	paths := flags.Args()
	if len(paths) != 2 {
		fmt.Fprintln(errOut, "benchcheck: want a baseline file and a current file")
		usage(errOut)
		return 2
	}
	base, err := parseBenchmarkFile(paths[0])
	if err != nil {
		fmt.Fprintf(errOut, "benchcheck: %v\n", err)
		return 2
	}
	current, err := parseBenchmarkFile(paths[1])
	if err != nil {
		fmt.Fprintf(errOut, "benchcheck: %v\n", err)
		return 2
	}
	if len(base.results) == 0 {
		fmt.Fprintf(errOut,
			"benchcheck: %s holds no benchmark results; record it with `make baseline`\n",
			paths[0])
		return 2
	}
	if len(current.results) == 0 {
		fmt.Fprintf(errOut, "benchcheck: %s holds no benchmark results\n", paths[1])
		return 2
	}
	if !settings.force && base.env.platform() != current.env.platform() {
		fmt.Fprintf(errOut,
			"benchcheck: refusing to compare %s with %s: benchmark numbers only "+
				"compare on one machine; regenerate the baseline there or pass -force\n",
			base.env.platform(), current.env.platform())
		return 2
	}

	changes := compare(base, current, settings)
	regressions, missing := report(out, base, current, changes, settings)
	if regressions > 0 || missing > 0 {
		return 1
	}
	return 0
}

// usage writes the command line reference to w.
func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  benchcheck [flags] <baseline.txt> <current.txt>

Compare two `+"`go test -bench`"+` outputs and report regressions.

  -tolerance=<percent>  a time regression must exceed, over the baseline's
                        slowest repeat, this percentage (default 50: a laptop
                        drifts over a whole measurement window, so tighten this
                        on a quiet machine rather than trusting the default)
  -floor=<ns>           and this absolute number of nanoseconds (default 1)
  -ignore=<prefixes>    comma-separated benchmark name prefixes to measure but
                        not gate, for cases whose cost is dominated by the
                        environment rather than by this package
  -gate-time            fail on a time regression too. Off by default: a machine
                        that also runs a browser drifts by tens of percent over a
                        whole sampling window, and allocations - which are
                        deterministic - catch the regressions that matter without
                        that noise. Turn it on for a quiet machine or a dedicated
                        runner.
  -force                compare files recorded on different platforms
  -allow-missing        warn instead of failing when a baseline benchmark is gone
  -v                    print every benchmark, not only the changed ones
  -h, --help            print this text and exit

Both files are produced by `+"`make benchmark`"+`-style runs with -benchmem; record
the baseline with `+"`make baseline`"+` and check with `+"`make benchcheck`"+`.

Exit status: 0 when nothing regressed, 1 on a regression or a missing benchmark,
2 on a usage or parse error.
`)
}

// parseBenchmarkFile reads one `go test -bench` output.
func parseBenchmarkFile(path string) (*benchmarkFile, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	file := &benchmarkFile{results: map[string]*result{}}
	collectors := map[string]*collector{}
	pkg := ""
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if value, ok := strings.CutPrefix(line, "goos: "); ok {
			file.env.goos = value
			continue
		}
		if value, ok := strings.CutPrefix(line, "goarch: "); ok {
			file.env.goarch = value
			continue
		}
		if value, ok := strings.CutPrefix(line, "cpu: "); ok {
			file.env.cpu = value
			continue
		}
		if value, ok := strings.CutPrefix(line, "pkg: "); ok {
			pkg = value
			continue
		}
		if !strings.HasPrefix(line, "Benchmark") {
			continue
		}
		value, ok, err := parseBenchLine(line, pkg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if !ok {
			continue
		}
		collected, seen := collectors[value.name]
		if !seen {
			collected = &collector{pkg: value.pkg}
			collectors[value.name] = collected
			file.order = append(file.order, value.name)
		}
		if collected.pkg != value.pkg {
			// Keying by name keeps the output readable, so a name that appears in
			// two packages would silently merge two different benchmarks.
			return nil, fmt.Errorf(
				"%s: benchmark name %q appears in both %s and %s; rename one of them",
				path, value.name, collected.pkg, value.pkg)
		}
		collected.times = append(collected.times, value.nsPerOp)
		collected.allocs = append(collected.allocs, value.allocsOp)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	for name, collected := range collectors {
		file.results[name] = &result{
			name:     name,
			nsPerOp:  median(collected.times),
			bestNs:   minimum(collected.times),
			worstNs:  maximumTime(collected.times),
			allocsOp: maximum(collected.allocs),
			repeats:  len(collected.times),
		}
	}
	return file, nil
}

// parseBenchLine decodes one result line. The value and unit pairs are scanned
// instead of matched with one pattern because -benchmem prints MB/s for a
// benchmark that sets bytes, between B/op and allocs/op.
func parseBenchLine(line, pkg string) (sample, bool, error) {
	fields := strings.Fields(line)
	// A result line is "name iterations value ns/op [value MB/s] [value B/op]
	// [value allocs/op]"; anything shorter is not one.
	if len(fields) < 4 || fields[3] != "ns/op" {
		return sample{}, false, nil
	}
	nanoseconds, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return sample{}, false, fmt.Errorf("line %q: ns/op value: %w", line, err)
	}
	value := sample{name: normalizeName(fields[0]), pkg: pkg, nsPerOp: nanoseconds}
	for index := 4; index+1 < len(fields); index++ {
		switch fields[index+1] {
		case "B/op":
			bytes, err := strconv.ParseInt(fields[index], 10, 64)
			if err != nil {
				return sample{}, false, fmt.Errorf("line %q: B/op value: %w", line, err)
			}
			value.bytesOp = bytes
		case "allocs/op":
			allocs, err := strconv.ParseInt(fields[index], 10, 64)
			if err != nil {
				return sample{}, false, fmt.Errorf("line %q: allocs/op value: %w", line, err)
			}
			value.allocsOp = allocs
		}
	}
	return value, true, nil
}

// normalizeName drops the trailing -N `go test -bench` appends, which is the
// value of GOMAXPROCS: it differs between machines and -cpu settings, and two
// runs of one benchmark have to compare equal regardless.
func normalizeName(name string) string {
	index := strings.LastIndexByte(name, '-')
	if index <= 0 {
		return name
	}
	if _, err := strconv.Atoi(name[index+1:]); err != nil {
		return name
	}
	return name[:index]
}

// median returns the middle value of a copy of values.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// minimum returns the smallest value, or zero for an empty slice.
func minimum(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	smallest := values[0]
	for _, value := range values[1:] {
		if value < smallest {
			smallest = value
		}
	}
	return smallest
}

// maximumTime returns the largest value, or zero for an empty slice.
func maximumTime(values []float64) float64 {
	var largest float64
	for _, value := range values {
		if value > largest {
			largest = value
		}
	}
	return largest
}

// maximum returns the largest value, or zero for an empty slice.
func maximum(values []int64) int64 {
	var largest int64
	for _, value := range values {
		if value > largest {
			largest = value
		}
	}
	return largest
}

// compare classifies every benchmark of both runs.
func compare(base, current *benchmarkFile, settings options) []change {
	changes := make([]change, 0, len(base.order)+len(current.order))
	seen := map[string]bool{}
	for _, name := range base.order {
		seen[name] = true
		changes = append(changes,
			classify(name, base.results[name], current.results[name], settings))
	}
	for _, name := range current.order {
		if seen[name] {
			continue
		}
		changes = append(changes, classify(name, nil, current.results[name], settings))
	}
	return changes
}

// classify decides one benchmark's verdict.
func classify(name string, base, current *result, settings options) change {
	entry := change{name: name, base: base, cur: current}
	switch {
	case base == nil:
		entry.verdict = verdictNew
	case current == nil:
		entry.verdict = verdictMissing
	default:
		entry.timePct = (current.nsPerOp - base.nsPerOp) / base.nsPerOp * 100
		// The reference is the baseline's slowest repeat, not its median: a
		// benchmark that already varies by 15% between repeats has not regressed
		// when the next run lands inside that band. Allocations stay exact.
		regression := (current.nsPerOp - base.worstNs) / base.worstNs * 100
		improvement := (base.bestNs - current.nsPerOp) / base.bestNs * 100
		switch {
		case current.allocsOp > base.allocsOp:
			entry.verdict = verdictMoreAlloc
		case current.nsPerOp-base.worstNs > settings.floor &&
			regression > settings.tolerance:
			entry.verdict = verdictSlower
		case base.bestNs-current.nsPerOp > settings.floor &&
			improvement > settings.tolerance:
			entry.verdict = verdictFaster
		default:
			entry.verdict = verdictSame
		}
	}
	entry.ignored = matchesPrefix(entry.name, settings.ignore)
	return entry
}

// matchesPrefix reports whether name starts with any of the prefixes.
func matchesPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// splitList parses a comma-separated flag value, dropping empty entries.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// report writes the comparison and returns the number of regressions and of
// benchmarks that disappeared from the current run.
func report(out io.Writer, base, current *benchmarkFile,
	changes []change, settings options) (regressions, missing int) {
	fmt.Fprintf(out, "benchcheck: %s -> %s, %d benchmarks, tolerance %.0f%%, floor %gns\n",
		base.env.platform(), current.env.platform(), len(changes),
		settings.tolerance, settings.floor)

	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "benchmark\tbaseline\tcurrent\tdelta\tallocs")
	ignored := 0
	for _, entry := range changes {
		if entry.ignored {
			ignored++
			if settings.verbose {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
					entry.name, timeCell(entry.base), timeCell(entry.cur),
					deltaCell(entry), allocCell(entry))
			}
			continue
		}
		if entry.verdict == verdictMoreAlloc ||
			(entry.verdict == verdictSlower && settings.gateTime) {
			regressions++
		}
		if entry.verdict == verdictMissing {
			missing++
		}
		if !settings.verbose && entry.verdict == verdictSame {
			continue
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			entry.name, timeCell(entry.base), timeCell(entry.cur),
			deltaCell(entry), allocCell(entry))
	}
	if err := writer.Flush(); err != nil {
		fmt.Fprintf(out, "benchcheck: writing the report failed: %v\n", err)
	}
	if ignored > 0 {
		fmt.Fprintf(out, "benchcheck: %d benchmarks measured but not gated by -ignore\n", ignored)
	}

	// Name every change before deciding the exit status: an ungated time change
	// is still worth reading, and a report that hides it would make -gate-time
	// look like the difference between noticing and not noticing.
	for _, entry := range changes {
		switch entry.verdict {
		case verdictSlower:
			fmt.Fprintf(out,
				"benchcheck: SLOWER %s: %.2fns -> %.2fns (%+.1f%% vs median; "+
					"baseline slowest %.2fns, tolerance %.0f%%)%s\n",
				entry.name, entry.base.nsPerOp, entry.cur.nsPerOp, entry.timePct,
				entry.base.worstNs, settings.tolerance, gatedSuffix(settings))
		case verdictMoreAlloc:
			fmt.Fprintf(out, "benchcheck: MORE ALLOCS %s: %d -> %d\n",
				entry.name, entry.base.allocsOp, entry.cur.allocsOp)
		case verdictMissing:
			fmt.Fprintf(out, "benchcheck: MISSING %s: in the baseline, not in the current run\n",
				entry.name)
		}
	}
	if missing > 0 && settings.allowMissing {
		fmt.Fprintf(out, "benchcheck: %d missing benchmarks allowed by -allow-missing\n", missing)
		missing = 0
	}
	if regressions == 0 && missing == 0 {
		fmt.Fprintf(out, "benchcheck: OK (%d benchmarks, 0 regressions, %d new)\n",
			len(changes), countVerdict(changes, verdictNew))
		return regressions, missing
	}
	fmt.Fprintf(out, "benchcheck: %d regressions, %d missing, %d new\n",
		regressions, missing, countVerdict(changes, verdictNew))
	return regressions, missing
}

// gatedSuffix marks a reported time change that did not fail the check.
func gatedSuffix(settings options) string {
	if settings.gateTime {
		return ""
	}
	return " [not gated: pass -gate-time to fail on time]"
}

func countVerdict(changes []change, want verdict) int {
	count := 0
	for _, entry := range changes {
		if entry.verdict == want {
			count++
		}
	}
	return count
}

// timeCell renders one side's time, or a dash when the benchmark is absent.
func timeCell(value *result) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%.2fns", value.nsPerOp)
}

// deltaCell renders the time change, or the verdict for a benchmark that only
// exists on one side.
func deltaCell(entry change) string {
	switch entry.verdict {
	case verdictMissing:
		return "gone"
	case verdictNew:
		return "new"
	default:
		return fmt.Sprintf("%+.1f%%", entry.timePct)
	}
}

// allocCell renders the allocation change.
func allocCell(entry change) string {
	switch {
	case entry.base == nil:
		return fmt.Sprintf("%d (new)", entry.cur.allocsOp)
	case entry.cur == nil:
		return fmt.Sprintf("%d (gone)", entry.base.allocsOp)
	case entry.base.allocsOp == entry.cur.allocsOp:
		return fmt.Sprintf("%d", entry.cur.allocsOp)
	default:
		return fmt.Sprintf("%d -> %d", entry.base.allocsOp, entry.cur.allocsOp)
	}
}
