package state

import "testing"

func TestRequestKey(t *testing.T) {
	r := Request{Identity: "/repo/.git", BranchRef: "refs/heads/f"}
	if got := r.Key(); got != "/repo/.git::refs/heads/f" {
		t.Errorf("Key = %q", got)
	}
}

func TestRequestHashIsStableAndSensitiveToEveryField(t *testing.T) {
	base := Request{Identity: "id", BranchRef: "refs/heads/f", Commit: "c", Base: "b", BranchNotes: "notes"}
	first, second := base.Hash(), base.Hash()
	if first != second {
		t.Fatal("Hash is not deterministic")
	}
	if len(base.Hash()) != 64 {
		t.Fatalf("Hash length = %d, want 64 hex chars", len(base.Hash()))
	}

	variants := map[string]Request{
		"identity": {Identity: "id2", BranchRef: "refs/heads/f", Commit: "c", Base: "b", BranchNotes: "notes"},
		"branch":   {Identity: "id", BranchRef: "refs/heads/g", Commit: "c", Base: "b", BranchNotes: "notes"},
		"commit":   {Identity: "id", BranchRef: "refs/heads/f", Commit: "c2", Base: "b", BranchNotes: "notes"},
		"base":     {Identity: "id", BranchRef: "refs/heads/f", Commit: "c", Base: "b2", BranchNotes: "notes"},
		"notes":    {Identity: "id", BranchRef: "refs/heads/f", Commit: "c", Base: "b", BranchNotes: "notes "},
	}
	for name, v := range variants {
		if v.Hash() == base.Hash() {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

func TestRequestHashLengthPrefixPreventsFieldShifting(t *testing.T) {
	// Without length prefixes these two would concatenate to the same bytes.
	a := Request{Identity: "ab", BranchRef: "c"}
	b := Request{Identity: "a", BranchRef: "bc"}
	if a.Hash() == b.Hash() {
		t.Error("field boundary shift produced an identical hash")
	}
}
