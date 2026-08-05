package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAtenetStatusAddrDerivesStatusPort(t *testing.T) {
	t.Setenv("SUBSTRATE_ATENET_STATUS", "")
	for _, tc := range []struct{ in, want string }{
		{"atenet-router.ate-system.svc:80", "atenet-router.ate-system.svc:4040"},
		{"atenet-router.ate-system.svc", "atenet-router.ate-system.svc:4040"},
		{"localhost:8000", "localhost:4040"},
	} {
		if got := atenetStatusAddr(tc.in); got != tc.want {
			t.Errorf("atenetStatusAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAtenetStatusAddrOverride(t *testing.T) {
	t.Setenv("SUBSTRATE_ATENET_STATUS", "elsewhere:9999")
	if got := atenetStatusAddr("atenet-router.ate-system.svc:80"); got != "elsewhere:9999" {
		t.Errorf("override ignored: got %q", got)
	}
}

// The whole point of #60: a template the cluster has and atenet does not means
// sessions pinned to it cannot be routed.
func TestMissingTemplatesFindsTheStaleCase(t *testing.T) {
	st := &atenetStatus{}
	st.Templates = append(st.Templates,
		struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}{"starter", "agentplane"},
		struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}{"sandbox-template", "ate-system"},
	)
	got := missingTemplates(st, "agentplane", []string{"starter", "starter-v2", "hand"})
	want := "hand,starter-v2"
	if strings.Join(got, ",") != want {
		t.Errorf("missing = %v, want %s", got, want)
	}
}

// atenet knowing MORE than the cluster is normal — its own sandbox template and
// other namespaces. Flagging that would make /readyz permanently red.
func TestMissingTemplatesIgnoresExtrasAndOtherNamespaces(t *testing.T) {
	st := &atenetStatus{}
	st.Templates = append(st.Templates,
		struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}{"starter", "agentplane"},
		struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}{"extra", "agentplane"},
		struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}{"starter", "other-ns"},
	)
	if got := missingTemplates(st, "agentplane", []string{"starter"}); len(got) != 0 {
		t.Errorf("expected no missing templates, got %v", got)
	}
	// Same name in a different namespace must not satisfy the check.
	if got := missingTemplates(st, "other-ns", []string{"starter", "extra"}); strings.Join(got, ",") != "extra" {
		t.Errorf("namespace scoping broken: got %v", got)
	}
}

func TestUnhealthySubsystemsNamesTheFailure(t *testing.T) {
	var st atenetStatus
	if err := json.Unmarshal([]byte(`{"health":{
		"envoy":{"healthy":true,"message":"LIVE"},
		"ate_api":{"healthy":false,"message":"connection refused"},
		"k8s_api":{"healthy":false,"message":"forbidden"}}}`), &st); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(unhealthySubsystems(&st), "; ")
	if got != "ate_api (connection refused); k8s_api (forbidden)" {
		t.Errorf("unhealthySubsystems = %q", got)
	}
	var healthy atenetStatus
	_ = json.Unmarshal([]byte(`{"health":{"envoy":{"healthy":true,"message":"LIVE"}}}`), &healthy)
	if got := unhealthySubsystems(&healthy); len(got) != 0 {
		t.Errorf("healthy atenet reported as unhealthy: %v", got)
	}
}

// Decoding must survive the fields this does not model — statusz also carries
// build info, flags, recent queries and parking state.
func TestFetchAtenetStatusToleratesUnmodelledFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json, got %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"build_tag":"dev","parking":{"enabled":true},
			"queries":[{"client":"x"}],
			"health":{"envoy":{"healthy":true,"message":"LIVE"}},
			"templates":[{"name":"starter","namespace":"agentplane"}]}`))
	}))
	defer srv.Close()
	t.Setenv("SUBSTRATE_ATENET_STATUS", strings.TrimPrefix(srv.URL, "http://"))

	st, err := fetchAtenetStatus(context.Background(), "unused:80")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(st.Templates) != 1 || st.Templates[0].Name != "starter" {
		t.Errorf("templates = %+v", st.Templates)
	}
	if !st.Health["envoy"].Healthy {
		t.Error("envoy should be healthy")
	}
}

func TestFetchAtenetStatusSurfacesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("SUBSTRATE_ATENET_STATUS", strings.TrimPrefix(srv.URL, "http://"))
	if _, err := fetchAtenetStatus(context.Background(), "unused:80"); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
}

// An unreachable atenet is itself the finding — serve routes every turn over
// that same network.
func TestCheckAtenetUnreachableIsNotReady(t *testing.T) {
	t.Setenv("SUBSTRATE_ATENET_STATUS", "127.0.0.1:1") // nothing listens here
	component, _, err := checkAtenet(context.Background(), sessionCtx{atenet: "unused:80"})
	if component != "atenet" {
		t.Errorf("component = %q, want atenet", component)
	}
	if err == nil {
		t.Error("expected the dial error to be reported")
	}
}
