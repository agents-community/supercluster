// Package sandbox is agentplane's backend-neutral seam: the minimal contract
// every layer above (vendor adapters, identity/policy, CLI) programs against.
//
// DECISION: Agent Substrate is the PRIMARY backend — internal/backend/substrate
// implements these interfaces against ateapi (gRPC lifecycle) and atenet (HTTP
// exec routing). Other platforms (kubernetes-sigs/agent-sandbox, GKE Agent
// Sandbox) are design references only. This package must stay free of vendor
// SDKs and backend client types.
package sandbox

import "context"

// ExecResult is the outcome of running a command inside a sandbox.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Sandbox is one session's execution environment. Implementations are lazy:
// the backing instance is created/resumed on first use.
type Sandbox interface {
	Exec(ctx context.Context, command string) (ExecResult, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
	// Suspend checkpoints the sandbox (in-memory state → object storage).
	// No-op if never resumed.
	Suspend(ctx context.Context) error
}

// Provider mints per-session sandboxes.
type Provider interface {
	// Sandbox returns a lazy sandbox bound to `name`; created/resumed on its
	// first use.
	Sandbox(name string) Sandbox
	Close() error
}
