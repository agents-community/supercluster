package main

// `agentplane dispatcher` — v0 of the lifecycle loop (RELIABILITY.md contract).
// Deliberately minimal: auto-suspend idle minds (escrow-first) and WARN on
// unreachable-but-running ones. No auto-remediation of zombies in v0.

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"

	"github.com/quantumnode/agentplane/internal/naming"
)

func runDispatcher(args []string) {
	fs := flag.NewFlagSet("dispatcher", flag.ExitOnError)
	idleAfter := fs.Duration("idle-after", 120*time.Second, "suspend a mind idle for this long")
	interval := fs.Duration("interval", 30*time.Second, "pass interval")
	once := fs.Bool("once", false, "run a single pass and exit")
	_ = fs.Parse(args)

	sc := newSessionCtx()
	// The dispatcher is a separate process from serve, so it needs its own
	// handle on the session store to record usage before it suspends a mind.
	// Auto-sleep is how most sessions end, so without this the common case
	// still loses its cost (#42).
	if project := env("AGENTPLANE_PROJECT", os.Getenv("GOOGLE_CLOUD_PROJECT")); project != "" {
		if store, err := newFirestoreStore(context.Background(), project); err != nil {
			log.Printf("warn: usage capture disabled (%v) — suspended sessions will not record cost", err)
		} else {
			captureUsage = func(ctx context.Context, sid string, u json.RawMessage) {
				store.touch(ctx, sid, u)
			}
			log.Printf("usage capture enabled (firestore %s)", project)
		}
	}
	for {
		dispatcherPass(sc, *idleAfter)
		if *once {
			return
		}
		time.Sleep(*interval)
	}
}

// A session with no last_event_at has never emitted anything, so there is no
// activity clock to read. Remember when this process first saw it idle and
// measure from there; the entry is dropped once it suspends, so this cannot
// grow without bound. In memory on purpose: losing it on restart costs one
// extra idle window, which is the safe direction.
var firstSeen sync.Map // sid -> time.Time

func firstSeenIdle(sid string) time.Time {
	v, _ := firstSeen.LoadOrStore(sid, time.Now())
	t, _ := v.(time.Time)
	return t
}

func dispatcherPass(sc sessionCtx, idleAfter time.Duration) {
	ctrl, closeFn, err := sc.dial()
	if err != nil {
		log.Printf("dispatcher: %v", err)
		return
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// One sweep, two maps: running brains and running hands by session. The
	// brain drives the decision (it knows busy/idle); the hand follows it —
	// suspending only the brain would leave the hand pinning its pool worker,
	// halving the multiplexing win.
	runningBrains := map[string]string{} // sid → actor name
	runningHands := map[string]string{}
	token := ""
	for {
		resp, err := ctrl.ListActors(ctx, &ateapipb.ListActorsRequest{PageToken: token})
		if err != nil {
			log.Printf("dispatcher: list: %v", err)
			return
		}
		for _, a := range resp.GetActors() {
			name := a.GetMetadata().GetName()
			sid, isSession := naming.SessionFromActor(name)
			if !isSession || a.GetMetadata().GetAtespace() != sc.atespace ||
				a.GetStatus() != ateapipb.Actor_STATUS_RUNNING {
				continue
			}
			switch {
			case strings.HasPrefix(name, "b-"):
				runningBrains[sid] = name
			case strings.HasPrefix(name, "h-"):
				runningHands[sid] = name
			}
		}
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}

	suspend := func(sid, name string) {
		if _, err := ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
			Actor: &ateapipb.ObjectRef{Atespace: sc.atespace, Name: name},
		}); err != nil {
			log.Printf("dispatcher: suspend %s (%s): %v", name, sid, err)
		} else {
			log.Printf("auto-suspended %s (%s)", name, sid)
		}
	}

	for sid, name := range runningBrains {
		h, ok := probeHealth(sc, name)
		switch {
		case !ok:
			log.Printf("WARN %s: RUNNING but unreachable — possible zombie (manual suspend advised)", sid)
		case h.Busy || h.Queued > 0:
			// active — leave it alone (turn-boundary rule)
		default:
			idleFor := time.Duration(0)
			if t, err := time.Parse(time.RFC3339Nano, h.LastEventAt); err == nil {
				idleFor = time.Since(t)
			} else {
				// No last_event_at. This is NOT an old image — it is a session
				// that was created and never messaged, so the brain has emitted
				// nothing (`events: 0`). Skipping it, as this used to, meant such
				// a session pinned a pool worker FOREVER: five of them exhausted
				// the six-worker brain pool and blocked every new agent from
				// baking its golden.
				//
				// Measured from first sight instead, so a session still has a
				// full idle window between being created and being slept — the
				// gap where a user is about to send their first message.
				idleFor = time.Since(firstSeenIdle(sid))
			}
			if idleFor >= idleAfter {
				// This is the path most sessions actually take: nobody deletes
				// or suspends by hand, they just stop typing. `h` is the probe
				// we already did to read last_event_at, so the usage is in hand
				// with no extra call and no risk of waking anything (#42).
				if captureUsage != nil && len(h.Usage) > 0 {
					captureUsage(ctx, sid, h.Usage)
				}
				escrowTranscript(sc, sid, name)
				suspend(sid, name)
				firstSeen.Delete(sid)
				if hand, ok := runningHands[sid]; ok {
					suspend(sid, hand)
					delete(runningHands, sid)
				}
			}
		}
	}

	// Orphaned hands: running while their brain already sleeps (manual suspend,
	// or a pass predating hand-following). A hand does nothing without its
	// brain — put it to sleep too. Its state is all in the durable /workspace.
	for sid, hand := range runningHands {
		if _, brainAwake := runningBrains[sid]; !brainAwake {
			suspend(sid, hand)
		}
	}
}
