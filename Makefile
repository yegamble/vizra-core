# vizra-core — `make ci` is the contract.
#
# ADR-002 § CI fan-in: every repository has one required check, `ci-required`,
# reading .github/required-checks.txt, where anything other than success —
# failure, cancellation, timeout, SKIP, never-ran — fails. The lanes below are
# what those checks run, so `make ci` locally and CI cannot disagree.
#
# A missing tool is a FAILURE here, never a silent skip (AGENTS.md: "a missing
# command or dependency is BLOCKED, never a pass").

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO          ?= go
SQLC        ?= sqlc
GOFLAGS     ?=
PKGS        := ./...

# Build identity, stamped into the binaries (ADR-001, ADR-002 § Release).
RELEASE     ?= dev
COMMIT      ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILT_AT    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X github.com/yegamble/vizra-core/internal/buildinfo.Release=$(RELEASE) \
               -X github.com/yegamble/vizra-core/internal/buildinfo.Commit=$(COMMIT) \
               -X github.com/yegamble/vizra-core/internal/buildinfo.BuiltAt=$(BUILT_AT)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
	 | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# The gate
# ---------------------------------------------------------------------------

.PHONY: ci
ci: fmt-check vet lint-imports migrate-lint config-template-check openapi-verify sqlc-verify ci-guard fixtures-verify test-race ## Everything ci-required runs
	@echo
	@echo "make ci: all lanes passed"

.PHONY: fmt-check
fmt-check: ## gofmt must have nothing to say
	@echo "==> fmt-check"
	@unformatted=$$(gofmt -l . ); \
	 if [ -n "$$unformatted" ]; then \
	   echo "  FAIL  these files are not gofmt-clean:"; \
	   echo "$$unformatted" | sed 's/^/          /'; \
	   echo "        Run: make fmt"; \
	   exit 1; \
	 fi; \
	 echo "  ok    all Go files are gofmt-clean"

.PHONY: fmt
fmt: ## Format
	@gofmt -w .

.PHONY: vet
vet: ## go vet
	@echo "==> vet"
	@$(GO) vet $(PKGS)
	@echo "  ok    go vet is clean"

.PHONY: lint-imports
lint-imports: ## Echo confinement, no package-global handles, no decoder in the api
	@echo "==> lint-imports"
	@./scripts/lint-imports.sh

.PHONY: migrate-lint
migrate-lint: ## Filename format, sequence gaps, down files, destructive statements, append-only manifest
	@echo "==> migrate-lint"
	@./scripts/migrate-lint.sh

.PHONY: migrations-manifest
migrations-manifest: ## Regenerate the append-only manifest (only when ADDING a migration)
	@./scripts/migration-manifest.sh generate

.PHONY: config-template-check
config-template-check: ## Every config key has exactly one documented home
	@echo "==> config-template-check"
	@$(GO) test -count=1 -run 'TestEveryKeyHasATemplateEntry|TestTemplateHasNoKeyNothingReads|TestTemplateSecretsAreRefusedInProduction|TestTemplateBootsInDevelopment|TestEscapeHatchesAreCommentedOutInTheTemplate' ./internal/config/

.PHONY: openapi-verify
openapi-verify: ## Route<->spec drift, BOTH directions, plus the internal search contract
	@echo "==> openapi-verify"
	@$(GO) test -count=1 -run 'TestEveryRouteHasASpecOperation|TestEverySpecOperationHasARoute|TestSpecOperationIDsAreUniqueAndPresent|TestM0ContractIsTheFourProbes|TestInternalSearchContractIsValid|TestInternalOperationsAreNotInThePublicContract|TestHMACTestVectorsArePublished' ./internal/httpapi/

.PHONY: sqlc-verify
sqlc-verify: ## Generated sqlc output matches the queries (hand edits fail)
	@echo "==> sqlc-verify"
	@command -v $(SQLC) >/dev/null 2>&1 || { \
	   echo "  FAIL  sqlc is not installed. This lane is BLOCKED, not passed."; \
	   echo "        Install sqlc 1.31.1 (ADR-001): https://docs.sqlc.dev/en/latest/overview/install.html"; \
	   exit 1; }
	@$(SQLC) diff
	@echo "  ok    internal/store/sqlcgen matches store/queries + migrations"

.PHONY: sqlc-generate
sqlc-generate: ## Regenerate sqlc output
	@$(SQLC) generate

.PHONY: ci-guard
ci-guard: ## The required-checks manifest floor, runner and action pinning
	@echo "==> ci-guard"
	@./scripts/ci-required-guard.sh

