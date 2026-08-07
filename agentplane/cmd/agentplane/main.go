// Command agentplane is the control-plane entry point: it manages sandboxes and
// brain sessions on the primary backend (Agent Substrate) and runs vendor
// adapters against them.
//
//	agentplane session <new|list|send|tail|suspend|delete>
//	                                           manage durable brain sessions (b-sess-<id>);
//	                                           see cmd/agentplane/session.go
//	agentplane worker                          run the Anthropic Managed Agents adapter
//	agentplane sbx exec -name <sb> -- <cmd>    run a command in a sandbox (created/resumed on demand)
//	agentplane sbx suspend -name <sb>          checkpoint a sandbox
//
// Backend config (env; port-forward defaults):
//
//	SUBSTRATE_ATEAPI       ateapi gRPC addr        (localhost:8080)
//	SUBSTRATE_ATENET       atenet HTTP addr        (localhost:8000)
//	SUBSTRATE_ATESPACE     atespace                (agents)
//	SUBSTRATE_TEMPLATE_NS  ActorTemplate namespace (ate-demo-sandbox; sbx only)
//	SUBSTRATE_TEMPLATE     ActorTemplate name      (sandbox-template; sbx only)
//	BRAIN_TEMPLATE_NS      brain template namespace (agentplane; session only)
//	BRAIN_TEMPLATE         brain template name      (brain; session only)
//
// Adapter config (worker):
//
//	ANTHROPIC_ENVIRONMENT_ID / ANTHROPIC_ENVIRONMENT_KEY   required
//	WORKER_CONCURRENCY (4)   INSTALL_SKILLS (1)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quantumnode/agentplane/internal/anthropicworker"
	substratebackend "github.com/quantumnode/agentplane/internal/backend/substrate"
	"github.com/quantumnode/agentplane/pkg/sandbox"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newProvider() sandbox.Provider {
	prov, err := substratebackend.NewProvider(substratebackend.Config{
		AteapiAddr:        env("SUBSTRATE_ATEAPI", "localhost:8080"),
		AtenetAddr:        env("SUBSTRATE_ATENET", "localhost:8000"),
		Atespace:          env("SUBSTRATE_ATESPACE", "agents"),
		TemplateNamespace: env("SUBSTRATE_TEMPLATE_NS", "ate-demo-sandbox"),
		TemplateName:      env("SUBSTRATE_TEMPLATE", "sandbox-template"),
	})
	if err != nil {
		log.Fatalf("substrate provider: %v", err)
	}
	return prov
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "worker":
		runWorker()
	case "sbx":
		runSbx(os.Args[2:])
	case "session":
		runSession(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	case "chat":
		runChat(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "token":
		runTokenCmd(os.Args[2:])
	case "configure":
		runConfigure(os.Args[2:])
	case "dispatcher":
		runDispatcher(os.Args[2:])
	case "console":
		runConsole(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentplane worker                          run the Anthropic adapter
  agentplane sbx exec -name <sb> -- <cmd>    run a command in a sandbox
  agentplane sbx suspend -name <sb>          checkpoint a sandbox
  agentplane chat -agent <name> | -id sess-…   plain REPL chat (pipe-friendly)
                                             (full TUI: tui/andromeda — npx @agentplane/andromeda)
  agentplane agent <create|list|delete>      manage agents (AgentSpec → template)
  agentplane serve [-addr :7433]             HTTP control-plane API (OTLP traces via OTEL_EXPORTER_OTLP_ENDPOINT)
  agentplane token <issue|list|revoke>       per-user access tokens for the API
  agentplane session <new|list|send|tail|events|suspend|delete>   manage brain sessions
  agentplane configure -api-key sk-…         set YOUR Anthropic key (BYO-key demos)
  agentplane dispatcher [-idle-after 2m] [-once]   auto-suspend idle minds (escrow-first)`)
	os.Exit(2)
}

func runWorker() {
	prov := newProvider()
	defer prov.Close()

	concurrency := 4
	if v := os.Getenv("WORKER_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			concurrency = n
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := anthropicworker.Run(ctx, anthropicworker.Config{
		EnvironmentID:  os.Getenv("ANTHROPIC_ENVIRONMENT_ID"),
		EnvironmentKey: os.Getenv("ANTHROPIC_ENVIRONMENT_KEY"),
		Provider:       prov,
		Concurrency:    concurrency,
		InstallSkills:  env("INSTALL_SKILLS", "1") != "0",
	})
	if err != nil {
		log.Fatalf("worker: %v", err)
	}
	log.Print("worker stopped cleanly")
}

func runSbx(args []string) {
	if len(args) < 1 {
		usage()
	}
	sub := args[0]
	fs := flag.NewFlagSet("sbx "+sub, flag.ExitOnError)
	name := fs.String("name", "", "sandbox name (required)")
	timeout := fs.Duration("timeout", 5*time.Minute, "operation timeout")
	_ = fs.Parse(args[1:])
	if *name == "" {
		fmt.Fprintln(os.Stderr, "sbx: -name is required")
		os.Exit(2)
	}

	prov := newProvider()
	defer prov.Close()
	sb := prov.Sandbox(*name)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch sub {
	case "exec":
		cmd := strings.Join(fs.Args(), " ")
		if cmd == "" {
			fmt.Fprintln(os.Stderr, "sbx exec: give a command after the flags, e.g. -name s1 -- uname -a")
			os.Exit(2)
		}
		r, err := sb.Exec(ctx, cmd)
		if err != nil {
			log.Fatalf("exec: %v", err)
		}
		fmt.Print(r.Stdout)
		if r.Stderr != "" {
			fmt.Fprint(os.Stderr, r.Stderr)
		}
		os.Exit(r.ExitCode)
	case "suspend":
		// Exec a no-op first so a never-resumed name still resolves to a real
		// sandbox, then checkpoint it.
		if _, err := sb.Exec(ctx, "true"); err != nil {
			log.Fatalf("resolve sandbox: %v", err)
		}
		if err := sb.Suspend(ctx); err != nil {
			log.Fatalf("suspend: %v", err)
		}
		fmt.Printf("sandbox %q suspended (checkpointed)\n", *name)
	default:
		usage()
	}
}
