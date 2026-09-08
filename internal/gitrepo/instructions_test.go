package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadInstructionsReturnsTheFileAtTheCommit(t *testing.T) {
	r := newTestRepo(t, "main")
	r.write(InstructionsFile, "Run make check.\nPrefer table tests.\n")
	first := r.commit("with-instructions")
	r.write(InstructionsFile, "Second version.\n")
	second := r.commit("changed-instructions")
	// The worktree differs from both commits and must not be consulted.
	r.write(InstructionsFile, "Uncommitted edit.\n")

	repo := r.open()
	got, err := repo.ReadInstructions(context.Background(), first)
	if err != nil || got != "Run make check.\nPrefer table tests." {
		t.Errorf("first commit: %q, %v", got, err)
	}
	got, err = repo.ReadInstructions(context.Background(), second)
	if err != nil || got != "Second version." {
		t.Errorf("second commit: %q, %v", got, err)
	}
}

func TestReadInstructionsIsEmptyWhenAbsentOrBlank(t *testing.T) {
	r := newTestRepo(t, "main")
	repo := r.open()
	if got, err := repo.ReadInstructions(context.Background(), r.head()); err != nil || got != "" {
		t.Errorf("absent: %q, %v", got, err)
	}
	r.write(InstructionsFile, " \n\t\n")
	blank := r.commit("blank")
	if got, err := repo.ReadInstructions(context.Background(), blank); err != nil || got != "" {
		t.Errorf("blank: %q, %v", got, err)
	}
}

func TestReadInstructionsAcceptsAnExecutableBlob(t *testing.T) {
	r := newTestRepo(t, "main")
	r.write(InstructionsFile, "exec mode\n")
	if err := os.Chmod(filepath.Join(r.dir, InstructionsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	c := r.commit("exec")
	if mode := r.git("ls-tree", c, "--", InstructionsFile); !strings.HasPrefix(mode, "100755 ") {
		t.Fatalf("fixture is not executable: %s", mode)
	}
	if got, err := r.open().ReadInstructions(context.Background(), c); err != nil || got != "exec mode" {
		t.Errorf("executable blob: %q, %v", got, err)
	}
}

func TestReadInstructionsRejectsUnusableFiles(t *testing.T) {
	cases := map[string]func(r *testRepo){
		"symlink": func(r *testRepo) {
			r.write("target.md", "elsewhere\n")
			if err := os.Symlink("target.md", filepath.Join(r.dir, InstructionsFile)); err != nil {
				r.t.Fatal(err)
			}
		},
		"directory":     func(r *testRepo) { r.write(filepath.Join(InstructionsFile, "inner.md"), "nested\n") },
		"too large":     func(r *testRepo) { r.write(InstructionsFile, strings.Repeat("a", MaxInstructionsBytes+1)) },
		"invalid utf-8": func(r *testRepo) { r.write(InstructionsFile, "ok\xff\xfe\n") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newTestRepo(t, "main")
			setup(r)
			c := r.commit("bad")
			got, err := r.open().ReadInstructions(context.Background(), c)
			if !errors.Is(err, ErrInstructionsInvalid) {
				t.Fatalf("err = %v, want ErrInstructionsInvalid", err)
			}
			if got != "" {
				t.Errorf("text returned with the error: %q", got)
			}
			if strings.Contains(err.Error(), "aaaa") || strings.Contains(err.Error(), "nested") {
				t.Errorf("error echoes file content: %v", err)
			}
		})
	}
}

func TestReadInstructionsAtTheSizeLimitIsAccepted(t *testing.T) {
	r := newTestRepo(t, "main")
	body := strings.Repeat("b", MaxInstructionsBytes-1) + "\n"
	r.write(InstructionsFile, body)
	c := r.commit("limit")
	got, err := r.open().ReadInstructions(context.Background(), c)
	if err != nil || got != strings.TrimRight(body, "\n") {
		t.Errorf("at the limit: len %d, %v", len(got), err)
	}
}

func TestReadInstructionsRequiresAFullObjectID(t *testing.T) {
	r := newTestRepo(t, "main")
	for _, id := range []string{"", "HEAD", "main", "-x", r.head()[:12]} {
		if _, err := r.open().ReadInstructions(context.Background(), id); !errors.Is(err, ErrCommitNotFound) {
			t.Errorf("%q: err = %v, want ErrCommitNotFound", id, err)
		}
	}
}
