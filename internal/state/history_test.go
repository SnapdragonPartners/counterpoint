package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// A version 1 file is a version 2 file with empty history: it loads, its
// stored review is intact, and the next save writes version 2.
func TestVersionOneFileMigratesToVersionTwo(t *testing.T) {
	st := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(st.Path()), dirPerm); err != nil {
		t.Fatal(err)
	}
	v1 := `{"version": 1, "workflows": {"k": {"thread_id": "thr_1", "last_commit": "` + oidA + `", "last_base": "` + oidB +
		`", "last_request_hash": "h", "round": 3, "last_review": "verdict three", "last_warnings": ["w"]}}}` + "\n"
	if err := os.WriteFile(st.Path(), []byte(v1), filePerm); err != nil {
		t.Fatal(err)
	}
	s, err := st.Load()
	if err != nil {
		t.Fatalf("Load v1: %v", err)
	}
	wf, ok := s.Get("k")
	if !ok || wf.Round != 3 || wf.LastReview != "verdict three" || len(wf.History) != 0 || wf.InvalidHistory() != "" {
		t.Fatalf("migrated workflow = %+v (invalid %q)", wf, wf.InvalidHistory())
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(st.Path())
	if !strings.Contains(string(data), `"version": 2`) || strings.Contains(string(data), `"history"`) {
		t.Errorf("rewritten file should be version 2 without an empty history:\n%s", data)
	}
	if _, err := NewStore(st.Path()).Load(); err != nil {
		t.Errorf("reload of the migrated file: %v", err)
	}
}

// A version 1 binary checks for exactly its own version, so a version 2
// file is refused there; this package refuses versions it does not know
// in both directions, which is the same guard.
func TestVersionZeroIsRejected(t *testing.T) {
	st := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(st.Path()), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.Path(), []byte(`{"version": 0, "workflows": {}}`+"\n"), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Load error = %v, want ErrUnsupportedVersion", err)
	}
}

func TestHistoryRoundTrips(t *testing.T) {
	st := tempStore(t)
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Workflow{
		ThreadID: "thr_1", LastCommit: oidA, LastBase: oidB, LastRequestHash: "h", Round: 3, LastReview: "three",
		History: []HistoryRecord{
			{Round: 1, Commit: oidA, Base: oidB, Omitted: OmittedTooLarge},
			{Round: 2, Commit: oidA, Base: oidB, Review: "two\nlines"},
		},
	}
	s.Put("k", want)
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	again, err := NewStore(st.Path()).Load()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := again.Get("k")
	if len(got.History) != 2 || got.History[0] != want.History[0] || got.History[1] != want.History[1] {
		t.Errorf("history = %+v, want %+v", got.History, want.History)
	}
}

func TestRetainHistoryBounds(t *testing.T) {
	rec := func(round int, text string) HistoryRecord {
		return HistoryRecord{Round: round, Commit: oidA, Base: oidB, Review: text}
	}

	// Count: the oldest record is evicted once the count bound is hit.
	h := RetainHistory(nil, rec(1, "one"), "two")
	h = RetainHistory(h, rec(2, "two"), "three")
	h = RetainHistory(h, rec(3, "three"), "four")
	if len(h) != MaxHistoryRecords || h[0].Round != 2 || h[1].Round != 3 {
		t.Errorf("after three rounds history = %+v", h)
	}

	// Per-record: an oversized verdict is a placeholder, never truncated.
	big := strings.Repeat("b", MaxHistoryRecordBytes+1)
	h = RetainHistory(nil, NewHistoryRecord(1, oidA, oidB, big), "two")
	if len(h) != 1 || h[0].Review != "" || h[0].Omitted != OmittedTooLarge || h[0].Round != 1 {
		t.Errorf("oversized record = %+v", h)
	}
	exact := strings.Repeat("e", MaxHistoryRecordBytes)
	if r := NewHistoryRecord(1, oidA, oidB, exact); r.Review != exact || r.Omitted != "" {
		t.Errorf("record at the bound was not kept verbatim: omitted=%q len=%d", r.Omitted, len(r.Review))
	}

	// Aggregate: the newest review counts against the byte bound, so a
	// large newest review evicts records that would otherwise fit.
	twenty := strings.Repeat("t", 20<<10)
	h = RetainHistory(nil, rec(1, twenty), twenty)
	h = RetainHistory(h, rec(2, twenty), twenty) // 40 KiB of records + 20 KiB newest > 48 KiB
	if len(h) != 1 || h[0].Round != 2 {
		t.Errorf("aggregate bound not applied with the newest review: %+v", h)
	}
	// A newest review over the per-record bound is quoted as a placeholder
	// and so contributes no bytes.
	h = RetainHistory(nil, rec(1, twenty), big)
	h = RetainHistory(h, rec(2, twenty), big)
	if len(h) != 2 {
		t.Errorf("placeholder newest review should not evict: %+v", h)
	}

	// Empty history stays nil so the field is omitted from the file.
	if h := RetainHistory(nil, rec(1, ""), ""); h != nil && len(h) != 1 {
		t.Errorf("history = %+v", h)
	}
}

