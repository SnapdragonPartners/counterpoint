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
	"slices"
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

// knownKeys is every key the configuration file may contain. Parsing is
// key by key rather than into a struct so that the four states a key can be
// in stay distinct: absent, null, the wrong type, and a usable value. A
// struct decode collapses the first three into the zero value, which would
// let an explicit null silently mean "use the default".
var knownKeys = [...]string{"review_effort", "state_file", "checkout_dir"} //nolint:gochecknoglobals // constant table

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

	fields, err := parse(path, raw)
	if err != nil {
		return Config{}, err
	}

	effort, ok, err := text(path, "review_effort", fields)
	if err != nil {
		return Config{}, err
	}
	if ok {
		if !review.EffortAccepted(effort) {
			return Config{}, fmt.Errorf("%w: %s: review_effort %q is not one of %s",
				ErrInvalid, path, effort, strings.Join(review.AcceptedEfforts(), ", "))
		}
		cfg.ReviewEffort = effort
	}
	if cfg.StateFile, err = absolute(path, "state_file", fields); err != nil {
		return Config{}, err
	}
	if cfg.CheckoutDir, err = absolute(path, "checkout_dir", fields); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parse decodes the file into its raw keys, rejecting anything that is not
// a JSON object, a null in place of one, an unknown key, or any content
// after the object.
func parse(path string, raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A pointer target distinguishes a JSON null, which leaves it nil,
	// from an object; decoding into the map directly would accept null as
	// an empty configuration.
	var fields *map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, path, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("%w: %s: null is not a configuration object", ErrInvalid, path)
	}
	// Decoder.More reports whether another element of the current array or
	// object follows, so it is false at a stray closing delimiter and would
	// accept `{}}`. Requiring the next decode to reach EOF rejects every
	// trailing byte, whether it parses as a value or not.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %s: unexpected content after the configuration object", ErrInvalid, path)
	}
	for key := range *fields {
		if !slices.Contains(knownKeys[:], key) {
			// An unknown key is a typo that would otherwise be silently
			// ignored, leaving the user with a setting they believe is in
			// force.
			return nil, fmt.Errorf("%w: %s: unknown key %q; accepted keys are %s",
				ErrInvalid, path, key, strings.Join(knownKeys[:], ", "))
		}
	}
	return *fields, nil
}

// text returns the string value of key and whether it was present. A key
// present but null is rejected: once decoded it is indistinguishable from
// omission, so accepting it would silently apply the default.
func text(path, key string, fields map[string]json.RawMessage) (string, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return "", false, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return "", false, fmt.Errorf("%w: %s: %s is null; remove the key to use the default", ErrInvalid, path, key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, fmt.Errorf("%w: %s: %s: %w", ErrInvalid, path, key, err)
	}
	return s, true, nil
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
	// The type is checked before the open, not after it: opening a FIFO
	// with no writer blocks indefinitely, so a stat on the descriptor
	// would never be reached and a mistyped path would hang startup
	// instead of reporting invalid configuration. Stat follows symlinks,
	// so a configuration file symlinked from elsewhere still works.
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat configuration file %s: %w", path, err)
	}
	if err := regular(path, info); err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open configuration file %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	// Re-checked on the descriptor, so a path replaced between the stat
	// and the open is rejected rather than read.
	if info, err = f.Stat(); err != nil {
		return nil, fmt.Errorf("stat configuration file %s: %w", path, err)
	}
	if err := regular(path, info); err != nil {
		return nil, err
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

// regular rejects anything that is not a readable regular file within the
// size bound.
func regular(path string, info os.FileInfo) error {
	if info.IsDir() {
		return fmt.Errorf("%w: %s is a directory", ErrInvalid, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInvalid, path)
	}
	if info.Size() > MaxBytes {
		return fmt.Errorf("%w: %s is %d bytes, limit %d", ErrInvalid, path, info.Size(), MaxBytes)
	}
	return nil
}

// absolute validates an optional path value. An absent key yields "", which
// leaves the built-in default in force; a key set to anything but an
// absolute path is rejected.
func absolute(path, key string, fields map[string]json.RawMessage) (string, error) {
	v, ok, err := text(path, key, fields)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	if v == "" {
		return "", fmt.Errorf("%w: %s: %s is empty; remove the key to use the default", ErrInvalid, path, key)
	}
	if !filepath.IsAbs(v) {
		return "", fmt.Errorf("%w: %s: %s must be an absolute path, got %q", ErrInvalid, path, key, v)
	}
	return v, nil
}
