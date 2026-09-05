package state

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// Request is the normalized identity of one review request. Two requests
// with equal fields are the same request and replay the stored review.
type Request struct {
	// Identity is the canonical repository identity.
	Identity string
	// BranchRef is the full local branch ref.
	BranchRef string
	// Commit is the resolved commit object ID under review.
	Commit string
	// Base is the resolved merge-base object ID against the primary branch.
	Base string
	// BranchNotes is the author's handoff text, hashed but not stored.
	BranchNotes string
	// Build is true for a build-capable review in a disposable checkout.
	// The same commit and notes reviewed in the other mode is a different
	// request, since the reviewer's evidence differs.
	Build bool
}

// Key returns the workflow key for the request.
func (r Request) Key() string {
	return r.Identity + "::" + r.BranchRef
}

// buildMarker is the extra hashed field for a build-capable request. It is
// present only when Build is true, so hashes stored before the flag existed
// keep matching read-only requests and still replay.
const buildMarker = "build"

// Hash returns a hex SHA-256 over the request fields. Each field is length
// prefixed so that no two distinct requests can produce the same byte
// stream, regardless of field contents.
func (r Request) Hash() string {
	h := sha256.New()
	fields := []string{r.Identity, r.BranchRef, r.Commit, r.Base, r.BranchNotes}
	if r.Build {
		fields = append(fields, buildMarker)
	}
	for _, field := range fields {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(field)))
		h.Write(n[:])
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}
