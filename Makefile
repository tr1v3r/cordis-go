# Common tasks. CI runs `make ci`.
#
# staticcheck and revive must be built with a Go at least as new as the version
# in go.mod. A binary built with an older Go cannot parse generic methods and
# either fails outright or silently analyzes nothing; `make tools` installs
# versions known to work.

GO          ?= go
GOFMT       ?= gofmt
STATICCHECK ?= staticcheck
REVIVE      ?= revive

# Reporting samples one second per benchmark; the gate below samples less per
# benchmark but repeats, which is what the comparison aggregates over.
BENCHTIME  ?= 1s
CHECKTIME  ?= 150ms
CHECKCOUNT ?= 5
# Benchmark numbers are only comparable on one machine, so the baseline is
# per-platform and the gate refuses to compare across platforms.
PLATFORM   ?= $(shell $(GO) env GOOS)-$(shell $(GO) env GOARCH)
BASELINE   ?= benchmarks/baseline-$(PLATFORM).txt
# The two load-layer cases that read a file measure this machine's sandboxed
# filesystem (a cached 5KB read costs ~140us here, and a 26-byte read costs the
# same), so their spread is the environment rather than this package. They are
# still measured and printed; only BenchmarkLoadLayer/ParseOnly, which parses a
# byte slice in memory, is gated.
#
# Time regressions are reported but not gated by default: on a machine that also
# runs a browser, one sampling window can be 40-70% slower than another for the
# same code, so a time threshold either fires on the machine or is too wide to
# mean anything. Allocations are deterministic and are gated exactly; add
# -gate-time on a quiet machine or a dedicated runner.
BENCHCHECKFLAGS ?= -ignore=BenchmarkLoadLayer/ReadFileOnly,BenchmarkLoadLayer/Combined

.PHONY: all fmt vet lint test benchsmoke benchmark baseline benchcheck \
	integration fuzz examples tools ci

all: ci

# Format every Go source in place.
fmt:
	$(GOFMT) -w .

vet:
	$(GO) vet ./...

# Everything CI checks: formatting, vet, staticcheck, revive.
lint: vet
	@unformatted="$$($(GOFMT) -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would rewrite:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	$(STATICCHECK) ./...
	$(REVIVE) -config .revive.toml ./...

test:
	$(GO) test -race ./...

# Execute every benchmark body once. `go test -bench` is the only thing that
# runs them, and each body validates its own result, so this keeps the benchmark
# suite itself honest; -race additionally checks the run-in-parallel cases
# against the detector. One iteration per benchmark keeps it cheap enough for
# `make ci`.
benchsmoke:
	$(GO) test -race -run '^$$' -bench . -benchtime=1x ./...

# Measure the core and loader hot paths. Override BENCHTIME for longer sampling.
benchmark:
	$(GO) test -run '^$$' -bench . -benchmem -benchtime=$(BENCHTIME) ./...

# Record the baseline the gate compares against. Regenerate it after an
# intentional change - a new benchmark, a deliberate trade-off, a new Go
# toolchain - and never just to make a failing check pass.
baseline:
	@mkdir -p benchmarks
	$(GO) test -run '^$$' -bench . -benchmem \
		-benchtime=$(CHECKTIME) -count=$(CHECKCOUNT) ./... > $(BASELINE)
	@printf 'wrote %s: %s benchmarks\n' "$(BASELINE)" \
		"$$(grep -c '^Benchmark' $(BASELINE))"

# Compare a fresh run against the committed baseline and fail on a regression.
# The baseline and this check must sample identically, which is why both use
# CHECKTIME/CHECKCOUNT; overriding either means regenerating the baseline.
benchcheck:
	@current=$$(mktemp); \
	trap 'rm -f "$$current"' EXIT; \
	$(GO) test -run '^$$' -bench . -benchmem \
		-benchtime=$(CHECKTIME) -count=$(CHECKCOUNT) ./... > "$$current" || exit 1; \
	$(GO) run ./cmd/benchcheck $(BENCHCHECKFLAGS) $(BASELINE) "$$current"

# Everything the integration workflow runs: the whole suite under the race
# detector, three times over. A single run can hide ordering- and timing-
# dependent failures behind one lucky schedule, and a plain repeat would be
# answered from the test cache; -count=3 forces real reruns so flakes surface.
integration:
	$(GO) test -race -count=3 ./...

# Fuzz the loader configuration path for a bounded time. go test runs one
# -fuzz target per invocation, so this names the target; the seed corpus runs
# on every plain `go test` as ordinary regression cases.
fuzz:
	$(GO) test -fuzz=FuzzCompose -fuzztime=30s -run '^$$' ./loader

# Run every example once. `go vet` already compiles them; this executes them,
# because the README tells readers to, and a demo that no longer runs is a
# regression the compiler cannot see. The timeout tool guards a hang where it
# exists (CI); locally a hung demo still fails the target by hand.
examples:
	@for dir in examples/*/; do \
		name=$$(basename "$$dir"); \
		printf '=== %s\n' "$$name"; \
		if command -v timeout >/dev/null 2>&1; then \
			timeout 60 $(GO) run ./examples/$$name || exit 1; \
		else \
			$(GO) run ./examples/$$name || exit 1; \
		fi \
	done

# Install the lint tools at the versions this repository is verified against.
tools:
	$(GO) install honnef.co/go/tools/cmd/staticcheck@v0.8.1
	$(GO) install github.com/mgechev/revive@v1.16.0

ci: lint test benchsmoke
