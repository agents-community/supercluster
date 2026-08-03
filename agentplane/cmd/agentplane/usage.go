package main

// Usage capture at the lifecycle points the platform controls (#42).
//
// Usage used to reach the store only via GET /v1/sessions/{id}, and only when
// the session happened to be awake — that path reads it from a live probe. So
// the ordinary case lost it entirely: a user chats, closes the client, the
// dispatcher suspends the mind, and nobody ever ran a GET. The session's cost
// was never recorded, and the delete-time rollup folded in nothing, leaving a
// lifetime total that was quietly wrong. A cost figure that under-counts in
// silence is worse than none, because it still looks authoritative.
//
// Suspend and delete are the two moments the platform always drives, and at
// both the actor is still awake and already being talked to. Capturing there
// costs no extra probe and — critically — never touches a mind that is already
// asleep, which is the constraint the whole session store was built around.

import (
	"context"
	"encoding/json"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// captureUsage stores a session's latest usage totals. Set by `serve` at
// startup; nil under the plain CLI, which has no store to write to.
var captureUsage func(ctx context.Context, sid string, usage json.RawMessage)

// recordUsageIfAwake reads a session's usage and persists it, but ONLY when the
// brain is already running.
//
// The status check is the whole point: probing a SUSPENDED actor resumes it,
// which would burn a checkpoint restore to collect a cost number and undo the
// auto-sleep that just freed the worker. If the mind is asleep we skip — its
// usage was already captured on the way down.
func recordUsageIfAwake(ctx context.Context, ctrl ateapipb.ControlClient, sc sessionCtx, sid, brain string) {
	if captureUsage == nil || ctrl == nil {
		return
	}
	resp, err := ctrl.GetActor(ctx, &ateapipb.GetActorRequest{
		Actor: &ateapipb.ObjectRef{Atespace: sc.atespace, Name: brain},
	})
	if err != nil || resp.GetStatus().String() != "STATUS_RUNNING" {
		return // absent or already asleep — never wake it just to read a number
	}
	h, ok := probeHealth(sc, brain)
	if !ok || len(h.Usage) == 0 {
		return
	}
	captureUsage(ctx, sid, h.Usage)
}
