.PHONY: build build-all clean install test lint fmt vet help

# Variables
BINARIES=infrahub-backup infrahub-environment infrahub-taskmanager infrahub-version
BUILD_DIR=$(shell pwd)/bin
SRC_ROOT=./src
VERSION?=1.0.0
LDFLAGS=-ldflags "-X main.version=$(VERSION) -s -w"

# Default target
help: ## Display this help message
	@echo "Infrahub Operations Tool - Build System"
	@echo ""
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

build: ## Build all CLI binaries for the current platform
	@echo "Building Infrahub CLI binaries..."
	@mkdir -p $(BUILD_DIR)
	@for bin in $(BINARIES); do \
		echo "  $$bin"; \
		go build $(LDFLAGS) -o $(BUILD_DIR)/$$bin $(SRC_ROOT)/cmd/$$bin; \
	done
	@echo "Binaries available in $(BUILD_DIR)"

build-all: ## Build for multiple platforms
	@echo "Building multi-platform binaries..."
	@mkdir -p $(BUILD_DIR)
	@for bin in $(BINARIES); do \
		for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
			OS=$${platform%/*}; \
			ARCH=$${platform#*/}; \
			EXT=$$( [ "$$OS" = "windows" ] && echo ".exe" ); \
			OUT=$(BUILD_DIR)/$$bin-$$OS-$$ARCH$$EXT; \
			echo "  $$bin ($$OS/$$ARCH)"; \
			GOOS=$$OS GOARCH=$$ARCH go build $(LDFLAGS) -o "$$OUT" $(SRC_ROOT)/cmd/$$bin; \
		done; \
	done
	@echo "Built binaries are located in $(BUILD_DIR)"

install: ## Install the binaries to $GOPATH/bin
	@echo "Installing Infrahub CLI binaries..."
	@for bin in $(BINARIES); do \
		echo "  $$bin"; \
		go install $(LDFLAGS) $(SRC_ROOT)/cmd/$$bin; \
	done
	@echo "Binaries installed to $(shell go env GOPATH)/bin/"

clean: ## Clean build artifacts
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@go clean

test: ## Run tests
	@echo "Running tests..."
	@go test -v ./...

test-coverage: ## Run tests with coverage
	@echo "Running tests with coverage..."
	@go test -v -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

lint: ## Run golangci-lint
	@echo "Running linter..."
	@golangci-lint run

fmt: ## Format Go code
	@echo "Formatting code..."
	@go fmt ./...

vet: ## Run go vet
	@echo "Running go vet..."
	@go vet ./...

deps: ## Download dependencies
	@echo "Downloading dependencies..."
	@go mod download
	@go mod tidy

deps-update: ## Update dependencies
	@echo "Updating dependencies..."
	@go get -u ./...
	@go mod tidy

run-example: build ## Run example commands
	@echo "Running example commands..."
	@echo "1. Environment detection:"
	@./$(BUILD_DIR)/infrahub-environment detect || true
	@echo ""
	@echo "2. Backup help:"
	@./$(BUILD_DIR)/infrahub-backup --help || true
	@echo ""
	@echo "3. Version information:"
	@./$(BUILD_DIR)/infrahub-version || true

dev-setup: ## Set up development environment
	@echo "Setting up development environment..."
	@go mod download
	@which golangci-lint > /dev/null || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $$(go env GOPATH)/bin v1.54.2
	@echo "Development environment ready!"

docker-build: ## Build Docker image
	@echo "Building Docker image..."
	@docker build -t infrahub-ops:$(VERSION) -t infrahub-ops:latest .

release: test lint build-all ## Prepare release
	@echo "Preparing release $(VERSION)..."
	@echo "All binaries built and tests passed"

.DEFAULT_GOAL := help
