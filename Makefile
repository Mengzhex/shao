# shao build.
#
# On this machine the Go toolchain lives in a conda environment whose
# conda-forge build ships a trimmed binary at $PREFIX/bin/go.exe with GOROOT
# at $PREFIX/go and no $GOROOT/bin, so GOROOT has to be exported explicitly.
# Override CONDA_ENV_PREFIX, or just GO and GOROOT, to build elsewhere.

CONDA_ENV_PREFIX ?= C:/HUIXIN/AppData/Anaconda/envs/gotmon
export GOROOT     ?= $(CONDA_ENV_PREFIX)/go
GO                ?= $(GOROOT)/bin/go.exe

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.buildVersion=$(VERSION)

BIN     := shao
DIST    := dist

.PHONY: all build probe dist fmt vet test clean help

all: build

## build: build shao for the current platform
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/shao

## probe: build the Linux probe binary to upload to target hosts
#
# It is the same program: sshd invokes it under the name shao-probe, and the
# binary switches to probe mode based on the name it was called as.
probe: dist

## dist: build every release artifact into dist/, plus checksums.txt
#
# Delegated to scripts/release.sh rather than duplicated here, because the
# release workflow runs that same script. Two copies of the target list is
# exactly how the asset names and the README's download URLs would drift
# apart.
dist:
	bash scripts/release.sh $(VERSION)

## crosscheck: compile for every platform without producing binaries
#
# The Windows pseudoconsole path and the POSIX pty path are separate files
# behind build tags, so building on one platform proves nothing about the
# other. This compiles all of them.
crosscheck:
	GOOS=windows GOARCH=amd64 $(GO) build ./...
	GOOS=linux   GOARCH=amd64 $(GO) build ./...
	GOOS=darwin  GOARCH=arm64 $(GO) build ./...

## fmt: format all source
fmt:
	$(GOROOT)/bin/gofmt.exe -w ./cmd ./internal

## vet: run go vet
vet:
	$(GO) vet ./...

## test: run the test suite
test:
	$(GO) test ./...

## clean: remove build output
clean:
	rm -rf $(DIST) $(BIN) $(BIN).exe

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
