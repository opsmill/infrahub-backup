.PHONY: build clean install test lint fmt vet help

# Variables
BINARY_NAME=infrahub-ops
BUILD_DIR=bin
VERSION?=1.0.0
LDFLAGS=-ldflags "-X main.version=$(VERSION)"

# Default target
help: ## Display this help message
	@echo "Infrahub Operations Tool - Build System"
	@echo ""
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

build: ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	@go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .
	@echo "Binary built: $(BUILD_DIR)/$(BINARY_NAME)"

build-all: ## Build for multiple platforms
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	@GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 .
	@GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 .
	@GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 .
	@GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 .
	@GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe .
	@GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-arm64.exe .
	@echo "Built binaries:"
	@ls -la $(BUILD_DIR)/

install: build ## Install the binary to $GOPATH/bin
	@echo "Installing $(BINARY_NAME)..."
	@go install $(LDFLAGS) .
	@echo "$(BINARY_NAME) installed to $(shell go env GOPATH)/bin/"

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
	@./$(BUILD_DIR)/$(BINARY_NAME) environment detect || true
	@echo ""
	@echo "2. List projects:"
	@./$(BUILD_DIR)/$(BINARY_NAME) environment list || true
	@echo ""
	@echo "3. Help:"
	@./$(BUILD_DIR)/$(BINARY_NAME) --help

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
