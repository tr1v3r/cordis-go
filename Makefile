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

.PHONY: all fmt vet lint test integration fuzz tools ci

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

# Install the lint tools at the versions this repository is verified against.
tools:
	$(GO) install honnef.co/go/tools/cmd/staticcheck@v0.8.1
	$(GO) install github.com/mgechev/revive@v1.16.0

ci: lint test
