.PHONY: build test lint vet fmt fmt-check check install-lint install-hooks schema clean

GOLANGCI_LINT_VERSION := v1.64.8
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Directory for the app-server schema regenerated from the installed Codex CLI.
# Gitignored: the bundle is large and is regenerated per developer machine.
SCHEMA_DIR := .schema

build:
	go build -ldflags "$(LDFLAGS)" -o bin/counterpoint ./cmd/counterpoint

test:
	go test -race -cover ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(git ls-files '*.go')

# Fails when any tracked Go file is not gofmt-clean.
fmt-check:
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

lint: install-lint
	golangci-lint run

# Everything CI runs, in the same order.
check: fmt-check vet lint test

install-lint:
	@which golangci-lint > /dev/null || { \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	}
	@golangci-lint version 2>/dev/null | grep -q "$(GOLANGCI_LINT_VERSION)" || \
		echo "warning: golangci-lint on PATH is not $(GOLANGCI_LINT_VERSION); results may differ from CI"

# Non-fatal for read-only checkouts and CI.
install-hooks:
	@if [ -d .git ] && [ -w .git/hooks ]; then \
		cp hooks/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit; \
		echo "git hooks installed"; \
	fi

# Regenerate the codex app-server JSON schema from the installed CLI so
# protocol claims can be checked against the version actually in use.
schema:
	scripts/gen-schema.sh $(SCHEMA_DIR)

clean:
	rm -rf bin
