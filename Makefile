.PHONY: build install test lint vet fmt fmt-check check install-lint install-hooks schema clean

GOLANGCI_LINT_VERSION := v1.64.8
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Tools are installed into a repository-local, gitignored directory at an
# exact version, so local and CI lint runs use the same binary regardless of
# what is on PATH. The version is part of the file name so a bump installs
# fresh instead of reusing a stale binary.
TOOLS_DIR := $(CURDIR)/bin/tools
GOLANGCI_LINT := $(TOOLS_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/counterpoint ./cmd/counterpoint

# The installed binary is the stable tool MCP clients run; bin/counterpoint is
# the build under test. Installing promotes deliberately. go install chooses
# the destination: GOBIN when set, otherwise the first GOPATH entry's bin, the
# conventional PATH entry for Go tools. go list reports that same path.
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/counterpoint
	@echo "Installed $(VERSION) to $$(go list -f '{{.Target}}' ./cmd/counterpoint)"

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

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

# Everything CI runs, in the same order.
check: fmt-check vet lint test

install-lint: $(GOLANGCI_LINT)

$(GOLANGCI_LINT):
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION) into $(TOOLS_DIR)..."
	@mkdir -p $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(TOOLS_DIR)/golangci-lint $(GOLANGCI_LINT)

# Resolves the hooks directory through Git so linked worktrees and a
# configured core.hooksPath work. Fails loudly when it cannot install.
install-hooks:
	@hooks="$$(git rev-parse --git-path hooks 2>/dev/null)"; \
	if [ -z "$$hooks" ]; then echo "not a git repository; hooks not installed" >&2; exit 1; fi; \
	mkdir -p "$$hooks"; \
	if [ ! -w "$$hooks" ]; then echo "hooks directory $$hooks is not writable; hooks not installed" >&2; exit 1; fi; \
	cp hooks/pre-commit "$$hooks/pre-commit" && chmod +x "$$hooks/pre-commit" \
		&& echo "git hooks installed into $$hooks"

# Regenerate the codex app-server JSON schema from the installed CLI into the
# gitignored .schema/ directory so protocol claims can be checked against the
# version actually in use.
schema:
	scripts/gen-schema.sh

clean:
	rm -rf bin
