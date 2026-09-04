package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, os.Stdin, &stdout, &stderr); err != nil {
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
	err := run(context.Background(), []string{"serve"}, os.Stdin, &stdout, &stderr)
	if err == nil {
		t.Fatal("run(serve) = nil error, want error")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty: stdout is reserved for protocol data", stdout.String())
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--bogus"}, os.Stdin, &stdout, &stderr); err == nil {
		t.Fatal("run --bogus: want error, got nil")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// TestRunServesInjectedStreams drives the MCP handshake through injected
// pipes and checks that protocol bytes reach the injected stdout only.
func TestRunServesInjectedStreams(t *testing.T) {
	t.Setenv("COUNTERPOINT_STATE_FILE", t.TempDir()+"/state.json")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), nil, inR, outW, &stderr) }()

	go func() {
		_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`+"\n")
	}()
	// Read on a goroutine so a regression that writes nothing to the
	// injected stdout fails here instead of stalling the whole suite.
	type readResult struct {
		line string
		err  error
	}
	got := make(chan readResult, 1)
	go func() {
		line, err := bufio.NewReader(outR).ReadString('\n')
		got <- readResult{line, err}
	}()
	select {
	case r := <-got:
		if r.err != nil || !strings.Contains(r.line, `"serverInfo"`) {
			t.Fatalf("initialize response did not reach the injected stdout: %q, %v", r.line, r.err)
		}
	case <-time.After(10 * time.Second):
		_ = inW.Close()
		_ = outR.Close()
		t.Fatal("no initialize response on the injected stdout within 10s")
	}
	_ = inW.Close() // client disconnects; the server must exit
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after the client disconnected")
	}
	if strings.Contains(stderr.String(), `"serverInfo"`) {
		t.Error("protocol output leaked to stderr")
	}
}
