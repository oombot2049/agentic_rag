GO ?= go
BINARY ?= bin/agenticrag
VERSION ?= dev
COMMIT ?= unknown
BUILD_DATE ?= unknown

MODULE := github.com/agenticrag/agenticrag
LDFLAGS := -s -w \
	-X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.BuildDate=$(BUILD_DATE)

.PHONY: all fmt fmt-check test integration e2e concurrency eval load-contract race vet staticcheck vuln build check clean

all: check

fmt:
	$(GO) fmt ./...

fmt-check:
	@./ci/check-format.sh

test:
	$(GO) test ./...

integration:
	$(GO) test -tags=integration -count=1 -v ./tests/integration

e2e:
	$(GO) test -tags=e2e -count=1 -v ./tests/e2e

concurrency:
	$(GO) test -race -tags=concurrency -count=1 -v ./tests/concurrency

eval:
	$(GO) run ./evals/cmd/rageval -golden ./evals/testdata/golden/v1/dataset.json -predictions ./evals/testdata/predictions/reference-v1.json -baseline ./evals/testdata/baselines/v1.json
	$(GO) test -count=1 ./evals/...

load-contract:
	$(GO) test -count=1 ./tests/load/...
	$(GO) run ./tests/load/cmd/loadreport -profile ./tests/load/profiles/smoke-v1.json -measurement ./tests/load/testdata/contract-pass.json

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

staticcheck:
	staticcheck ./...

vuln:
	govulncheck ./...

build:
	mkdir -p "$(dir $(BINARY))"
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o "$(BINARY)" ./cmd/agenticrag

check: fmt-check vet test build

clean:
	rm -rf -- "$(BINARY)" coverage
