package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run --version: %v", err)
	}
	if got := stdout.String(); !strings.HasPrefix(got, "counterpoint ") {
		t.Fatalf("stdout = %q, want prefix %q", got, "counterpoint ")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunRejectsPositionalArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"serve"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run(serve) = nil error, want error")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty: stdout is reserved for protocol data", stdout.String())
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--bogus"}, &stdout, &stderr); err == nil {
		t.Fatal("run --bogus: want error, got nil")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}
