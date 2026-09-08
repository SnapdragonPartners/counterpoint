package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// InstructionsFile is the per-repository reviewer instruction file,
	// read from the root of the commit under review (issue #16).
	InstructionsFile = "COUNTERPOINT.md"

	// MaxInstructionsBytes bounds the file. A larger file is rejected, never
	// truncated: a shortened instruction could mislead, and the author can
	// shorten it in the next commit.
	MaxInstructionsBytes = 16 << 10
)

// ErrInstructionsInvalid reports a COUNTERPOINT.md that cannot be quoted:
// not a regular file, too large, or not UTF-8.
var ErrInstructionsInvalid = errors.New("COUNTERPOINT.md cannot be used")

// ReadInstructions returns the text of InstructionsFile at the root of
// commit, or "" when the commit has no such file or it is blank. The file is
// read from the commit object, never from a worktree, so the text is part of
// the immutable review target. commit must be a full object id. Trailing
// newlines are removed, as for every Git result in this package; the text
// is otherwise unchanged.
//
// Accepted: a blob in mode 100644 or 100755, at most MaxInstructionsBytes,
// valid UTF-8. Rejected with ErrInstructionsInvalid: a symbolic link, a
// submodule, a directory, an oversized file, or invalid UTF-8.
func (r *Repository) ReadInstructions(ctx context.Context, commit string) (string, error) {
	if !objectIDPattern.MatchString(commit) {
		return "", fmt.Errorf("%w: %q is not a full object id", ErrCommitNotFound, commit)
	}
	entry, err := r.git(ctx, "ls-tree", "-z", commit, "--", InstructionsFile)
	if err != nil {
		return "", fmt.Errorf("look up %s at %s: %w", InstructionsFile, commit, err)
	}
	entry = strings.TrimRight(entry, "\x00")
	if entry == "" {
		return "", nil
	}
	// "<mode> <type> <object>\t<path>"
	meta, _, ok := strings.Cut(entry, "\t")
	fields := strings.Fields(meta)
	if !ok || len(fields) != 3 {
		return "", fmt.Errorf("look up %s at %s: unexpected ls-tree output", InstructionsFile, commit)
	}
	mode, typ, oid := fields[0], fields[1], fields[2]
	switch {
	case mode == "120000":
		return "", fmt.Errorf("%w: it is a symbolic link at commit %s", ErrInstructionsInvalid, commit)
	case mode == "160000":
		return "", fmt.Errorf("%w: it is a submodule at commit %s", ErrInstructionsInvalid, commit)
	case typ != "blob" || (mode != "100644" && mode != "100755"):
		return "", fmt.Errorf("%w: it is not a regular file at commit %s (mode %s, type %s)", ErrInstructionsInvalid, commit, mode, typ)
	case !objectIDPattern.MatchString(oid):
		return "", fmt.Errorf("look up %s at %s: unexpected object id in ls-tree output", InstructionsFile, commit)
	}

	// The size is checked before the blob is read so an oversized file is
	// reported by its size rather than as a truncated read.
	sizeText, err := r.git(ctx, "cat-file", "-s", oid)
	if err != nil {
		return "", fmt.Errorf("size of %s at %s: %w", InstructionsFile, commit, err)
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil {
		return "", fmt.Errorf("size of %s at %s: unexpected cat-file output: %w", InstructionsFile, commit, err)
	}
	if size > MaxInstructionsBytes {
		return "", fmt.Errorf("%w: it is %d bytes at commit %s, limit %d", ErrInstructionsInvalid, size, commit, MaxInstructionsBytes)
	}
	text, err := runWithLimit(ctx, r.Worktree, MaxInstructionsBytes+1, "cat-file", "blob", oid)
	if err != nil {
		return "", fmt.Errorf("read %s at %s: %w", InstructionsFile, commit, err)
	}
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("%w: it is not valid UTF-8 at commit %s", ErrInstructionsInvalid, commit)
	}
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	return text, nil
}
