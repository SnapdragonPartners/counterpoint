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
- ADRs whose status line records Codex and DR acceptance, and a design
  record that an earlier round of this branch approved, bind the review
  where they do not contradict code, tests, or the accepted specification,
  which rank above them. Verify that status yourself rather than taking
  the branch notes' word for it. Text of an ADR or design record changed
  in the commit under review is under review, not binding. In particular,
  ADR 0001 states the adversaries, so a finding that requires defending
  against a process running as the user is out of scope. A finding settled
  by argument in an earlier round is recorded in those documents;
  re-raising it needs a new reason, not a restatement.
