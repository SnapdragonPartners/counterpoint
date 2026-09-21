package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

// writeUsage prints the recorded token usage, newest round first within
// each workflow, workflows in key order so the output is stable.
//
// It reports completed rounds only, and says so: a review that failed or
// timed out still spent tokens and is not recorded, so the totals here are
// a floor on what was spent, not the whole bill. Rounds reviewed before
// usage was tracked, or whose app-server reported none, print as not
// recorded rather than as zero.
//
// Figures are labelled as the app-server reported them and never as a cost
// measured by Counterpoint. The schema does not define whether a last report covers a
// whole turn or only its final model request, so a per-round cost derived
// from one would assert something unestablished.
func writeUsage(w io.Writer, st *state.State, path string) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "Counterpoint token usage from %s\n", path)
	b.WriteString("Completed rounds only; failed and interrupted rounds are not recorded.\n")
	b.WriteString("Every figure is what the app-server reported, not a cost Counterpoint measured.\n")

	keys := make([]string, 0, len(st.Workflows))
	for k := range st.Workflows {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sum int64
	var saturated bool
	var recorded int
	for _, k := range keys {
		wf := st.Workflows[k]
		fmt.Fprintf(b, "\n%s\n", k)
		for _, r := range rounds(wf) {
			if r.usage == nil {
				fmt.Fprintf(b, "  round %-4d %s  not recorded\n", r.round, short(r.commit))
				continue
			}
			l := r.usage.Last
			sum, saturated = addSaturating(sum, l.Total, saturated)
			recorded++
			fmt.Fprintf(b, "  round %-4d %s  last report %s tokens  (input %s, cached %s, output %s, reasoning %s)\n",
				r.round, short(r.commit), group(l.Total), group(l.Input), group(l.Cached), group(l.Output), group(l.Reasoning))
		}
		if wf.LastUsage != nil {
			fmt.Fprintf(b, "  thread total reported at round %d: %s tokens\n", wf.Round, group(wf.LastUsage.Total.Total))
		}
	}

	if len(keys) == 0 {
		b.WriteString("\nNo workflows recorded yet.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	fmt.Fprintf(b, "\n%d workflow(s), %d recorded round(s).\n", len(keys), recorded)
	if saturated {
		// Counters come from the app-server and only non-negativity is
		// validated, so a report large enough to overflow the sum is
		// reachable. Printing a wrapped negative total would be worse
		// than saying the sum cannot be represented.
		b.WriteString("Sum of last reports: more than can be represented; at least one reported counter is implausibly large.\n")
	} else {
		fmt.Fprintf(b, "Sum of last reports: %s tokens.\n", group(sum))
	}
	// The app-server schema does not say whether a last report covers a
	// whole turn or only its final model request. Presenting the sum as
	// round-by-round cost would assert the first; saying so would be a
	// claim this has not established.
	b.WriteString("A last report may cover only the final model request of a turn rather than\n")
	b.WriteString("the whole turn, so treat these as reported figures and the sum as a lower\n")
	b.WriteString("bound, not as measured round costs.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// round is one printable round: the newest review, then the retained
// history, newest first.
type round struct {
	round  int
	commit string
	usage  *state.Usage
}

func rounds(wf state.Workflow) []round {
	out := []round{{round: wf.Round, commit: wf.LastCommit, usage: wf.LastUsage}}
	for i := len(wf.History) - 1; i >= 0; i-- {
		h := wf.History[i]
		out = append(out, round{round: h.Round, commit: h.Commit, usage: h.Usage})
	}
	return out
}

// short abbreviates an object id for display; anything that is not one is
// printed as it is, since the state file is untrusted and this is a report.
func short(commit string) string {
	if state.IsObjectID(commit) {
		return commit[:12]
	}
	return commit
}

// addSaturating returns a+b, clamped at math.MaxInt64 rather than wrapping,
// and whether the clamp has happened. Both values are non-negative: the
// state file's validation guarantees that much and nothing more, so a
// counter near the maximum is untrusted input rather than an impossibility.
func addSaturating(a, b int64, already bool) (int64, bool) {
	if b > math.MaxInt64-a {
		return math.MaxInt64, true
	}
	return a + b, already
}

// group renders n with thousands separators, which is the difference
// between reading a token count and counting its digits.
func group(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