# ---------------------------------------------------------------------------
# Fixtures (VZ-FOUND-007, ADR-009)
# ---------------------------------------------------------------------------
#
# The corpus is NOT committed; fixtures/manifest.json is. `make fixtures`
# produces the twelve files from the pinned generator with nothing but a Go
# toolchain — no libvips, no exiftool, no Docker — so an M1 slice can get the
# bytes it needs in one command.

.PHONY: fixtures
fixtures: ## Generate the M0 fixture corpus into testdata/fixtures
	@echo "==> fixtures"
	@$(GO) run ./cmd/fixturegen -repo . generate

.PHONY: fixtures-verify
fixtures-verify: ## Regenerate the corpus and compare every byte against the committed manifest
	@echo "==> fixtures-verify"
	@$(GO) run ./cmd/fixturegen -repo . verify

.PHONY: fixtures-manifest
fixtures-manifest: ## Re-pin fixtures/manifest.json (ONLY when the corpus is meant to move)
	@$(GO) run ./cmd/fixturegen -repo . manifest

.PHONY: load-corpus
load-corpus: ## Generate the DECLARED load corpus (VZ-OPS-007). Not committed, not in the manifest.
	@echo "==> load-corpus"
	@if [ -z "$${LOAD_CORPUS_OUT:-}" ]; then \
	   echo "  FAIL  set LOAD_CORPUS_OUT to a directory with room for the corpus."; \
	   echo "        e.g. make load-corpus LOAD_CORPUS_OUT=/var/tmp/vizra-load LOAD_CORPUS_COUNT=10000"; \
	   exit 1; fi
	@$(GO) run ./cmd/loadcorpusgen \
	   -out "$$LOAD_CORPUS_OUT" \
	   -count "$${LOAD_CORPUS_COUNT:-10000}" \
	   -seed  "$${LOAD_CORPUS_SEED:-1}" \
	   -mix   "$${LOAD_CORPUS_MIX:-12,8,4}"

.PHONY: test
test: ## Unit tests
	@$(GO) test -count=1 $(PKGS)

.PHONY: test-race
test-race: ## Unit tests with the race detector
	@echo "==> test-race"
	@$(GO) test -race -count=1 $(PKGS)

.PHONY: test-integration
test-integration: ## Tests needing PostgreSQL and a RESP server (VIZRA_TEST_DATABASE_URL, VIZRA_TEST_CACHE_URL)
	@echo "==> test-integration"
	@if [ -z "$${VIZRA_TEST_DATABASE_URL:-}" ]; then \
	   echo "  FAIL  VIZRA_TEST_DATABASE_URL is not set. This lane is BLOCKED, not passed."; \
	   exit 1; fi
	@$(GO) test -race -count=1 -tags=integration ./...

.PHONY: test-integration-shuffle
test-integration-shuffle: ## The same suite in a RANDOM order, so order dependence cannot hide
	@echo "==> test-integration-shuffle"
	@# Verifier FINDING V-1 was an order- and timing-dependent failure that the
	@# fixed source order happened to expose only sometimes. Running the suite
	@# in a random permutation on every CI run is what stops the next one being
	@# discovered by a builder at 2am instead of by CI. `go test` prints
	@# `-test.shuffle <seed>` as the first line of a FAILING package's output
	@# (verified 2026-09-20 on go1.27.1; a green run prints no seed), so a
	@# failing permutation is reproducible with -shuffle=<seed>.
	@if [ -z "$${VIZRA_TEST_DATABASE_URL:-}" ]; then \
	   echo "  FAIL  VIZRA_TEST_DATABASE_URL is not set. This lane is BLOCKED, not passed."; \
	   exit 1; fi
	@$(GO) test -race -count=1 -shuffle=on -tags=integration ./...

.PHONY: govulncheck
govulncheck: ## Known vulnerabilities in the dependency set
	@echo "==> govulncheck"
	@# v1.8.0, verified on the live proxy 2026-09-20 and run against this tree.
	@# v1.1.4 panics on the Go 1.27 AST ("unexpected expr: *ast.KeyValueExpr"),
	@# which is a TOOL failure, not a clean scan — and a lane that crashes is not
	@# a pass.
	@$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

.PHONY: tidy-check
tidy-check: ## go.mod and go.sum are tidy
	@echo "==> tidy-check"
	@$(GO) mod tidy -diff

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

.PHONY: build
build: ## Build all three entry points into bin/
	@mkdir -p bin
	@$(GO) build -ldflags "$(LDFLAGS)" -o bin/vizra-api     ./cmd/api
	@$(GO) build -ldflags "$(LDFLAGS)" -o bin/vizra-worker  ./cmd/worker
	@$(GO) build -ldflags "$(LDFLAGS)" -o bin/vizra         ./cmd/vizra
	@ls -la bin/

.PHONY: clean
clean:
	@rm -rf bin
