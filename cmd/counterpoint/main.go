// Command counterpoint is an MCP stdio server that hands local commits to a
// persistent Codex review thread. See docs/MVP.md for the accepted contract.
//
// The MCP server is not implemented yet. The binary currently supports only
// --version so that build, lint, and test automation have a real target.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev" //nolint:gochecknoglobals // build-time injection target

// errNotImplemented reports that the MCP server has not been built yet.
var errNotImplemented = errors.New("counterpoint: MCP server not implemented yet; see docs/MVP.md")

func main() {
	os.Exit(realMain())
}

// realMain exists so deferred cleanup runs before the process exits; os.Exit
// in main would skip it.
func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// run parses args and dispatches. It writes only to the supplied writers so
// tests can capture output and so stdout stays reserved for protocol data once
// the MCP server exists.
func run(_ context.Context, args []string, stdout, stderr io.Writer) error {
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

	return errNotImplemented
}
