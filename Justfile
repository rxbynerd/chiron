# Chiron task runner. Run `just --list` for an overview.

# Build the chiron binary into ./bin.
build:
    go build -o bin/chiron ./cmd/chiron

# Run all tests.
test:
    go test ./...

# Vet the whole module.
vet:
    go vet ./...

# Lint with golangci-lint if installed; fall back to go vet.
lint:
    @if command -v golangci-lint >/dev/null 2>&1; then \
        golangci-lint run ./...; \
    else \
        echo "golangci-lint not found; running go vet instead"; \
        go vet ./...; \
    fi

# Everything CI runs.
ci: build vet test lint
