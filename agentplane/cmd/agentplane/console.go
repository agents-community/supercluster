// `agentplane console` — the platform team's fleet view, as its own process.
//
// Why not inside serve: serve is the user-facing control plane, reachable from
// the internet through an ingress that routes `/`. An operator view of every
// user's sessions has a different audience, a different blast radius and a
// different release cadence, and putting it on that process meant one mux edit
// away from publishing it. It is now a separate Deployment with no Service of
// its own, so nothing routes to it from outside the cluster.
//
// Why not on a laptop either: actors are NOT a Kubernetes resource — there is
// no actors.ate.dev CRD, they live in valkey behind ateapi, and ateapi
// authenticates callers with a projected Kubernetes ServiceAccount token
// (api.ate-system.svc:443). Nothing outside a pod can enumerate them, so a
// purely local console is not possible; this is the smallest in-cluster thing
// that can answer the question.
//
//	kubectl -n agentplane port-forward deploy/agentplane-console 7434:7434
//	open http://localhost:7434/admin
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func runConsole(args []string) {
	fs := flag.NewFlagSet("console", flag.ExitOnError)
	addr := fs.String("addr", env("AGENTPLANE_CONSOLE_ADDR", ":7434"), "listen address")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Tokens and the admin allowlist are the gate. Being un-routable is a
	// second layer, not the only one: a port-forward is available to anyone
	// with cluster access, which is a wider set than the admin allowlist.
	tokens := newTokenStore()
	admins := newAdminAllowlist()
	go func() {
		for range time.Tick(15 * time.Second) {
			tokens.reload()
			admins.reload()
		}
	}()

	s := &server{
		sc:     newSessionCtx(),
		tokens: tokens,
		admins: admins,
		log:    logger,
		client: &http.Client{},
		owners: newSessionStore(context.Background(),
			env("AGENTPLANE_PROJECT", os.Getenv("GOOGLE_CLOUD_PROJECT")),
			env("BRAIN_TEMPLATE_NS", "agentplane"), logger),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin", s.handleAdminConsole)
	mux.HandleFunc("GET /v1/admin/actors", s.auth(s.handleAdminActors))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	logger.Info("console: listening", "addr", *addr, "admins", admins.count(),
		"auth", tokens.enabled())
	if admins.count() == 0 {
		// Worth saying out loud: the console will render and then refuse every
		// request, which looks like a bug rather than a missing config.
		logger.Warn("console: the admin allowlist is EMPTY — every request will 404 " +
			"(set AGENTPLANE_ADMIN_EMAILS or mount the ConfigMap key)")
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("console: %v", err)
	}
}
