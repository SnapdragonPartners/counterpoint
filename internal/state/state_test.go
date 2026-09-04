package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "nested", "state.json"))
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	st := tempStore(t)
	s, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Workflows) != 0 {
		t.Fatalf("Workflows = %v, want empty", s.Workflows)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	st := tempStore(t)
	s, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Workflow{
		ThreadID: "thr_1", LastCommit: "c1", LastBase: "b1", LastRequestHash: "h1",
		Round: 2, LastReview: "looks good\nwith newline", LastWarnings: []string{"declined x"},
	}
	s.Put("/repo/.git::refs/heads/f", want)
	if err := st.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(st.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("state file mode = %o, want %o", perm, filePerm)
	}
	dirInfo, err := os.Stat(filepath.Dir(st.Path()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != dirPerm {
		t.Errorf("state dir mode = %o, want %o", perm, dirPerm)
	}

	again, err := NewStore(st.Path()).Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := again.Get("/repo/.git::refs/heads/f")
	if !ok {
		t.Fatal("workflow missing after reload")
	}
	if got.ThreadID != want.ThreadID || got.Round != want.Round || got.LastReview != want.LastReview ||
		got.LastCommit != want.LastCommit || got.LastBase != want.LastBase || got.LastRequestHash != want.LastRequestHash ||
		len(got.LastWarnings) != 1 || got.LastWarnings[0] != "declined x" {
		t.Errorf("reloaded = %+v, want %+v", got, want)
	}

	data, err := os.ReadFile(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 1`) {
		t.Errorf("state file lacks version envelope:\n%s", data)
	}
}

func TestSaveLeavesNoTemporaryFiles(t *testing.T) {
	st := tempStore(t)
	s, _ := st.Load()
	for i := 0; i < 3; i++ {
		s.Put("k", Workflow{Round: i})
		if err := st.Save(s); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(st.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %v, want only state.json", names)
	}
}

func TestSaveReplacesExistingContent(t *testing.T) {
	st := tempStore(t)
	s, _ := st.Load()
	s.Put("a", Workflow{Round: 1})
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	s.Workflows = map[string]Workflow{"b": {Round: 5}}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := NewStore(st.Path()).Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Get("a"); ok {
		t.Error("old workflow a survived a full replacement")
	}
	if w, ok := got.Get("b"); !ok || w.Round != 5 {
		t.Errorf("b = %+v, %v; want round 5", w, ok)
	}
}

func TestMalformedStateIsReportedAndNeverOverwritten(t *testing.T) {
	st := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(st.Path()), dirPerm); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("{not json")
	if err := os.WriteFile(st.Path(), garbage, filePerm); err != nil {
		t.Fatal(err)
	}

	_, err := st.Load()
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Load error = %v, want ErrMalformed", err)
	}
	if !strings.Contains(err.Error(), st.Path()) {
		t.Errorf("error %q should name the file", err)
	}

	err = st.Save(&State{Workflows: map[string]Workflow{"k": {Round: 1}}})
	if !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Save after failed Load error = %v, want ErrNotLoaded", err)
	}
	data, readErr := os.ReadFile(st.Path())
	if readErr != nil || string(data) != string(garbage) {
		t.Errorf("malformed file changed: %q, %v", data, readErr)
	}
}

func TestUnsupportedVersionIsRejectedAndPreserved(t *testing.T) {
	st := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(st.Path()), dirPerm); err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"version": 2, "workflows": {}}` + "\n")
	if err := os.WriteFile(st.Path(), future, filePerm); err != nil {
		t.Fatal(err)
	}

	_, err := st.Load()
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("Load error = %v, want ErrUnsupportedVersion", err)
	}
	if err := st.Save(&State{}); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Save error = %v, want ErrNotLoaded", err)
	}
	data, _ := os.ReadFile(st.Path())
	if string(data) != string(future) {
		t.Errorf("future-version file changed: %q", data)
	}
}

