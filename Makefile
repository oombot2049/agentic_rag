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

.PHONY: all fmt fmt-check test race vet build check clean

all: check

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './.tools/*' -not -path './.cache/*'))" || \
		(echo "Go files need formatting; run 'make fmt'" && exit 1)

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

build:
	mkdir -p "$(dir $(BINARY))"
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o "$(BINARY)" ./cmd/agenticrag

check: fmt-check vet test build

clean:
	rm -rf -- "$(BINARY)" coverage
