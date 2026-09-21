// Command counterpoint is an MCP stdio server that hands local commits to a
// persistent Codex review thread. See docs/SPEC.md for the accepted contract.
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

	"github.com/SnapdragonPartners/counterpoint/internal/config"
	"github.com/SnapdragonPartners/counterpoint/internal/mcpserver"
	"github.com/SnapdragonPartners/counterpoint/internal/review"
	"github.com/SnapdragonPartners/counterpoint/internal/scratch"
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
	showUsage := fs.Bool("usage", false, "print recorded token usage and exit")
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
	// Configuration is loaded and validated before anything else starts,
	// so a bad value fails here rather than part-way through a review.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	statePath, err := state.ResolvePath(cfg.StateFile)
	if err != nil {
		return err
	}
	// A report, not a server: it reads the state file and exits without
	// starting the MCP transport.
	if *showUsage {
		st, lerr := state.NewStore(statePath).Load()
		if lerr != nil {
			return lerr
		}
		return writeUsage(stdout, st, statePath)
	}
	checkoutRoot, err := scratch.ResolveRoot(cfg.CheckoutDir)
	if err != nil {
		return err
	}
	svc := review.New(review.Options{
		Store:           state.NewStore(statePath),
		Logger:          log,
		Version:         version,
		CheckoutRoot:    checkoutRoot,
		ReasoningEffort: cfg.ReviewEffort,
	})
	// configFile is empty when no file exists, which is the normal case
	// and not a warning; it names the source of a surprising setting.
	log.Info("counterpoint serving MCP on stdio", "version", version, "state", statePath,
		"checkouts", checkoutRoot, "effort", cfg.ReviewEffort, "config_file", cfg.Path)
	return mcpserver.Serve(ctx, mcpserver.New(ctx, svc, version, log), stdin, nopCloser{stdout})
}

// nopCloser adapts the injected stdout to the transport's WriteCloser
// without closing the real stream on session end.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
