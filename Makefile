# GoDrop — development shortcuts.
BINARY  := godrop
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./bin
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/godrop
	@echo "built bin/$(BINARY) $(VERSION)"

.PHONY: test
test: ## Run every test with the race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and report coverage, failing below 100%
	go test -covermode=atomic -coverprofile=coverage.out ./internal/...
	@go tool cover -func=coverage.out | tail -n 1
	@go tool cover -func=coverage.out | grep -v "100.0%" || true
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {print $$3}' | tr -d '%'); \
	 [ "$$(echo "$$total >= 100" | bc -l)" = "1" ] || { echo "coverage is $$total%, want 100%"; exit 1; }

.PHONY: cover-html
cover-html: ## Open the coverage report in a browser
	go test -coverprofile=coverage.out ./internal/... >/dev/null
	go tool cover -html=coverage.out

.PHONY: fuzz
fuzz: ## Fuzz the input sanitisers for a minute each
	go test ./internal/server/ -run "^$$" -fuzz FuzzSanitizeExt -fuzztime 60s
	go test ./internal/server/ -run "^$$" -fuzz FuzzSanitizeSlug -fuzztime 60s
	go test ./internal/server/ -run "^$$" -fuzz FuzzSplitStoredName -fuzztime 60s

.PHONY: lint
lint: ## Vet, check formatting and lint the installer
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	 [ -z "$$unformatted" ] || { echo "not gofmt'd:"; echo "$$unformatted"; exit 1; }
	@command -v shellcheck >/dev/null && shellcheck install.sh || echo "shellcheck not installed, skipping"

.PHONY: run
run: ## Run a development server on port 48080 with a throwaway token
	GODROP_TOKENS=dev-token-with-enough-entropy \
	GODROP_DATA_DIR=./data \
	GODROP_ADDR=127.0.0.1:48080 \
	GODROP_LOG_FORMAT=text \
	GODROP_TELEMETRY=off \
	go run ./cmd/godrop serve

.PHONY: snapshot
snapshot: ## Build every release artefact locally, without publishing
	POSTHOG_KEY="" POSTHOG_HOST="https://eu.i.posthog.com" goreleaser release --snapshot --clean
	@ls -1 dist/*.tar.gz dist/*.zip dist/*.deb dist/*.rpm dist/*.apk

.PHONY: docker
docker: ## Build the container image locally
	docker build -t godrop:dev --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

.PHONY: clean
clean: ## Remove build output and local data
	rm -rf bin dist coverage.out data
