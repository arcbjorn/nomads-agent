// Command nomads-mcp serves the Nomads.com integration to AI agents over MCP.
//
// It speaks JSON-RPC on stdin/stdout, so it is launched by the agent itself and
// never listens on a network port.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/arcbjorn/nomads-agent/internal/client"
	"github.com/arcbjorn/nomads-agent/internal/mcp"
	"github.com/arcbjorn/nomads-agent/internal/storage"
	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

func main() {
	configDir := flag.String("config", "", "config directory (default ~/.config/nomads-agent)")
	debug := flag.Bool("debug", false, "verbose, redacted logging on stderr")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Diagnostics go to stderr; stdout is reserved for the JSON-RPC stream.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if *debug {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	srv := mcp.NewServer(func() (*client.Hybrid, error) {
		return buildClient(*configDir, logger, *debug)
	})

	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "nomads-mcp: %v\n", err)
		os.Exit(1)
	}
}

// buildClient assembles the hybrid client from stored or environment credentials.
func buildClient(configDir string, logger *slog.Logger, debug bool) (*client.Hybrid, error) {
	store, err := storage.New(configDir)
	if err != nil {
		return nil, err
	}
	sess, err := store.LoadSession()
	if err != nil {
		return nil, err
	}

	username := firstNonEmpty(os.Getenv("NOMADS_USERNAME"), sess.Username)
	apiKey := firstNonEmpty(os.Getenv("NOMADS_API_KEY"), sess.APIKey)

	var official *client.Official
	if username != "" && apiKey != "" {
		official, err = client.NewOfficial(username, apiKey, client.WithOfficialLogger(logger))
		if err != nil {
			return nil, err
		}
	}

	var private *client.Client
	if sess.HasBrowserSession() {
		private, err = client.New(sess, client.WithDebug(debug))
		if err != nil {
			return nil, err
		}
		if username == "" {
			if u, err := private.WhoAmI(context.Background()); err == nil {
				username = u
			}
		}
		private.SetUsername(username)
	}

	if official == nil && private == nil {
		return nil, nomads.Errorf(nomads.ErrAuthExpired,
			"no credentials configured; run 'nomads auth key --username <handle> --key <key>' "+
				"and optionally 'nomads auth login --link <magic link>'")
	}
	return client.NewHybrid(official, private)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