func TestOversizedStateIsRejectedAndPreserved(t *testing.T) {
	st := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(st.Path()), dirPerm); err != nil {
		t.Fatal(err)
	}
	// A sparse file reports a huge size without consuming disk.
	f, err := os.OpenFile(st.Path(), os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version": 1, "workflows": {}}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxStateFileSize + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = st.Load()
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Load error = %v, want ErrTooLarge", err)
	}
	if !strings.Contains(err.Error(), st.Path()) {
		t.Errorf("error %q should name the file", err)
	}
	if err := st.Save(&State{}); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Save error = %v, want ErrNotLoaded", err)
	}
	info, err := os.Stat(st.Path())
	if err != nil || info.Size() != MaxStateFileSize+1 {
		t.Errorf("oversized file changed: size %d, %v", info.Size(), err)
	}
}

func TestExactlyMaxSizeIsAccepted(t *testing.T) {
	st := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(st.Path()), dirPerm); err != nil {
		t.Fatal(err)
	}
	// Valid JSON padded with trailing whitespace to exactly the limit.
	body := `{"version": 1, "workflows": {}}`
	data := append([]byte(body), make([]byte, MaxStateFileSize-len(body))...)
	for i := len(body); i < len(data); i++ {
		data[i] = ' '
	}
	if err := os.WriteFile(st.Path(), data, filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(); err != nil {
		t.Fatalf("Load at exactly the limit: %v", err)
	}
}

func TestSaveRefusesOversizedStateAndKeepsPrevious(t *testing.T) {
	st := tempStore(t)
	s, _ := st.Load()
	s.Put("k", Workflow{Round: 1})
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(st.Path())
	if err != nil {
		t.Fatal(err)
	}

	s.Put("k", Workflow{Round: 2, LastReview: strings.Repeat("x", MaxStateFileSize)})
	err = st.Save(s)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Save oversized error = %v, want ErrTooLarge", err)
	}
	after, err := os.ReadFile(st.Path())
	if err != nil || string(after) != string(before) {
		t.Errorf("previous state not preserved after refused save: %v", err)
	}
	if _, err := NewStore(st.Path()).Load(); err != nil {
		t.Errorf("state unreadable after refused save: %v", err)
	}
}

func TestSaveRejectsNilState(t *testing.T) {
	st := tempStore(t)
	if _, err := st.Load(); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(nil); err == nil {
		t.Fatal("Save(nil) = nil error, want error")
	}
	if _, err := os.Stat(st.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file created by refused save: %v", err)
	}
}

func TestSaveWithoutLoadIsRefused(t *testing.T) {
	st := tempStore(t)
	if err := st.Save(&State{}); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Save without Load error = %v, want ErrNotLoaded", err)
	}
	if _, err := os.Stat(st.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file created despite refusal: %v", err)
	}
}

func TestReplay(t *testing.T) {
	s := &State{}
	s.Put("k", Workflow{LastRequestHash: "abc", LastReview: "r", Round: 3})

	if w, ok := s.Replay("k", "abc"); !ok || w.LastReview != "r" || w.Round != 3 {
		t.Errorf("Replay(matching) = %+v, %v; want stored workflow", w, ok)
	}
	if _, ok := s.Replay("k", "other"); ok {
		t.Error("Replay(different hash) = true, want false")
	}
	if _, ok := s.Replay("missing", "abc"); ok {
		t.Error("Replay(missing key) = true, want false")
	}
	s.Put("empty", Workflow{})
	if _, ok := s.Replay("empty", ""); ok {
		t.Error("Replay(empty hash against empty record) = true, want false")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv(EnvStatePath, "/abs/state.json")
	p, err := DefaultPath()
	if err != nil || p != "/abs/state.json" {
		t.Errorf("DefaultPath with override = %q, %v", p, err)
	}

	t.Setenv(EnvStatePath, "relative/state.json")
	if _, err := DefaultPath(); err == nil {
		t.Error("DefaultPath accepted a relative override")
	}

	t.Setenv(EnvStatePath, "")
	p, err = DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if !filepath.IsAbs(p) || filepath.Base(p) != stateFileName || filepath.Base(filepath.Dir(p)) != configSubdir {
		t.Errorf("DefaultPath = %q, want <config>/%s/%s", p, configSubdir, stateFileName)
	}
}

func TestLockPathIsBesideStateFile(t *testing.T) {
	st := NewStore("/x/y/state.json")
	if got := st.LockPath(); got != "/x/y/state.json.lock" {
		t.Errorf("LockPath = %q", got)
	}
}
