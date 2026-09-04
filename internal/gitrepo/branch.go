package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Sentinel errors for branch handling.
var (
	ErrInvalidBranch   = errors.New("invalid branch name")
	ErrBranchNotFound  = errors.New("local branch not found")
	ErrNoPrimaryBranch = errors.New("cannot determine primary branch")
)

const (
	headsPrefix   = "refs/heads/"
	remotesPrefix = "refs/remotes/"
	// defaultRemote is the remote whose HEAD symbolic ref identifies the
	// primary branch when the repository has one.
	defaultRemote = "origin"
)

// fallbackPrimaryNames are tried in order when no remote HEAD is available.
var fallbackPrimaryNames = [...]string{"main", "master"} //nolint:gochecknoglobals // constant table

// objectIDPattern matches a full SHA-1 or SHA-256 object ID.
var objectIDPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`) //nolint:gochecknoglobals // compiled once

// Branch is a validated local branch.
type Branch struct {
	// Name is the short branch name, without refs/heads/.
	Name string
	// Ref is the full local ref, refs/heads/<Name>.
	Ref string
	// Tip is the full object ID the branch points at.
	Tip string
}

// normalizeBranchName accepts a bare branch name or a full refs/heads/ ref
// and returns the short name. Any other ref namespace is rejected.
func normalizeBranchName(name string) (string, error) {
	switch {
	case name == "":
		return "", fmt.Errorf("%w: empty", ErrInvalidBranch)
	case strings.HasPrefix(name, headsPrefix):
		short := strings.TrimPrefix(name, headsPrefix)
		if short == "" {
			return "", fmt.Errorf("%w: %q", ErrInvalidBranch, name)
		}
		return short, nil
	case strings.HasPrefix(name, "refs/"):
		return "", fmt.Errorf("%w: %q is not under %s", ErrInvalidBranch, name, headsPrefix)
	case strings.HasPrefix(name, "-"):
		return "", fmt.Errorf("%w: %q begins with a dash", ErrInvalidBranch, name)
	default:
		return name, nil
	}
}

// ResolveBranch normalizes name, validates it with git check-ref-format, and
// verifies that the local branch exists. It returns the branch with its tip.
func (r *Repository) ResolveBranch(ctx context.Context, name string) (*Branch, error) {
	short, err := normalizeBranchName(name)
	if err != nil {
		return nil, err
	}
	ref := headsPrefix + short

	// check-ref-format does not accept a "--" separator. The argument always
	// starts with refs/heads/, so it can never be parsed as an option.
	if _, err := r.git(ctx, "check-ref-format", ref); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidBranch, name)
		}
		return nil, err
	}

	tip, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		if !isNotFound(err) {
			return nil, err
		}
		remote, err := r.refExists(ctx, remotesPrefix+short)
		if err != nil {
			return nil, err
		}
		if remote {
			return nil, fmt.Errorf("%w: %q is a remote-tracking branch, not a local branch", ErrBranchNotFound, name)
		}
		return nil, fmt.Errorf("%w: %q", ErrBranchNotFound, name)
	}
	if !objectIDPattern.MatchString(tip) {
		return nil, fmt.Errorf("unexpected object id %q for %s", tip, ref)
	}
	return &Branch{Name: short, Ref: ref, Tip: tip}, nil
}

// refExists reports whether a fully qualified ref resolves. Only Git's
// not-found status means absence; cancellation and repository failures are
// returned as errors.
func (r *Repository) refExists(ctx context.Context, ref string) (bool, error) {
	_, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
	switch {
	case err == nil:
		return true, nil
	case isNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// PrimaryBranch determines the repository's primary branch name and the ref
// to diff against. The name comes from the origin remote's HEAD when it is
// set, otherwise from the first of main or master that exists locally or as
// an origin remote-tracking branch. The ref is the local branch when it
// exists and otherwise the remote-tracking branch.
func (r *Repository) PrimaryBranch(ctx context.Context) (name, ref string, err error) {
	if name, err = r.remoteHeadName(ctx); err != nil {
		return "", "", err
	}
	if name != "" {
		if ref, err = r.primaryRef(ctx, name); err != nil {
			return "", "", err
		}
		return name, ref, nil
	}
	for _, candidate := range fallbackPrimaryNames {
		ref, err = r.primaryRef(ctx, candidate)
		if err == nil {
			return candidate, ref, nil
		}
		if !errors.Is(err, ErrNoPrimaryBranch) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("%w: no origin HEAD and no local or origin main or master", ErrNoPrimaryBranch)
}

// remoteHeadName returns the branch name origin's HEAD points at, or "" when
// origin has no HEAD.
func (r *Repository) remoteHeadName(ctx context.Context) (string, error) {
	out, err := r.git(ctx, "symbolic-ref", "--quiet", "--", remotesPrefix+defaultRemote+"/HEAD")
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	prefix := remotesPrefix + defaultRemote + "/"
	if !strings.HasPrefix(out, prefix) {
		return "", fmt.Errorf("%w: origin HEAD points at %q", ErrNoPrimaryBranch, out)
	}
	return strings.TrimPrefix(out, prefix), nil
}

// primaryRef returns the local ref for name when it exists, otherwise the
// origin remote-tracking ref, otherwise ErrNoPrimaryBranch.
func (r *Repository) primaryRef(ctx context.Context, name string) (string, error) {
	for _, ref := range []string{headsPrefix + name, remotesPrefix + defaultRemote + "/" + name} {
		exists, err := r.refExists(ctx, ref)
		if err != nil {
			return "", err
		}
		if exists {
			return ref, nil
		}
	}
	return "", fmt.Errorf("%w: %q exists neither locally nor on origin", ErrNoPrimaryBranch, name)
}
