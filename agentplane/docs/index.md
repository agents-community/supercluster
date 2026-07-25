# AgentPlane

A runtime platform for **durable agent minds** — persistent harness sessions
(Claude Code, Codex, OpenCode) inside checkpointable sandboxes (Agent Substrate
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

- **[Brain server](brain-server.md)** — architecture of a durable mind
- **[Reliability runbook](runbook-brain-reliability.md)** — drills + findings log
