# AgentPlane

A runtime platform for **durable agent minds** — persistent harness sessions
(Claude Code, Codex, **pi**) inside checkpointable sandboxes (Agent Substrate
on GKE). Minds survive suspension with full in-memory state, resume in
milliseconds, cost ~nothing while asleep, and speak a stable session-events
dialect over SSE.

```text
$ agentplane session new
session: sess-x7k2m9qw4a

$ agentplane session send -id sess-x7k2m9qw4a -m "Remember: MOONRIVER-7"
$ agentplane session suspend -id sess-x7k2m9qw4a     # mind → GCS object
# …days later…
$ agentplane session send -id sess-x7k2m9qw4a -m "Codeword?"
# → "MOONRIVER-7"   (resumed from checkpoint in ~1s, memory intact)
```

- **[Concepts](concepts.md)** — the building blocks: durable sessions, agents, harnesses, brain/hand, the broker
- **[Using the API](api-usage.md)** — a task-oriented walkthrough of the control-plane API
- **[Architecture](architecture.md)** — components, protocols, and trust boundaries
- **[AgentSpec](agents.md)** — defining agents; harness templates in `examples/`
- **[API reference](api.md)** — every endpoint, error, and auth rule
- **[Observability](observability.md)** — end-to-end tracing (serve → brain → hand)
