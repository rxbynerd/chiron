# Chiron task runner. Run `just --list` for an overview.
#
# `just ci` runs the same checks, in the same read-only form, as
# .github/workflows/ci.yml. `fmt` and `build` write to the tree; `fmt-check`
# never does.

version_pkg := "github.com/rxbynerd/chiron/internal/version"
buf := "go run github.com/bufbuild/buf/cmd/buf@v1.75.0"
govulncheck := "go run golang.org/x/vuln/cmd/govulncheck@v1.9.0"

# Build the chiron binary into ./bin, stamped with its git version.
build:
    go build -ldflags "-X {{version_pkg}}.Version=$(git describe --tags --always --dirty)" -o bin/chiron ./cmd/chiron

# Run all tests, shuffled, with the race detector.
test:
    go test -race -shuffle=on ./...

# Run all tests and print total statement coverage (writes coverage.out).
cover:
    go test -race -shuffle=on -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | tail -1

# Vet the whole module.
vet:
    go vet ./...

# Fail unless golangci-lint is installed; warn when it is not the CI version.
[private]
require-golangci-lint:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! command -v golangci-lint >/dev/null 2>&1; then
        echo "golangci-lint not found; install the version pinned in .github/workflows/ci.yml: https://golangci-lint.run/docs/welcome/install/local/" >&2
        exit 1
    fi
    pinned=$(grep -oE 'version: v[0-9.]+' .github/workflows/ci.yml | head -n 1 | cut -d' ' -f2) || true
    if [ -z "${pinned}" ]; then
        echo "cannot find the golangci-lint pin in .github/workflows/ci.yml" >&2
        exit 1
    fi
    installed="v$(golangci-lint version --short)"
    echo "golangci-lint ${installed} (CI pins ${pinned})"
    if [ "${installed}" != "${pinned}" ]; then
        echo "warning: installed golangci-lint differs from the version CI pins" >&2
    fi

# Lint with golangci-lint; fails when it is not installed.
lint: require-golangci-lint
    golangci-lint run ./...

# Fail on any formatting difference without touching files.
fmt-check: require-golangci-lint
    golangci-lint fmt --diff ./...

# Apply gofmt and goimports in place. WRITES to tracked files.
fmt: require-golangci-lint
    golangci-lint fmt ./...

# Fail when go.mod or go.sum need tidying, without touching files.
tidy-check:
    go mod tidy -diff

# Fail when the chiron binary links a test-only package.
binary-deps-check:
    ./scripts/check-binary-deps.sh

# Fail when the code reaches a known vulnerability (needs network).
vuln:
    {{govulncheck}} ./...

# Everything CI runs, in its read-only form.
ci: require-golangci-lint build binary-deps-check vet test fmt-check lint tidy-check proto-lint vuln

# Lint and compile-check the proto/chiron/v1 Buf module.
proto-lint:
    cd proto && {{buf}} lint && {{buf}} build

# Generate Go from proto/chiron/v1 (pinned remote plugins; needs network).
# Output is not committed in v1 — see docs/DECISIONS.md.
proto:
    cd proto && buf generate
