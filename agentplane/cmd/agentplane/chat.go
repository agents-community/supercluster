package main

// `agentplane chat` — the interactive front door. One command to talk to a
// durable mind, claude-code-style:
//
//	agentplane chat -agent buddy        mint a session and start chatting
//	agentplane chat -id sess-…          re-attach to an existing mind
//
// The REPL is turn-based: your line is sent, the agent's stream (text + tool
// activity) renders live, the prompt returns at turn end. Detaching (Ctrl-D or
// /quit) does NOT kill anything — the mind keeps its memory and can be
// re-attached any time, even after a suspend (sends auto-wake it). That
// difference from a normal CLI chat is the product.

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/quantumnode/agentplane/internal/naming"
)

const (
	cDim   = "\033[2m"
	cBold  = "\033[1m"
	cCyan  = "\033[36m"
	cReset = "\033[0m"
)

func runChat(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	agent := fs.String("agent", "", "agent to mint a NEW session from")
	id := fs.String("id", "", "existing session id (sess-…) to re-attach")
	_ = fs.Parse(args)
	sc := newSessionCtx()

	sid := *id
	switch {
	case sid == "" && *agent == "":
		fmt.Fprintln(os.Stderr, "chat: pass -agent <name> (new session) or -id sess-… (re-attach)")
		os.Exit(2)
	case sid == "":
		var err error
		if sid, err = createSession(context.Background(), sc, *agent); err != nil {
			log.Fatalf("chat: %v", err)
		}
		fmt.Printf("%snew session %s (agent %s)%s\n", cDim, sid, *agent, cReset)
	case !naming.IsSessionID(sid):
		fmt.Fprintln(os.Stderr, "chat: -id must be a valid session id (sess-…)")
		os.Exit(2)
	}
	brain := naming.BrainActor(sid)

	// On re-attach, show the tail of the conversation for context.
	cursor := ""
	if events := chatHistory(sc, brain); len(events) > 0 {
		fmt.Printf("%s── recent history ──%s\n", cDim, cReset)
		start := len(events) - 6
		if start < 0 {
			start = 0
		}
		for _, ev := range events[start:] {
			printEvent(ev, true)
		}
		cursor, _ = events[len(events)-1]["id"].(string)
		fmt.Printf("%s────────────────────%s\n", cDim, cReset)
	}

	fmt.Printf("%sdurable mind attached — detach with Ctrl-D or /quit (the mind keeps living)%s\n", cDim, cReset)
	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Printf("%s%s>%s ", cBold, cCyan, cReset)
		if !stdin.Scan() {
			break // Ctrl-D
		}
		line := strings.TrimSpace(stdin.Text())
		switch line {
		case "":
			continue
		case "/quit", "/exit", "/q":
			goto detach
		case "/suspend":
			sessionSuspend(sc, []string{"-id", sid}, false)
			continue
		}
		if err := chatSend(sc, brain, line); err != nil {
			fmt.Fprintf(os.Stderr, "send failed: %v\n", err)
			continue
		}
		cursor = streamTurn(sc, brain, cursor)
	}
detach:
	fmt.Printf("\n%sdetached. The mind lives on:%s\n  re-attach:  agentplane chat -id %s\n  sleep it:   agentplane session suspend -id %s\n", cDim, cReset, sid, sid)
}

// chatSend posts one user message (finding #3: retry 5xx wake races).
func chatSend(sc sessionCtx, brain, text string) error {
	body, _ := json.Marshal(map[string]string{"message": text})
	hc := &http.Client{Timeout: 60 * time.Second}
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("http://%s/v1/sessions/%s/events", sc.atenet, brain), strings.NewReader(string(body)))
		req.Host = naming.ActorDNS(brain, sc.atespace)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if attempt < 4 {
			fmt.Printf("%s(waking the mind…)%s\n", cDim, cReset)
			time.Sleep(5 * time.Second)
		}
	}
	return lastErr
}

// streamTurn renders SSE events after `cursor` until the turn ends; returns
// the new cursor. A turn deadline plus 2min bounds a wedged stream.
func streamTurn(sc sessionCtx, brain, cursor string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	u := fmt.Sprintf("http://%s/v1/sessions/%s/events/stream", sc.atenet, brain)
	if cursor != "" {
		u += "?since=" + url.QueryEscape(cursor)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Host = naming.ActorDNS(brain, sc.atespace)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stream: %v\n", err)
		return cursor
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line[6:]), &ev) != nil {
			continue
		}
		if idv, _ := ev["id"].(string); idv != "" {
			cursor = idv
		}
		printEvent(ev, false)
		switch ev["type"] {
		case "session.status_idle", "session.error":
			return cursor
		}
	}
	return cursor
}

func printEvent(ev map[string]any, history bool) {
	text := ""
	if cs, ok := ev["content"].([]any); ok && len(cs) > 0 {
		if c0, ok := cs[0].(map[string]any); ok {
			text, _ = c0["text"].(string)
		}
	}
	switch ev["type"] {
	case "user.message":
		if history {
			fmt.Printf("%s> %s%s\n", cDim, text, cReset)
		}
	case "agent.message":
		fmt.Printf("%s\n", text)
	case "agent.tool_use":
		name, _ := ev["name"].(string)
		fmt.Printf("%s⚙ %s%s\n", cDim, name, cReset)
	case "session.error":
		if e, ok := ev["error"].(map[string]any); ok {
			fmt.Printf("%s! %v%s\n", cDim, e["message"], cReset)
		}
	}
}

// chatHistory fetches the persisted event log (wakes a sleeping mind).
func chatHistory(sc sessionCtx, brain string) []map[string]any {
	data, err := fetchEvents(sc, brain)
	if err != nil {
		return nil
	}
	var payload struct {
		Events []map[string]any `json:"events"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	return payload.Events
}
