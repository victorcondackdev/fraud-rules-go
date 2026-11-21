// Command riskd serves real-time Pix risk decisions over HTTP.
//
//	riskd [-addr :8080] [-ruleset rulesets/pix-default.json] [-budget 25ms] [-log-level info]
//
// Endpoints:
//
//	POST /v1/evaluate           decide on a transaction
//	POST /v1/events             record settled transfers (object or array)
//	GET  /v1/rulesets           revisions, newest first
//	GET  /v1/rulesets/active    the active ruleset document
//	PUT  /v1/rulesets           validate and hot-swap a ruleset
//	POST /v1/rulesets/rollback  return to the previous revision
//	POST /v1/rules/validate     type-check one expression, with positions
//	GET  /metrics               Prometheus text format
//	GET  /healthz               200 once a ruleset is active
//
// SIGHUP reloads the -ruleset file; an invalid file is rejected and the
// active revision keeps serving.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/engine"
	"github.com/victorcondackdev/fraud-rules-go/internal/server"
	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	os.Exit(run(ctx, os.Args[1:], os.Stderr, hup, nil))
}

// run starts the server and blocks until ctx is done. If ready is not nil
// it receives the listening address once the server accepts connections.
func run(ctx context.Context, args []string, stderr io.Writer, reload <-chan os.Signal, ready chan<- string) int {
	fs := flag.NewFlagSet("riskd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":8080", "listen address")
	rulesetPath := fs.String("ruleset", "rulesets/pix-default.json", "ruleset file loaded at start and on SIGHUP")
	budget := fs.Duration("budget", 25*time.Millisecond, "latency budget per evaluation (0 disables)")
	level := fs.String("log-level", "info", "debug, info, warn or error")
	sweep := fs.Duration("sweep", time.Minute, "interval between feature store TTL sweeps")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*level)); err != nil {
		fmt.Fprintln(stderr, "riskd: invalid -log-level:", *level)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl}))

	store, err := velocity.New(velocity.Config{})
	if err != nil {
		log.Error("feature store", "error", err)
		return 1
	}
	eng := engine.New(store, engine.Options{Budget: *budget, Logger: log})
	load := func() error {
		b, err := os.ReadFile(*rulesetPath)
		if err != nil {
			return err
		}
		_, err = eng.LoadJSON(b)
		return err
	}
	if err := load(); err != nil {
		log.Error("loading ruleset", "path", *rulesetPath, "error", err)
		return 1
	}

	srv := server.New(eng, store, log)
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "addr", *addr, "error", err)
		return 1
	}
	log.Info("riskd listening", "addr", ln.Addr().String(), "version", version)
	if ready != nil {
		ready <- ln.Addr().String()
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()

	ticker := time.NewTicker(*sweep)
	defer ticker.Stop()
	for {
		select {
		case <-reload:
			if err := load(); err != nil {
				log.Error("reload rejected; keeping active ruleset", "path", *rulesetPath, "error", err)
			}
		case <-ticker.C:
			if n := store.Sweep(time.Now()); n > 0 {
				log.Debug("velocity sweep", "evicted", n)
			}
		case err := <-errc:
			log.Error("server stopped", "error", err)
			return 1
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := hs.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("shutdown", "error", err)
				return 1
			}
			log.Info("riskd stopped")
			return 0
		}
	}
}
