package state

import "fmt"

// Retention bounds for a workflow's review history, the ledger of earlier
// verdicts that later prompts quote back to the reviewer. The history is
// derived convenience data: the replay copy of the newest review lives in
// the workflow's LastReview and is never subject to these bounds.
const (
	// MaxHistoryRecords bounds the records kept before the newest review,
	// so the newest review plus the records make three rounds visible.
	MaxHistoryRecords = 2

	// MaxHistoryRecordBytes bounds the review text of one record. A larger
	// review is kept as a placeholder, never truncated: a shortened verdict
	// could mislead.
	MaxHistoryRecordBytes = 24 << 10

	// MaxHistoryBytes bounds all review text a prompt quotes: the records
	// plus the newest review when that fits the per-record bound.
	MaxHistoryBytes = 48 << 10

	// OmittedTooLarge is the only omission reason a placeholder carries.
	OmittedTooLarge = "too-large"
)

// HistoryRecord is one completed earlier round. Exactly one of Review and
// Omitted is set.
type HistoryRecord struct {
	Round  int    `json:"round"`
	Commit string `json:"commit"`
	Base   string `json:"base"`
	// Review is the verdict text, verbatim.
	Review string `json:"review,omitempty"`
	// Omitted names why the text was not kept; OmittedTooLarge is the only
	// value.
	Omitted string `json:"omitted,omitempty"`
}

// injectedBytes is how much review text a prompt quotes for one record.
func (r HistoryRecord) injectedBytes() int {
	return len(r.Review)
}

// NewHistoryRecord builds the record for a completed round, replacing text
// over the per-record bound with a placeholder.
func NewHistoryRecord(round int, commit, base, review string) HistoryRecord {
	r := HistoryRecord{Round: round, Commit: commit, Base: base}
	if len(review) > MaxHistoryRecordBytes {
		r.Omitted = OmittedTooLarge
	} else {
		r.Review = review
	}
	return r
}

// RetainHistory returns the history to persist when a round completes:
// previous, the record of the round that was the newest until now, is
// appended, then the oldest records are evicted until the count bound holds
// and the byte bound holds for the records together with the newest review
// the next prompt quotes beside them. newestReview is that review's text.
// Evicted rounds are dropped outright; the prompt discloses them by count.
func RetainHistory(history []HistoryRecord, previous HistoryRecord, newestReview string) []HistoryRecord {
	out := make([]HistoryRecord, 0, len(history)+1)
	out = append(out, history...)
	out = append(out, previous)
	newestBytes := NewHistoryRecord(0, "", "", newestReview).injectedBytes()
	for len(out) > MaxHistoryRecords || historyBytes(out)+newestBytes > MaxHistoryBytes {
		out = out[1:]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func historyBytes(history []HistoryRecord) int {
	n := 0
	for _, r := range history {
		n += r.injectedBytes()
	}
	return n
}

// InvalidHistory describes the first way w.History violates the ledger's
// invariants, or returns "" when it holds. The history is untrusted file
// content; the description names the defect and never echoes a value. The
// records must be a contiguous ascending run of rounds ending just before
// w.Round, within the count and byte bounds, each with validated object
// ids and exactly one of a bounded review or the fixed omission reason.
func (w Workflow) InvalidHistory() string {
	h := w.History
	if len(h) > MaxHistoryRecords {
		return fmt.Sprintf("history has %d records, limit %d", len(h), MaxHistoryRecords)
	}
	for i, r := range h {
		want := w.Round - len(h) + i
		switch {
		case r.Round != want:
			return fmt.Sprintf("history record %d has round %d, want %d", i, r.Round, want)
		case want < 1:
			return fmt.Sprintf("history record %d has a round below 1", i)
		case !IsObjectID(r.Commit):
			return fmt.Sprintf("history record %d has an invalid commit", i)
		case !IsObjectID(r.Base):
			return fmt.Sprintf("history record %d has an invalid base", i)
		case r.Review != "" && r.Omitted != "":
			return fmt.Sprintf("history record %d has both a review and an omission", i)
		case r.Review == "" && r.Omitted == "":
			return fmt.Sprintf("history record %d has neither a review nor an omission", i)
		case r.Omitted != "" && r.Omitted != OmittedTooLarge:
			return fmt.Sprintf("history record %d has an unknown omission reason", i)
		case len(r.Review) > MaxHistoryRecordBytes:
			return fmt.Sprintf("history record %d review is %d bytes, limit %d", i, len(r.Review), MaxHistoryRecordBytes)
		}
	}
	newest := NewHistoryRecord(0, "", "", w.LastReview).injectedBytes()
	if total := historyBytes(h) + newest; total > MaxHistoryBytes {
		return fmt.Sprintf("history quotes %d bytes with the last review, limit %d", total, MaxHistoryBytes)
	}
	return ""
}

// IsObjectID accepts a full SHA-1 or SHA-256 object id in lower-case hex,
// the same domain the Git boundary accepts.
func IsObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
