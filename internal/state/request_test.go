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

// TestRequestHashKeepsLegacyEncodingForReadOnly pins the hash of a request
// without the build flag to the value the pre-flag code produced, so state
// written before the flag existed still replays identical read-only
// requests instead of starting a paid turn.
func TestRequestHashKeepsLegacyEncodingForReadOnly(t *testing.T) {
	base := Request{Identity: "id", BranchRef: "refs/heads/f", Commit: "c", Base: "b", BranchNotes: "notes"}
	const legacy = "0169522937f4f325152809af94d63fb745260c7ccef363a211c2df410c372419"
	if got := base.Hash(); got != legacy {
		t.Fatalf("Hash() = %s, want the pre-flag value %s", got, legacy)
	}
	build := base
	build.Build = true
	if build.Hash() == legacy {
		t.Fatal("a build-capable request hashes like the read-only one")
	}
	// The marker is length-prefixed like every field, so notes ending in
	// the marker text do not collide with a build request.
	tricky := base
	tricky.BranchNotes += "build"
	if tricky.Hash() == build.Hash() {
		t.Fatal("notes ending in the marker collide with the build flag")
	}
}
