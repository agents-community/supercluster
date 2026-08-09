package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestBrokerHandURL(t *testing.T) {
	// Disabled by default: empty base => empty URL => brain dials the hand directly.
	if got := brokerHandURL(sessionCtx{atespace: "agents"}, "sess-x"); got != "" {
		t.Fatalf("broker disabled should yield empty, got %q", got)
	}
	sc := sessionCtx{atespace: "agents", brokerMCPBase: "http://broker.agentplane.svc/mcp"}
	got := brokerHandURL(sc, "sess-abc")
	// It must carry the real hand's actor MCP URL, url-encoded, as ?hand=.
	want := "http://h-sess-abc.agents.actors.resources.substrate.ate.dev/mcp"
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result not a URL: %v", err)
	}
	if u.Query().Get("hand") != want {
		t.Fatalf("hand target = %q, want %q (from %q)", u.Query().Get("hand"), want, got)
	}
	if !strings.HasPrefix(got, "http://broker.agentplane.svc/mcp?") {
		t.Fatalf("must target the broker base, got %q", got)
	}
}

func TestBrokerHandURL_BaseWithQuery(t *testing.T) {
	// If the base already has a query, we must append with & not a second ?.
	sc := sessionCtx{atespace: "agents", brokerMCPBase: "http://broker.svc/mcp?v=1"}
	got := brokerHandURL(sc, "sess-1")
	if strings.Count(got, "?") != 1 {
		t.Fatalf("must not produce a second '?': %q", got)
	}
	u, _ := url.Parse(got)
	if u.Query().Get("v") != "1" || u.Query().Get("hand") == "" {
		t.Fatalf("both query params must survive: %q", got)
	}
}
