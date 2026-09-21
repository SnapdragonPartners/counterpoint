package main

import (
	"bytes"
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
		"22,644 tokens",
		"round 2",
		"19,100 tokens",
		// A round recorded before usage was tracked is unknown, not zero.
		"round 1",
		"not recorded",
		"thread total as reported by the app-server: 61,744",
		// Only the two recorded rounds are summed.
		"41,744 tokens across 2 recorded round(s)",
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
