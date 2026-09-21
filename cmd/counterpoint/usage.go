package main

import (
	"fmt"
	"io"
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
func writeUsage(w io.Writer, st *state.State, path string) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "Counterpoint token usage from %s\n", path)
	b.WriteString("Completed rounds only; failed and interrupted rounds are not recorded.\n")

	keys := make([]string, 0, len(st.Workflows))
	for k := range st.Workflows {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var grand int64
	var recorded int
	for _, k := range keys {
		wf := st.Workflows[k]
		fmt.Fprintf(b, "\n%s\n", k)
		for _, r := range rounds(wf) {
			switch {
			case r.usage == nil:
				fmt.Fprintf(b, "  round %-4d %s  not recorded\n", r.round, short(r.commit))
			default:
				l := r.usage.Last
				grand += l.Total
				recorded++
				fmt.Fprintf(b, "  round %-4d %s  %s tokens  (input %s, cached %s, output %s, reasoning %s)\n",
					r.round, short(r.commit), group(l.Total), group(l.Input), group(l.Cached), group(l.Output), group(l.Reasoning))
			}
		}
		if wf.LastUsage != nil {
			fmt.Fprintf(b, "  thread total as reported by the app-server: %s tokens\n", group(wf.LastUsage.Total.Total))
		}
	}

	if len(keys) == 0 {
		b.WriteString("\nNo workflows recorded yet.\n")
	} else {
		fmt.Fprintf(b, "\n%d workflow(s); %s tokens across %d recorded round(s).\n", len(keys), group(grand), recorded)
	}
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
