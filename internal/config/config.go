// Package config loads Counterpoint's optional configuration file and
// resolves its values against the environment. The file is the user's own,
// kept beside the state file in the user configuration directory; it is not
// read from a reviewed repository and carries no per-repository policy.
//
// Precedence is environment variable, then file, then the built-in default,
// so an existing environment override keeps working unchanged and the file
// only supplies what the environment does not. Every value is validated
// here, at startup, so a typo fails before a review is started rather than
// in the middle of one.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/SnapdragonPartners/counterpoint/internal/review"
)

const (
	// EnvPath overrides the configuration file location, like
	// COUNTERPOINT_STATE_FILE does for the state file. It must be an
	// absolute path.
	EnvPath = "COUNTERPOINT_CONFIG_FILE"

	// configSubdir and fileName name the default location under
	// os.UserConfigDir, beside the state file.
	configSubdir = "counterpoint"
	fileName     = "config.json"

	// MaxBytes bounds the file. It holds a handful of short scalars, so
	// anything approaching this is a mistake or a different file, and the
	// bound keeps a wrong path from being read into memory whole.
	MaxBytes = 64 << 10
)

// ErrInvalid is returned for every rejected file: a shape that does not
// decode, an unknown key, or a value outside its accepted set.
var ErrInvalid = errors.New("invalid configuration")

// Config is the resolved configuration. Empty path fields mean the caller
// should use its own built-in default; ReviewEffort is always set.
type Config struct {
	// ReviewEffort is the reasoning effort for the review turn.
	ReviewEffort string
	// StateFile and CheckoutDir are absolute paths, or "" when the file
	// did not set them.
	StateFile   string
	CheckoutDir string
	// Path is the file the values came from, or "" when no file exists.
	// It is logged at startup so the source of a surprising setting is
	// discoverable.
	Path string
}

// file is the on-disk shape. Pointers distinguish a key that is absent from
// one explicitly set to the empty string: the first falls through to the
// next source, the second is a mistake and is rejected.
type file struct {
	ReviewEffort *string `json:"review_effort"`
	StateFile    *string `json:"state_file"`
	CheckoutDir  *string `json:"checkout_dir"`
}

// Load reads the configuration file and returns the resolved configuration.
// A missing file is not an error: it yields the built-in defaults. Any file
// that exists must be valid.
//
// The file lives in the user's own configuration directory and is trusted to
// the same degree as the user, per ADR 0001: the adversaries are untrusted
// inputs and the sandboxed reviewer, not a process running as the user. It
// is still fully validated, because the common failure here is a typo rather
// than an attack.
func Load() (Config, error) {
	cfg := Config{ReviewEffort: review.DefaultReasoningEffort}

	path, err := Path()
	if err != nil {
		return Config{}, err
	}

	raw, err := read(path)
	if err != nil {
		return Config{}, err
	}
	if raw == nil {
		return cfg, nil
	}
	cfg.Path = path

	var f file
	dec := json.NewDecoder(bytes.NewReader(raw))
	// An unknown key is a typo that would otherwise be silently ignored,
	// leaving the user with a setting they believe is in force.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	// Anything after the object means the file is not what it appears to
	// be; decoding only the first value would accept half a file.
	if dec.More() {
		return Config{}, fmt.Errorf("%w: %s: unexpected content after the configuration object", ErrInvalid, path)
	}

	if f.ReviewEffort != nil {
		if !review.EffortAccepted(*f.ReviewEffort) {
			return Config{}, fmt.Errorf("%w: %s: review_effort %q is not one of %s",
				ErrInvalid, path, *f.ReviewEffort, strings.Join(review.AcceptedEfforts(), ", "))
		}
		cfg.ReviewEffort = *f.ReviewEffort
	}
	if cfg.StateFile, err = absolute(path, "state_file", f.StateFile); err != nil {
		return Config{}, err
	}
	if cfg.CheckoutDir, err = absolute(path, "checkout_dir", f.CheckoutDir); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Path returns the configuration file location: EnvPath when set, otherwise
// the Counterpoint subdirectory of the user configuration directory.
func Path() (string, error) {
	if p := os.Getenv(EnvPath); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", EnvPath, p)
		}
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, configSubdir, fileName), nil
}

// read returns the file's bytes, or nil when it does not exist. A file over
// MaxBytes is rejected without being read whole.
func read(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open configuration file %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat configuration file %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", ErrInvalid, path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInvalid, path)
	}
	if info.Size() > MaxBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes, limit %d", ErrInvalid, path, info.Size(), MaxBytes)
	}

	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read configuration file %s: %w", path, err)
	}
	// The file may have grown between the stat and the read.
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("%w: %s is over the %d byte limit", ErrInvalid, path, MaxBytes)
	}
	return raw, nil
}

// absolute validates an optional path value. An absent key yields "", which
// leaves the built-in default in force; a key set to anything but an
// absolute path is rejected.
func absolute(path, key string, v *string) (string, error) {
	if v == nil {
		return "", nil
	}
	if *v == "" {
		return "", fmt.Errorf("%w: %s: %s is empty; remove the key to use the default", ErrInvalid, path, key)
	}
	if !filepath.IsAbs(*v) {
		return "", fmt.Errorf("%w: %s: %s must be an absolute path, got %q", ErrInvalid, path, key, *v)
	}
	return *v, nil
}
