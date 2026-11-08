VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X github.com/amirkode/git-keepr/internal/cli.Version=$(VERSION) -X github.com/amirkode/git-keepr/internal/cli.Commit=$(COMMIT)

.PHONY: fmt fmt-check test test-race vet check build dist docker-build test-deploy test-docker shellcheck clean
fmt:
	go fmt ./...
fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)
test:
	go test ./...
test-race:
	go test -race ./...
vet:
	go vet ./...
check: fmt-check vet test-race shellcheck
build:
	mkdir -p dist && CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/git-keepr ./cmd/git-keepr
# Static Linux binaries named for scripts/install.sh --binary.
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/git-keepr-linux-amd64 ./cmd/git-keepr
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/git-keepr-linux-arm64 ./cmd/git-keepr
docker-build:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t git-keepr:local .
shellcheck:
	shellcheck scripts/install.sh tests/deploy/*.sh
# Installer idempotency test in a throwaway container (needs docker or podman).
test-deploy: dist
	tests/deploy/run-install-test.sh
# Image + non-root + writable-volume + graceful-stop smoke test (needs docker or podman).
test-docker: docker-build
	tests/deploy/docker-smoke.sh
clean:
	rm -rf dist
