package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/counterpoint/internal/review"
)

// write puts body at a temporary path and points EnvPath at it.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvPath, path)
	return path
}

func TestLoadWithoutAFileUsesDefaults(t *testing.T) {
	t.Setenv(EnvPath, filepath.Join(t.TempDir(), "absent.json"))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReviewEffort != review.DefaultReasoningEffort {
		t.Errorf("ReviewEffort = %q, want %q", cfg.ReviewEffort, review.DefaultReasoningEffort)
	}
	if cfg.StateFile != "" || cfg.CheckoutDir != "" {
		t.Errorf("paths = %q, %q, want both empty", cfg.StateFile, cfg.CheckoutDir)
	}
	if cfg.Path != "" {
		t.Errorf("Path = %q, want empty when no file exists", cfg.Path)
	}
}

func TestLoadAccepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Config
	}{
		{"empty object", `{}`, Config{ReviewEffort: review.DefaultReasoningEffort}},
		{"effort only", `{"review_effort":"low"}`, Config{ReviewEffort: "low"}},
		{"ceiling", `{"review_effort":"xhigh"}`, Config{ReviewEffort: "xhigh"}},
		{
			"every key",
			`{"review_effort":"medium","state_file":"/s/state.json","checkout_dir":"/c"}`,
			Config{ReviewEffort: "medium", StateFile: "/s/state.json", CheckoutDir: "/c"},
		},
		{
			"paths only",
			`{"state_file":"/s/state.json","checkout_dir":"/c"}`,
			Config{ReviewEffort: review.DefaultReasoningEffort, StateFile: "/s/state.json", CheckoutDir: "/c"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, tc.body)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.want.Path = path
			if cfg != tc.want {
				t.Errorf("Load() = %+v, want %+v", cfg, tc.want)
			}
		})
	}
}

func TestLoadRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not json", `not json`},
		{"array", `[]`},
		{"string", `"review_effort"`},
		{"unknown key", `{"effort":"low"}`},
		{"wrong spelling convention", `{"reviewEffort":"low"}`},
		{"trailing object", `{"review_effort":"low"} {"review_effort":"max"}`},
		{"trailing junk", `{} nonsense`},
		{"wrong type", `{"review_effort":4}`},
		// The ceiling is the point of the accepted set: levels above
		// xhigh exist on some models and must not be selectable.
		{"above the ceiling", `{"review_effort":"max"}`},
		{"unknown effort", `{"review_effort":"ultra"}`},
		{"wrong case", `{"review_effort":"XHIGH"}`},
		{"empty effort", `{"review_effort":""}`},
		{"relative state file", `{"state_file":"state.json"}`},
		{"relative checkout dir", `{"checkout_dir":"checkouts"}`},
		{"empty state file", `{"state_file":""}`},
		{"empty checkout dir", `{"checkout_dir":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, tc.body)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %s", tc.body)
			} else if !errors.Is(err, ErrInvalid) {
				t.Errorf("Load error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLoadRejectsAnOversizedFile(t *testing.T) {
	write(t, `{"review_effort":"low","state_file":"/s/`+strings.Repeat("x", MaxBytes)+`"}`)
	_, err := Load()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error does not name the limit: %v", err)
	}
}

func TestLoadRejectsANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvPath, dir)
	if _, err := Load(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load on a directory = %v, want ErrInvalid", err)
	}
}

func TestPathRejectsARelativeOverride(t *testing.T) {
	t.Setenv(EnvPath, "config.json")
	if _, err := Path(); err == nil {
		t.Error("Path accepted a relative override")
	}
}

func TestPathDefaultsBesideTheStateFile(t *testing.T) {
	t.Setenv(EnvPath, "")
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if !filepath.IsAbs(p) || filepath.Base(p) != fileName || filepath.Base(filepath.Dir(p)) != configSubdir {
		t.Errorf("Path = %q, want <config>/%s/%s", p, configSubdir, fileName)
	}
}

// The error names the accepted set, so a user who picks a wrong value is
// told what the right ones are rather than only that theirs is wrong.
func TestRejectionNamesTheAcceptedEfforts(t *testing.T) {
	write(t, `{"review_effort":"max"}`)
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an effort above the ceiling")
	}
	for _, want := range review.AcceptedEfforts() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}
