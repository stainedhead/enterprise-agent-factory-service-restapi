.PHONY: all build test race vet lint fmt fmt-check tidy vuln cover gate clean

VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
BINS    := afs-api afs-reconciler afs-gateway afs-billing

GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.8.0

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

# Build golangci-lint with this module's toolchain (go.mod), so it can analyze Go 1.27 code.
lint:
	GOTOOLCHAIN=$(shell go env GOVERSION) go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

tidy:
	go mod tidy

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Coverage gate: domain, usecase and authz packages must stay at or above 90%.
cover:
	go test -cover ./internal/domain/... ./internal/usecase/... ./internal/authz/...

# The full quality gate. Must pass before every commit.
gate: tidy fmt-check vet lint race build
	./bin/afs-api --version

clean:
	$(RM) -r ./bin ./dist
