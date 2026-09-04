// Package state persists Counterpoint's per-workflow review state and
// serializes reviews across processes.
//
// The state file is a versioned JSON envelope keyed by workflow key (see
// docs/MVP.md). Writes are atomic: a temporary file in the same directory is
// written, synced, and renamed over the old file, so a crash leaves either the
// old complete state or the new complete state. A file that fails to parse is
// reported and never overwritten.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// Version is the state envelope version this package reads and writes.
	Version = 1

	// EnvStatePath is the single environment variable that overrides the
	// state file location, for tests and unusual installations.
	EnvStatePath = "COUNTERPOINT_STATE_FILE"

	// configSubdir is the directory under os.UserConfigDir.
	configSubdir = "counterpoint"
	// stateFileName is the state file name inside configSubdir.
	stateFileName = "state.json"

	dirPerm  = 0o700
	filePerm = 0o600

	// MaxStateFileSize bounds how much of the state file Load will read.
	// The file is untrusted input; a corrupt or hostile file must not be
	// able to exhaust memory before JSON validation. Reviews are text, so
	// this leaves ample room for many workflows.
	MaxStateFileSize = 16 << 20
)

// Sentinel errors. Wrapped errors name the file; match with errors.Is.
var (
	ErrMalformed          = errors.New("state file is malformed")
	ErrTooLarge           = errors.New("state file exceeds the size limit")
	ErrUnsupportedVersion = errors.New("state file version is not supported")
	ErrNotLoaded          = errors.New("state must be loaded before it is saved")
)

// Workflow is the persisted record for one repository-and-branch workflow.
type Workflow struct {
	ThreadID        string   `json:"thread_id"`
	LastCommit      string   `json:"last_commit"`
	LastBase        string   `json:"last_base"`
	LastRequestHash string   `json:"last_request_hash"`
	Round           int      `json:"round"`
	LastReview      string   `json:"last_review"`
	LastWarnings    []string `json:"last_warnings,omitempty"`
}

// State is the in-memory form of the state file.
type State struct {
	Workflows map[string]Workflow
}

// envelope is the on-disk shape.
type envelope struct {
	Version   int                 `json:"version"`
	Workflows map[string]Workflow `json:"workflows"`
}

// Get returns the workflow for key and whether it exists.
func (s *State) Get(key string) (Workflow, bool) {
	w, ok := s.Workflows[key]
	return w, ok
}

// Put records the workflow for key, replacing any existing record.
func (s *State) Put(key string, w Workflow) {
	if s.Workflows == nil {
		s.Workflows = map[string]Workflow{}
	}
	s.Workflows[key] = w
}

// Replay returns the stored workflow when the last completed request for
// key has the same hash, meaning the same review can be returned without a
// new Codex turn.
func (s *State) Replay(key, hash string) (Workflow, bool) {
	w, ok := s.Workflows[key]
	if !ok || hash == "" || w.LastRequestHash != hash {
		return Workflow{}, false
	}
	return w, true
}

// DefaultPath returns the state file path: EnvStatePath when set, otherwise
// the Counterpoint subdirectory of the user configuration directory.
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvStatePath); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", EnvStatePath, p)
		}
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, configSubdir, stateFileName), nil
}

// Store reads and writes one state file.
type Store struct {
	path string
	// loaded records that the file was read successfully (or was absent),
	// which is the precondition for Save: a malformed file is never
	// overwritten.
	loaded bool
}

// NewStore returns a store for the state file at path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path returns the state file path.
func (st *Store) Path() string { return st.path }

// LockPath returns the lock file path beside the state file.
func (st *Store) LockPath() string {
	return st.path + ".lock"
}

// Load reads the state file. A missing file yields empty state. A file that
// cannot be parsed, or whose version is not supported, is an error that
// names the file, and the store refuses to save until a later Load succeeds.
func (st *Store) Load() (*State, error) {
	st.loaded = false
	data, err := readBounded(st.path, MaxStateFileSize)
	if errors.Is(err, os.ErrNotExist) {
		st.loaded = true
		return &State{Workflows: map[string]Workflow{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", st.path, err)
	}

	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrMalformed, st.path, err)
	}
	if env.Version != Version {
		return nil, fmt.Errorf("%w: %s has version %d, want %d", ErrUnsupportedVersion, st.path, env.Version, Version)
	}
	if env.Workflows == nil {
		env.Workflows = map[string]Workflow{}
	}
	st.loaded = true
	return &State{Workflows: env.Workflows}, nil
}

// Save writes s atomically. It requires a prior successful Load on this
// store so that a file Load rejected is never replaced.
func (st *Store) Save(s *State) error {
	if !st.loaded {
		return fmt.Errorf("%w: %s", ErrNotLoaded, st.path)
	}
	env := envelope{Version: Version, Workflows: s.Workflows}
	if env.Workflows == nil {
		env.Workflows = map[string]Workflow{}
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(st.path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	if err := writeAtomic(dir, st.path, data); err != nil {
		return fmt.Errorf("write state %s: %w", st.path, err)
	}
	return nil
}

// readBounded reads path, returning ErrTooLarge if it holds more than
// limit bytes. It reads at most limit+1 bytes regardless of the file's
// reported size, so a sparse or growing file cannot force a large
// allocation.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // state file path is configured, not user input
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, limit)
	}
	return data, nil
}

// writeAtomic writes data to a temporary file in dir, syncs it, renames it
// over path, and syncs the directory. The temporary file is removed on any
// failure before the rename.
func writeAtomic(dir, path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(filePerm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	renamed = true

	return syncDir(dir)
}

// syncDir flushes a directory entry change so the rename is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}
	return nil
}
