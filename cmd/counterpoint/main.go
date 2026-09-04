// Command counterpoint is an MCP stdio server that hands local commits to a
// persistent Codex review thread. See docs/MVP.md for the accepted contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/SnapdragonPartners/counterpoint/internal/mcpserver"
	"github.com/SnapdragonPartners/counterpoint/internal/review"
	"github.com/SnapdragonPartners/counterpoint/internal/state"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev" //nolint:gochecknoglobals // build-time injection target

func main() {
	os.Exit(realMain())
}

// realMain exists so deferred cleanup runs before the process exits; os.Exit
// in main would skip it.
func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// run parses args and dispatches. Stdout is reserved for MCP protocol data;
// --version writes to it directly and the server writes nothing else to it.
// All diagnostics go to stderr.
func run(ctx context.Context, args []string, stdin io.ReadCloser, stdout io.Writer, stderr io.Writer) error {
	fs := flag.NewFlagSet("counterpoint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *showVersion {
		fmt.Fprintf(stdout, "counterpoint %s\n", version)
		return nil
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %v: counterpoint takes no positional arguments", fs.Args())
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	statePath, err := state.DefaultPath()
	if err != nil {
		return err
	}
	svc := review.New(review.Options{Store: state.NewStore(statePath), Logger: log, Version: version})
	log.Info("counterpoint serving MCP on stdio", "version", version, "state", statePath)
	return mcpserver.Serve(ctx, mcpserver.New(ctx, svc, version, log), stdin, nopCloser{stdout})
}

// nopCloser adapts the injected stdout to the transport's WriteCloser
// without closing the real stream on session end.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
