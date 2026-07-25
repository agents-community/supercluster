// Package anthropicworker is agentplane's Anthropic Managed Agents adapter
// (Architecture B): Anthropic's control plane runs the agent loop and streams
// events; this worker executes the agent's tools inside sandboxes from the
// configured Provider.
//
// Each Managed Agents *session* maps deterministically to one sandbox (name
// derived from the session id), so every event sent to the same session resumes
// the SAME sandbox — its filesystem, installed packages and skills survive
// across suspend/resume.
//
// The SDK's EnvironmentWorker.Run polls SERIALLY and its ToolsFunc never sees
// the session id, so we drive our own WorkPoller loops (Concurrency of them)
// and call the public HandleItem per work item with a session-aware ToolsFunc
// closure — giving both concurrency (backend multiplexing) and the stable
// session→sandbox binding.
package anthropicworker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/lib/environments"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"

	"github.com/quantumnode/agentplane/pkg/sandbox"
)

// Config drives one adapter instance (one self-hosted environment).
type Config struct {
	EnvironmentID  string // env_… (self-hosted environment)
	EnvironmentKey string // sk-ant-oat01-…
	Provider       sandbox.Provider
	Concurrency    int  // concurrent pollers; ≥ pool size to see multiplexing (default 4)
	InstallSkills  bool // unpack the agent's skills into the sandbox on first use
	// ExtraTools, if set, returns additional per-session tools (e.g. a
	// credential-injecting gateway tool) appended to the standard toolset.
	ExtraTools func(sessionID string) []anthropic.BetaTool
	Log        *slog.Logger
}

// Run polls the environment's work queue until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	if cfg.EnvironmentID == "" || cfg.EnvironmentKey == "" {
		return fmt.Errorf("anthropicworker: EnvironmentID and EnvironmentKey are required")
	}
	if cfg.Provider == nil {
		return fmt.Errorf("anthropicworker: Provider is required")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	client := anthropic.NewClient(option.WithAuthToken(cfg.EnvironmentKey))
	host, _ := os.Hostname()

	cfg.Log.Info("anthropic adapter polling",
		slog.String("environment_id", cfg.EnvironmentID),
		slog.Int("concurrency", cfg.Concurrency),
		slog.Bool("skills", cfg.InstallSkills))

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			pollLoop(ctx, client, cfg, fmt.Sprintf("%s-agentplane-%d", host, n))
		}(i)
	}
	wg.Wait()
	return nil
}

// pollLoop runs one serial WorkPoller: claim a session, bind it to its stable
// sandbox, run its tools, repeat. N of these run concurrently (see Run).
func pollLoop(ctx context.Context, client anthropic.Client, cfg Config, workerID string) {
	poller := environments.NewWorkPoller(ctx, client, environments.WorkPollerOptions{
		EnvironmentID:  cfg.EnvironmentID,
		EnvironmentKey: cfg.EnvironmentKey,
		WorkerID:       workerID,
	})
	defer poller.Close()

	for poller.Next() {
		work := poller.Current()
		if work == nil {
			continue
		}
		sessionID := work.Data.ID
		name := SandboxName(sessionID)
		cfg.Log.Info("session claimed",
			slog.String("session_id", sessionID),
			slog.String("sandbox", name),
			slog.String("worker_id", workerID))

		itemWorker := environments.NewEnvironmentWorker(client, environments.EnvironmentWorkerOptions{
			EnvironmentID:  cfg.EnvironmentID,
			EnvironmentKey: cfg.EnvironmentKey,
			// Session-aware: the closure captures this session's stable sandbox,
			// so the same session always executes against the same one.
			ToolsFunc: func(_ *agenttoolset.AgentToolContext) []anthropic.BetaTool {
				sb := cfg.Provider.Sandbox(name)
				if cfg.InstallSkills {
					if err := installSkills(ctx, client, sb, sessionID, cfg.EnvironmentKey, cfg.Log); err != nil {
						cfg.Log.Warn("skills install failed",
							slog.String("session_id", sessionID), slog.Any("error", err))
					}
				}
				tools := Tools(sb)
				if cfg.ExtraTools != nil {
					tools = append(tools, cfg.ExtraTools(sessionID)...)
				}
				return tools
			},
		})
		if err := itemWorker.HandleItem(ctx, environments.HandleItemOptions{
			WorkID:         work.ID,
			EnvironmentID:  work.EnvironmentID,
			SessionID:      sessionID,
			EnvironmentKey: cfg.EnvironmentKey,
		}); err != nil {
			cfg.Log.Warn("handle item failed",
				slog.String("work_id", work.ID),
				slog.String("session_id", sessionID),
				slog.Any("error", err))
		}
	}
	if err := poller.Err(); err != nil {
		cfg.Log.Error("poller exited", slog.String("worker_id", workerID), slog.Any("error", err))
	}
}

// SandboxName derives a stable, DNS-safe sandbox name from a session id. It is
// deterministic so the same session always resumes the same sandbox; the name
// becomes part of a DNS label, so it must be lowercase [a-z0-9-] and ≤60 chars.
func SandboxName(sessionID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(sessionID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		name = "sess"
	}
	return name
}
