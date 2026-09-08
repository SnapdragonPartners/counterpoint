# Reviewer instructions for Counterpoint

- `docs/MVP.md` is the accepted contract; `CLAUDE.md` holds the engineering
  invariants. Check changes against both, and flag any divergence between
  code and specification rather than resolving it silently.
- Verification is `make check` (gofmt, vet, golangci-lint, `go test -race`).
  In a build-capable review run it from the checkout with
  `GOCACHE=$COUNTERPOINT_CACHE_DIR/go-build GOPROXY=off`; golangci-lint is
  not available offline, so report lint as not run and let CI cover it.
- Never run live Codex integration tests or anything that needs credentials.
- Weight findings toward the durable invariants: atomic state, serialized
  workflows, untrusted input at every boundary, argument arrays for Git and
  Codex, bounded reads and buffers, explicit responses to every
  server-originated request, and no edits to the reviewed repository.
- A regression test that cannot fail for the defect it names is a finding.
