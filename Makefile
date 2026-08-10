BIN        := local-apiserver-lb
MODULE     := github.com/cooloo9871/local-apiserver-lb
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE)

GOFLAGS  := -trimpath
PLATFORMS := linux/amd64 linux/arm64

.PHONY: build build-all test vet lint fmt clean install

## build: static binary for the host platform into bin/
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/$(BIN)

## build-all: static binaries for every supported platform into dist/
build-all:
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=dist/$(BIN)-$$os-$$arch; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out ./cmd/$(BIN) || exit 1; \
	done

## test: run all tests with the race detector
test:
	go test -race ./...

## vet: run go vet
vet:
	go vet ./...

## lint: gofmt check + go vet (no third-party linters required)
lint: vet
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofmt: the following files need formatting:"; echo "$$out"; exit 1; \
	fi

## fmt: format all Go sources in place
fmt:
	gofmt -w .

## clean: remove build artifacts
clean:
	rm -rf bin dist

## install: install binary and deploy files onto this machine (needs root)
install: build
	./deploy/install.sh bin/$(BIN)
