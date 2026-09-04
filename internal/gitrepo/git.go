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
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	// maxStdout bounds captured standard output while Git runs. Every
	// command this package issues produces a few lines at most, except
	// status in a badly dirty worktree, which handles overflow itself.
	maxStdout = 1 << 20
	// maxStderr bounds captured diagnostics while Git runs.
	maxStderr = 64 << 10
	// maxErrorOutput bounds how much stderr is quoted in an error message.
	maxErrorOutput = 512
	// exitNotFound is Git's exit status for a ref or object that does not
	// exist under --verify --quiet, for a missing symbolic ref, and for
	// "false" answers such as merge-base --is-ancestor.
	exitNotFound = 1
)

// ErrOutputTooLarge reports that a Git command produced more standard output
// than this package is prepared to hold.
var ErrOutputTooLarge = errors.New("git output exceeded the size limit")

// gitEnvBlocklist names environment variables that would redirect Git away
// from the directory passed with -C, plus the two variables cleanEnv pins.
// They are removed so the supplied repository path is authoritative and the
// pinned values are the only definitions, independent of how a platform
// resolves duplicate keys.
var gitEnvBlocklist = [...]string{ //nolint:gochecknoglobals // constant table
	"GIT_DIR=",
	"GIT_WORK_TREE=",
	"GIT_INDEX_FILE=",
	"GIT_COMMON_DIR=",
	"GIT_OBJECT_DIRECTORY=",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
	"GIT_NAMESPACE=",
	"GIT_TERMINAL_PROMPT=",
	"LC_ALL=",
	"GIT_OPTIONAL_LOCKS=",
}

// boundedBuffer retains at most limit bytes and records whether more were
// offered. It never fails a write, so the child process keeps draining
// instead of blocking on a full pipe.
type boundedBuffer struct {
	buf      strings.Builder
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	switch {
	case room <= 0:
		b.overflow = true
	case len(p) > room:
		b.buf.Write(p[:room])
		b.overflow = true
	default:
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// exitError is returned when Git exits non-zero. Callers that treat a
// specific exit status as a result inspect it with exitCode.
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
// stdout. Output beyond maxStdout is an error, since every caller of run
// needs the complete result.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runWithLimit(ctx, dir, maxStdout, args...)
}

// runWithLimit is run with an explicit stdout limit; exceeding it is
// ErrOutputTooLarge.
func runWithLimit(ctx context.Context, dir string, stdoutLimit int, args ...string) (string, error) {
	out, truncated, err := runBounded(ctx, dir, stdoutLimit, args...)
	if err != nil {
		return "", err
	}
	if truncated {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), ErrOutputTooLarge)
	}
	return out, nil
}

// runBounded executes git and captures at most stdoutLimit bytes of standard
// output, reporting whether more was discarded. It never composes a shell
// command. A failure caused by context cancellation wraps the context error
// so callers can match it with errors.Is.
func runBounded(ctx context.Context, dir string, stdoutLimit int, args ...string) (out string, truncated bool, err error) {
	return runBoundedEnv(ctx, dir, stdoutLimit, nil, args...)
}

// runBoundedEnv is runBounded with extra environment entries appended after
// the cleaned environment, so they take effect on platforms that honor the
// last definition of a key.
func runBoundedEnv(ctx context.Context, dir string, stdoutLimit int, extraEnv []string, args ...string) (out string, truncated bool, err error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // argument array; inputs never reach a shell
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(cmd.Environ()), extraEnv...)

	stdout := &boundedBuffer{limit: stdoutLimit}
	stderr := &boundedBuffer{limit: maxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, fmt.Errorf("git %s: %w", strings.Join(args, " "), ctxErr)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", false, &exitError{args: args, code: ee.ExitCode(), stderr: truncate(stderr.String())}
		}
		return "", false, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimRight(stdout.String(), "\n"), stdout.overflow, nil
}

// cleanEnv drops blocklisted variables and pins three settings: prompts
// off so no invocation can wait on a terminal, the C locale so diagnostics
// are stable, and optional locks off so read-only queries such as status
// never refresh .git/index and the reviewed repository is left untouched.
// The pinned keys are also blocklisted, so each appears exactly once.
func cleanEnv(env []string) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		if blocked(kv) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")
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
// otherwise, including for cancellation and process-launch failures.
func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return -1
}

// isNotFound reports whether err is Git's documented "does not exist" or
// "false" exit status. Every other failure is operational and must be
// surfaced rather than treated as absence.
func isNotFound(err error) bool {
	return exitCode(err) == exitNotFound
}

// stderrOf returns the captured diagnostics when err is an exitError.
func stderrOf(err error) string {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.stderr
	}
	return ""
}
