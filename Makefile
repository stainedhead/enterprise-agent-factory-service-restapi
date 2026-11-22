.PHONY: all build test race vet vet-tags lint fmt fmt-check tidy vuln cover gate clean \
	contract contract-check openapi openapi-check test-db envtest e2e load conformance

VERSION ?= 1.0.1
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
BINS    := afs-api afs-reconciler afs-gateway afs-billing afs-edge

GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.8.0

# Root repository checkout holding docs/contracts and the root PRD (contract extraction source).
AFS_ROOT_DIR ?= ../enterprise-agent-factory-service
# Coverage threshold for domain, usecase and authz packages.
COVER_MIN  ?= 90
COVER_PKGS := ./internal/domain/... ./internal/usecase/... ./internal/authz/...

all: gate

build:
	@mkdir -p bin
	@for b in $(BINS); do CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; done

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

# Build-tagged suites (they need a cluster, a dev account or envtest assets) must still compile.
vet-tags:
	go vet -tags e2e,load,conformance,envtest ./...

# Build golangci-lint with this module's toolchain (go.mod), so it can analyze Go 1.27 code.
lint:
	GOTOOLCHAIN=$(shell go env GOVERSION) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --build-tags envtest

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

tidy:
	go mod tidy

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Coverage gate: every domain, usecase and authz package must stay at or above COVER_MIN percent.
cover:
	go run ./tools/covercheck -min $(COVER_MIN) $(COVER_PKGS)

# Regenerate api/contract/*.json and the generated Go files from the root contract (AFS_ROOT_DIR).
contract:
	go run ./tools/contractgen -root $(AFS_ROOT_DIR)

# Fails when the committed extract differs from the root contract; skipped without a root checkout.
contract-check:
	go run ./tools/contractgen -root $(AFS_ROOT_DIR) -check -skip-missing

# Bundle api/src/** into api/openapi.yaml.
openapi:
	go run ./tools/openapibundle -src api/src -out api/openapi.yaml

# Fails when api/openapi.yaml is not the bundle of api/src/**.
openapi-check:
	go run ./tools/openapibundle -src api/src -out api/openapi.yaml -check

# Integration tests against a real PostgreSQL. AFS_TEST_DATABASE_URL must be set in the environment.
test-db:
	@test -n "$$AFS_TEST_DATABASE_URL" || { echo "AFS_TEST_DATABASE_URL is not set"; exit 1; }
	go test -race -count=1 ./internal/adapter/postgres/... ./test/contract/...

envtest:
	go test -tags envtest -count=1 ./...

e2e:
	go test -tags e2e -count=1 ./test/e2e/...

load:
	go test -tags load -count=1 ./test/load/...

conformance:
	go test -tags conformance -count=1 ./test/conformance/...

# The full quality gate. Must pass before every commit.
gate: tidy fmt-check vet vet-tags openapi-check contract-check lint race build
	./bin/afs-api --version

clean:
	$(RM) -r ./bin ./dist
