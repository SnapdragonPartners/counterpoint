.PHONY: build install register test lint vet fmt fmt-check check install-lint install-hooks schema clean

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

# Registers the installed binary with Claude Code as a user-scope stdio MCP
# server. The registration stores the command name, which Claude Code resolves
# on PATH at each session start, so this is a once-per-machine step that
# survives later installs; it is kept out of install, which CI and contributors
# without Claude Code run. Skipped only when the name is already registered at
# user scope: `claude mcp get` has no scope filter and reports whichever scope
# wins, so its "Scope:" line is checked rather than its exit status, and a
# local- or project-scope server of the same name does not mask a missing
# user-scope one (Claude Code 2.1.261). A local-scope server shadowing an
# existing user-scope one makes the add fail loudly with "already exists in
# user config", never a silent overwrite. Refuses to register a command that
# does not resolve, which would leave a server Claude Code cannot start.
register:
	@if ! command -v claude >/dev/null 2>&1; then \
		echo "claude not found on PATH; install Claude Code first" >&2; exit 1; \
	fi; \
	if ! command -v counterpoint >/dev/null 2>&1; then \
		echo "counterpoint not found on PATH; run make install and put its directory on PATH, or register the absolute path:" >&2; \
		echo "  claude mcp add -s user counterpoint -- $$(go list -f '{{.Target}}' ./cmd/counterpoint)" >&2; \
		exit 1; \
	fi; \
	if claude mcp get counterpoint 2>/dev/null | grep -q '^ *Scope: User config'; then \
		echo "counterpoint is already registered with Claude Code at user scope; nothing to do"; \
	else \
		claude mcp add -s user counterpoint -- counterpoint \
			&& echo "registered counterpoint with Claude Code at user scope; restart Claude Code to pick it up"; \
	fi

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