func TestInvalidHistoryRejectsEveryBadShape(t *testing.T) {
	valid := func() Workflow {
		return Workflow{Round: 3, LastReview: "three", History: []HistoryRecord{
			{Round: 1, Commit: oidA, Base: oidB, Review: "one"},
			{Round: 2, Commit: oidA, Base: oidB, Omitted: OmittedTooLarge},
		}}
	}
	if msg := valid().InvalidHistory(); msg != "" {
		t.Fatalf("valid history rejected: %s", msg)
	}
	sha256 := strings.Repeat("0123456789abcdef", 4)
	ok := valid()
	ok.History[0].Commit, ok.History[0].Base = sha256, sha256
	if msg := ok.InvalidHistory(); msg != "" {
		t.Fatalf("SHA-256 ids rejected: %s", msg)
	}

	cases := map[string]func(w *Workflow){
		"too many records": func(w *Workflow) {
			w.Round = 4
			w.History = append([]HistoryRecord{{Round: 0, Commit: oidA, Base: oidB, Review: "zero"}}, w.History...)
			for i := range w.History {
				w.History[i].Round = i + 1
			}
		},
		"gap before last review": func(w *Workflow) { w.Round = 4 },
		"gap between records":    func(w *Workflow) { w.History[0].Round = 0; w.Round = 3 },
		"descending rounds":      func(w *Workflow) { w.History[0].Round, w.History[1].Round = 2, 1 },
		"round below one":        func(w *Workflow) { w.Round = 2; w.History[0].Round, w.History[1].Round = 0, 1 },
		"short commit":           func(w *Workflow) { w.History[0].Commit = oidA[:39] },
		"uppercase commit":       func(w *Workflow) { w.History[0].Commit = strings.ToUpper(oidA) },
		"newline in base":        func(w *Workflow) { w.History[1].Base = oidB[:39] + "\n" },
		"review and omission":    func(w *Workflow) { w.History[1].Review = "text" },
		"neither":                func(w *Workflow) { w.History[1].Omitted = "" },
		"unknown omission":       func(w *Workflow) { w.History[1].Omitted = "evicted" },
		"oversized review":       func(w *Workflow) { w.History[0].Review = strings.Repeat("x", MaxHistoryRecordBytes+1) },
		"aggregate with last review": func(w *Workflow) {
			w.History[0].Review = strings.Repeat("x", MaxHistoryRecordBytes)
			w.History[1] = HistoryRecord{Round: 2, Commit: oidA, Base: oidB, Review: strings.Repeat("y", MaxHistoryRecordBytes)}
			w.LastReview = "z"
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			w := valid()
			damage(&w)
			msg := w.InvalidHistory()
			if msg == "" {
				t.Fatalf("accepted: %+v", w)
			}
			for _, leaked := range []string{"one", "text", "evicted", "xxxx", oidA[:8], "0123456789ABCDEF"} {
				if strings.Contains(msg, leaked) {
					t.Errorf("message echoes a stored value %q: %s", leaked, msg)
				}
			}
		})
	}
}
