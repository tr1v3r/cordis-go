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

.PHONY: all fmt vet lint test tools ci

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

# Install the lint tools at the versions this repository is verified against.
tools:
	$(GO) install honnef.co/go/tools/cmd/staticcheck@v0.8.1
	$(GO) install github.com/mgechev/revive@v1.16.0

ci: lint test
