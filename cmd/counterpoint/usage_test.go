package main

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

func oid(c string) string { return strings.Repeat(c, 40) }

func TestWriteUsageReportsRecordedRounds(t *testing.T) {
	u := func(last, total int64) *state.Usage {
		return &state.Usage{
			Last:  state.UsageBreakdown{Input: last, Cached: last / 2, Output: last / 5, Reasoning: last / 10, Total: last},
			Total: state.UsageBreakdown{Total: total},
		}
	}
	st := &state.State{Workflows: map[string]state.Workflow{
		"/repo/.git::refs/heads/b": {
			Round: 3, LastCommit: oid("a"), LastReview: "r", LastUsage: u(22644, 61744),
			History: []state.HistoryRecord{
				{Round: 1, Commit: oid("c"), Base: oid("d"), Review: "r"},
				{Round: 2, Commit: oid("b"), Base: oid("d"), Review: "r", Usage: u(19100, 39100)},
			},
		},
	}}
	var out bytes.Buffer
	if err := writeUsage(&out, st, "/s/state.json"); err != nil {
		t.Fatalf("writeUsage: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"/s/state.json",
		"Completed rounds only",
		"/repo/.git::refs/heads/b",
		"round 3",
		"last report 22,644 tokens",
		"round 2",
		"last report 19,100 tokens",
		// A round recorded before usage was tracked is unknown, not zero.
		"round 1",
		"not recorded",
		"thread total reported at round 3: 61,744",
		// Only the two recorded rounds are summed.
		"2 recorded round(s)",
		"Sum of last reports: 41,744 tokens",
		// The sum must not be presented as a measured round cost: the
		// scope of a last report is not established, so the caveat is
		// part of the contract, not decoration.
		"may cover only the final model request",
		"lower",
		"not as measured round costs",
		"not a cost Counterpoint measured",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// Newest round first within the workflow.
	if i, j := strings.Index(got, "round 3"), strings.Index(got, "round 2"); i > j {
		t.Errorf("rounds are not newest first:\n%s", got)
	}
	// Commits are abbreviated, not printed whole.
	if strings.Contains(got, oid("a")) {
		t.Errorf("full object id in the report:\n%s", got)
	}
}

func TestWriteUsageWithNoWorkflows(t *testing.T) {
	var out bytes.Buffer
	if err := writeUsage(&out, &state.State{}, "/s/state.json"); err != nil {
		t.Fatalf("writeUsage: %v", err)
	}
	if !strings.Contains(out.String(), "No workflows recorded yet.") {
		t.Errorf("empty state report:\n%s", out.String())
	}
}

// The state file is untrusted, so a commit that is not an object id is
// printed as it is rather than sliced, which would panic on a short value.
func TestWriteUsageToleratesAMalformedCommit(t *testing.T) {
	st := &state.State{Workflows: map[string]state.Workflow{
		"w": {Round: 1, LastCommit: "xy", LastReview: "r"},
	}}
	var out bytes.Buffer
	if err := writeUsage(&out, st, "/s/state.json"); err != nil {
		t.Fatalf("writeUsage: %v", err)
	}
	if !strings.Contains(out.String(), "xy") {
		t.Errorf("short commit not printed:\n%s", out.String())
	}
}

func TestGroupSeparatesThousands(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0", 1: "1", 999: "999", 1000: "1,000", 22644: "22,644",
		1000000: "1,000,000", -1234: "-1,234",
	} {
		if got := group(in); got != want {
			t.Errorf("group(%d) = %q, want %q", in, got, want)
		}
	}
}

// Counters come from the app-server and only non-negativity is validated,
// so a sum large enough to overflow is reachable from reports that pass
// every check. Wrapping would print a negative token count.
func TestWriteUsageDoesNotOverflowTheSum(t *testing.T) {
	huge := state.UsageBreakdown{Total: math.MaxInt64}
	st := &state.State{Workflows: map[string]state.Workflow{
		"a": {Round: 1, LastCommit: oid("a"), LastReview: "r", LastUsage: &state.Usage{Last: huge, Total: huge}},
		"b": {Round: 1, LastCommit: oid("b"), LastReview: "r", LastUsage: &state.Usage{Last: huge, Total: huge}},
	}}
	// The premise: these records are valid, so the report must cope.
	for k, wf := range st.Workflows {
		if bad := wf.InvalidHistory(); bad != "" {
			t.Fatalf("%s is not valid, so this does not test what it claims: %s", k, bad)
		}
	}
	var out bytes.Buffer
	if err := writeUsage(&out, st, "/s/state.json"); err != nil {
		t.Fatalf("writeUsage: %v", err)
	}
	got := out.String()
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "Sum of last reports") && strings.Contains(line, "-") {
			t.Errorf("wrapped to a negative total: %s", line)
		}
	}
	if !strings.Contains(got, "more than can be represented") {
		t.Errorf("saturation not reported:\n%s", got)
	}
}

func TestAddSaturating(t *testing.T) {
	if got, sat := addSaturating(2, 3, false); got != 5 || sat {
		t.Errorf("addSaturating(2,3) = %d, %v", got, sat)
	}
	if got, sat := addSaturating(math.MaxInt64, 1, false); got != math.MaxInt64 || !sat {
		t.Errorf("addSaturating(max,1) = %d, %v", got, sat)
	}
	// A later ordinary add must not clear a clamp that already happened.
	if _, sat := addSaturating(1, 1, true); !sat {
		t.Error("saturation flag cleared by a later in-range add")
	}
}
