# Configuration file and the reviewer's effort

Design record for
[issue 43](https://github.com/SnapdragonPartners/counterpoint/issues/43).

Status: Proposed (Claude, 2026-09-21)

## Problem

Counterpoint pins the reviewer to `xhigh` and nothing about a run is
adjustable. That was an MVP decision: one deliberate level, chosen by DR,
applied regardless of the user's interactive setting. In use it burns model
capacity fast enough to hit usage caps noticeably sooner, and there is no way
to trade reviewer depth for cost without rebuilding.

Three settings-shaped needs arrived together: the reviewer's effort (this
record), a backend selection once there is more than one reviewer
([issue 42](https://github.com/SnapdragonPartners/counterpoint/issues/42)),
and possibly a spend threshold once usage is measured
([issue 41](https://github.com/SnapdragonPartners/counterpoint/issues/41)).
A third environment variable would answer the first and leave the other two
with nowhere to go, so the file is defined once, now, with the one key that
has a caller.

## Invariants that stay

- The file is the user's own, in the user configuration directory. Nothing
  is read from a reviewed repository, so no author-controlled input reaches
  it. Per-repository policy stays limited to `COUNTERPOINT.md`.
- Existing environment overrides keep working unchanged and keep winning, so
  no installation or test that sets them changes behaviour.
- Every value is validated at startup. A rejected value ends the process with
  an error naming the file, the key, and the accepted set; it never fails
  part-way through a review, when a turn has already been paid for.
- The reviewer's effort stays a deliberate choice rather than the highest
  level a model advertises.

## Design

`os.UserConfigDir()/counterpoint/config.json`, beside the state file, or the
absolute path in `COUNTERPOINT_CONFIG_FILE`. Absent is the normal case and
means the defaults; present means valid.

```json
{
  "review_effort": "high",
  "state_file": "/absolute/path/state.json",
  "checkout_dir": "/absolute/path/checkouts"
}
```

Precedence per value is the environment variable, then the file, then the
built-in default. Resolution stays in the package that owns each value:
`state.ResolvePath` and `scratch.ResolveRoot` take what the file supplied and
apply that order, so the environment rule lives next to the variable it
governs rather than being reimplemented in the loader.

`review_effort` is one of `low`, `medium`, `high`, `xhigh`, and the default
drops from `xhigh` to `high`. The set is Counterpoint's policy, not the
protocol's: the app-server schema types reasoning effort as any non-empty
string the model advertises. The ceiling is deliberate and survives the
change, because some models advertise levels above `xhigh` including one that
enables automatic delegation a reviewer should not adopt implicitly. Raising
it is a policy decision, not a configuration change. `internal/config`
validates against `review.EffortAccepted` rather than keeping its own copy of
the set; the dependency runs from the loader to the policy it enforces, which
is the direction that keeps one definition.

Rejected shapes, each covered by a test: content that is not a JSON object,
including a bare `null`; an unknown key, which would otherwise leave the
user believing a misspelled setting is in force; trailing content after the
object, including a stray closing delimiter; a value of the wrong type; a
key whose value is `null`; an effort outside the accepted set, above the
ceiling, or in the wrong case; a path that is not absolute; a key set to the
empty string, which is a mistake rather than a request for the default; a
file that is not a regular file; and a file over 64 KiB, which is rejected
by size before it is read whole.

Parsing goes key by key through `map[string]json.RawMessage` rather than
into a struct, so the four states a key can be in stay distinct: absent,
`null`, the wrong type, and a usable value. A struct decode collapses the
first three into the zero value, which is what made `{"review_effort":null}`
and a bare `null` silently mean "use the defaults" in round 1.

Trailing content is rejected by requiring the decoder to reach `io.EOF`
after the object, not by `Decoder.More`. `More` reports whether another
element of the current array or object follows, so it is false at a stray
closing delimiter and accepted `{}}` in round 1.

The file's type is checked with `os.Stat` before the open and again on the
descriptor afterwards. `os.Open` on a FIFO with no writer blocks
indefinitely, so a check only on the descriptor is unreachable and a
mistyped path hung startup in round 1. `Stat` follows symlinks, so a
symlinked configuration file still works; the second check catches a path
replaced between the two.

The file is trusted to the same degree as the user, per ADR 0001: the
adversaries are untrusted inputs and the sandboxed reviewer, not a process
running as the user. It is nonetheless fully validated, because the failure
this guards against is a typo.

## Rejected alternatives

**A third environment variable.** Cheapest, consistent with the two that
exist, and it needs no format, precedence order, or validation boundary. It
was rejected because two more settings are already known to be coming and the
MCP client's server registration is an awkward place to accumulate them.

**Per-repository configuration.** Attractive for running several projects at
once, where different work deserves different depth. Rejected for now: a file
inside the reviewed repository is author-controlled input and would need the
bounding, validation, and read-from-the-commit rules `COUNTERPOINT.md` has,
which is a larger design than the one key that has a caller today. Global
first does not foreclose it.

**Making the bounds in "Limits" configurable.** Out of scope and unchanged.
The table stays fixed constants named in the code.

## Tests

`internal/config` covers the accepted shapes and every rejected one above.
`internal/state` and `internal/scratch` cover the precedence order in both
directions, including a configured value losing to the environment and a
relative configured value refused. `internal/review` covers the accepted
effort set, the ceiling, and that a configured effort reaches the service
while an absent one falls back to the default.

Six mutations were run to prove the tests can fail for the defects they
name, each restored afterwards: rejecting unknown keys (the unknown-key case
fails), adding `max` to the accepted set (the ceiling case fails), letting a
configured value beat the environment (the precedence case fails), reverting
the EOF check to `Decoder.More` (the trailing-delimiter cases fail),
accepting `null` as omission (the four null cases fail), and checking the
file type only on the open descriptor (the FIFO case blocks and fails on its
ten-second bound).

## Documentation

`docs/SPEC.md` gains a "Configuration" section and its "Model and reasoning
effort" section records the new default and the ceiling. The out-of-scope
entry that ruled out effort selection is narrowed to the settings that remain
out. `README.md` records the file beside the existing environment variables.
