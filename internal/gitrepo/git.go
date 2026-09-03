// Package gitrepo validates review targets against a local Git repository.
//
// It implements the repository validation and review-target rules in
// docs/MVP.md: canonical repository identity, local branch normalization,
// commit resolution, branch-tip and HEAD equality, clean-worktree detection,
// merge-base resolution against the primary branch, and rewritten-history
// detection. Every Git invocation uses an argument array; nothing is passed
// through a shell.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// maxErrorOutput bounds how much of Git's stderr is included in an error so
// a pathological repository cannot produce an unbounded message.
const maxErrorOutput = 512

// gitEnvBlocklist names environment variables that would redirect Git away
// from the directory passed with -C. They are removed so the supplied
// repository path is authoritative.
var gitEnvBlocklist = [...]string{ //nolint:gochecknoglobals // constant table
	"GIT_DIR=",
	"GIT_WORK_TREE=",
	"GIT_INDEX_FILE=",
	"GIT_COMMON_DIR=",
	"GIT_OBJECT_DIRECTORY=",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
	"GIT_NAMESPACE=",
}

// exitError is returned by run when Git exits non-zero. Callers that treat a
// specific exit code as a result (for example merge-base --is-ancestor) can
// inspect it with errors.As.
type exitError struct {
	args   []string
	code   int
	stderr string
}

func (e *exitError) Error() string {
	msg := fmt.Sprintf("git %s: exit status %d", strings.Join(e.args, " "), e.code)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

// run executes git with the given argument array in dir and returns trimmed
// stdout. It never composes a shell command.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // argument array; inputs never reach a shell
	cmd.Dir = dir
	cmd.Env = cleanEnv(cmd.Environ())

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", &exitError{args: args, code: ee.ExitCode(), stderr: truncate(stderr.String())}
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// cleanEnv drops blocklisted Git variables and pins prompts and locale so
// output is stable and no invocation can wait on a terminal.
func cleanEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if blocked(kv) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

func blocked(kv string) bool {
	for _, prefix := range gitEnvBlocklist {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxErrorOutput {
		return s[:maxErrorOutput] + "..."
	}
	return s
}

// exitCode returns Git's exit status when err is an exitError, and -1
// otherwise.
func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return -1
}
