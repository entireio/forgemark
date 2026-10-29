package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/entireio/forgemark/internal/server"
)

// runServe is `forgemark serve`: the local web GUI over the same engine the
// CLI drives.
func runServe(args []string) error {
	fs := flag.NewFlagSet("forgemark serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8377", "listen address; keep it loopback — this is a load-generation control panel")
	resultsDir := fs.String("results", "results", "directory for result JSON docs (shared with the CLI)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h/-help already printed usage; a help request isn't a failure
		}
		return err
	}
	// An empty host (":8377") is a wildcard bind — all interfaces — so it must
	// warn, not pass: server.IsLoopbackHost treats it as non-loopback.
	if host, _, err := net.SplitHostPort(*addr); err == nil && !server.IsLoopbackHost(host) {
		fmt.Fprintf(os.Stderr, "forgemark: WARNING: -addr %s is not loopback — anyone who can reach it can "+
			"drive benchmark runs. Credentials (pasted or CLI-sourced) and CLI discovery are refused on a "+
			"non-loopback bind, so only demo:// targets will run there. Only expose this on a network you trust.\n", *addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("forgemark: serving GUI on http://%s (results dir: %s)\n", *addr, *resultsDir)
	fmt.Println("           ctrl-c to stop")
	return server.New(*addr, *resultsDir).ListenAndServe(ctx)
}
