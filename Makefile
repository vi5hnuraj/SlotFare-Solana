# Slotfare quality gates, in the order CI runs them.
#
# Every target is offline: nothing here talks to a cluster, a database or a
# paid API. The live smoke tests are opt-in and stay behind their env vars
# (SLOTFARE_LIVE, SLOTFARE_CONSOLE_SMOKE) - see README, "Build and test".
#
#   make            build, vet and test Go, then the Rust program, then the
#                   console's build and tests
#   make check      the same, plus the formatting gates (what CI enforces)
#   make demo       the end-to-end deposit walkthrough (needs the Agave
#                   toolchain and a local validator; it starts one)

GO     ?= go
CARGO  ?= cargo
PROGRAM := programs/slotfare-ledger
WEB     := web

.PHONY: all check build vet test fmt-check \
        rust-test rust-fmt rust-clippy \
        web-install web-build web-test \
        demo clean

## --- Go ---------------------------------------------------------------

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./... -count=1

# gofmt -l prints the files that need formatting; an empty output is a pass.
fmt-check:
	@files=$$(gofmt -l cmd internal); \
	if [ -n "$$files" ]; then \
		echo "gofmt needed on:"; echo "$$files"; exit 1; \
	fi

## --- On-chain program --------------------------------------------------

rust-test:
	cd $(PROGRAM) && $(CARGO) test

rust-fmt:
	cd $(PROGRAM) && $(CARGO) fmt --check

rust-clippy:
	cd $(PROGRAM) && $(CARGO) clippy --all-targets

## --- Console (Angular) -------------------------------------------------

web-install:
	cd $(WEB) && npm ci --no-audit --no-fund

web-build:
	cd $(WEB) && npm run build

web-test:
	cd $(WEB) && npx ng test --watch=false

## --- Composite ---------------------------------------------------------

check: fmt-check build vet test rust-fmt rust-test rust-clippy web-build web-test
	@echo "all gates green"

all: build vet test rust-test web-build web-test
	@echo "build and test green (formatting gates: make check)"

## --- End to end --------------------------------------------------------

demo:
	scripts/demo-local.sh

clean:
	rm -rf $(PROGRAM)/target $(WEB)/dist $(WEB)/.angular
