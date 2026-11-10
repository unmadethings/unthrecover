SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-alpha.dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/unmadethings/unthrecover/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

.PHONY: all build test test-race test-integration test-install lint vuln sec check dist deb image web clean

all: check build

build: ## Build bin/unth
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/unth ./cmd/unth

test: ## Unit tests
	go test ./...

test-race: ## Unit tests with the race detector
	go test -race -count=1 ./...

test-integration: ## End-to-end tests (needs Docker + PostgreSQL client tools)
	UNTH_INTEGRATION_REQUIRED=$${UNTH_INTEGRATION_REQUIRED:-1} go test -tags integration -count=1 -timeout 30m -v ./tests/integration/...

test-install: ## Test scripts/install.sh against locally built artifacts
	sh scripts/test-install.sh

lint: ## go vet + gofmt + staticcheck
	go vet ./...
	go vet -tags integration ./tests/...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

vuln: ## Known-vulnerability scan of dependencies
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

sec: ## Static security analysis
	go run github.com/securego/gosec/v2/cmd/gosec@latest -exclude-dir=tests ./...

check: lint test-race

dist: ## Release binaries + SHA256SUMS in dist/
	rm -rf dist && mkdir -p dist
	for arch in amd64 arm64; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/unth_$(VERSION)_linux_$$arch ./cmd/unth; \
	done
	for arch in amd64 arm64; do sh scripts/build-deb.sh $(VERSION) $$arch >/dev/null; sh scripts/build-rpm.sh $(VERSION) $$arch >/dev/null; done
	sed 's#ghcr.io/unmadethings/unthrecover:v[0-9][^}]*#ghcr.io/unmadethings/unthrecover:$(VERSION)#' deployments/compose/compose.yaml > dist/compose.yaml
	grep -q 'unthrecover:$(VERSION)}' dist/compose.yaml
	cd dist && sha256sum unth_* unthrecover_* compose.yaml > SHA256SUMS

deb: dist ## Debian and RPM packages (built by dist)
rpm: dist

web: ## Build the web panel into internal/web/dist (embedded by go build)
	cd web && npm ci --no-audit --no-fund && npm run build

image: ## Docker image from dist/ (run make dist first)
	docker build --build-arg VERSION=$(VERSION) -t unthrecover:$(VERSION) .

clean:
	rm -rf bin dist
