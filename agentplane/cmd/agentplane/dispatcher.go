package main

// `agentplane dispatcher` — v0 of the lifecycle loop (RELIABILITY.md contract).
// Deliberately minimal: auto-suspend idle minds (escrow-first) and WARN on
// unreachable-but-running ones. No auto-remediation of zombies in v0.

import (
	"context"
	"flag"
	"log"
	"strings"
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
	for {
		dispatcherPass(sc, *idleAfter)
		if *once {
			return
		}
		time.Sleep(*interval)
	}
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
			if !isSession || !strings.HasPrefix(name, "b-") ||
				a.GetMetadata().GetAtespace() != sc.atespace ||
				a.GetStatus() != ateapipb.Actor_STATUS_RUNNING {
				continue
			}
			h, ok := probeHealth(sc, name)
			switch {
			case !ok:
				log.Printf("WARN %s: RUNNING but unreachable — possible zombie (manual suspend advised)", sid)
			case h.Busy || h.Queued > 0:
				// active — leave it alone (turn-boundary rule)
			default:
				idleFor := idleAfter // unknown last activity → conservative: treat as just-idle
				if t, err := time.Parse(time.RFC3339Nano, h.LastEventAt); err == nil {
					idleFor = time.Since(t)
				} else {
					continue // old image without last_event_at: skip rather than guess
				}
				if idleFor >= idleAfter {
					escrowTranscript(sc, sid, name)
					if _, err := ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
						Actor: &ateapipb.ObjectRef{Atespace: sc.atespace, Name: name},
					}); err != nil {
						log.Printf("dispatcher: suspend %s: %v", sid, err)
					} else {
						log.Printf("auto-suspended %s (idle %s)", sid, idleFor.Round(time.Second))
					}
				}
			}
		}
		token = resp.GetNextPageToken()
		if token == "" {
			return
		}
	}
}
