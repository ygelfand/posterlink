.DEFAULT_GOAL := help

BINARY_NAME=posterlink
VERSION ?= 0.0.0
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -X 'github.com/ygelfand/posterlink/internal/config.Version=$(VERSION)' \
	-X 'github.com/ygelfand/posterlink/internal/config.GitCommit=$(GIT_COMMIT)' \
	-X 'github.com/ygelfand/posterlink/internal/config.BuildDate=$(BUILD_DATE)'

##@ Development

.PHONY: build
build: ## Build the posterlink binary into ./bin
	go fmt ./...
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY_NAME) .

.PHONY: run
run: ## Run posterlink directly (e.g. make run ARGS="serve -v")
	go run . $(ARGS)

.PHONY: serve
serve: ## Run the server against ./posterlink.yaml
	go run . serve --config posterlink.yaml -v

.PHONY: test
test: ## Run tests
	go test -v ./...

.PHONY: fmt
fmt: ## Format Go source
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	go mod tidy

##@ Build & Release

.PHONY: install
install: ## go install posterlink into $$GOPATH/bin
	go install -ldflags "$(LDFLAGS)" .

.PHONY: docker-build
docker-build: ## Build the container image locally
	docker build -t $(BINARY_NAME):$(VERSION) --build-arg VERSION=$(VERSION) .

.PHONY: snapshot
snapshot: ## Build a local goreleaser snapshot (no publish)
	goreleaser build --clean --snapshot

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/

##@ Help

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
