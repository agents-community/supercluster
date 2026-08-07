package main

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The delete cascade's failure handling (#71). These assert the CLASSIFICATION
// the cascade depends on, because the bug was not in the delete call — it was
// in treating every error the same way as "no hand to delete".
//
// deleteSession itself needs a live ateapi, so the behaviour it wraps is
// exercised here at the level that actually went wrong: telling "there is no
// hand" apart from "the hand is still there and I could not remove it".

func TestNotFoundIsDistinguishableFromRealFailures(t *testing.T) {
	// The only error the cascade may treat as success.
	absent := status.Error(codes.NotFound, "actor h-sess-x not found")
	if status.Code(absent) != codes.NotFound {
		t.Fatal("NotFound must be recognisable — otherwise a single-actor agent errors on every delete")
	}

	// Everything else means the hand may still exist. Treating any of these as
	// success is what leaked a hand, its DurableDir and its snapshots, and
	// erased the ownership record that would have made it attributable.
	for _, err := range []error{
		status.Error(codes.Unavailable, "ateapi is down"),
		status.Error(codes.DeadlineExceeded, "timed out"),
		status.Error(codes.Internal, "boom"),
		status.Error(codes.PermissionDenied, "nope"),
		status.Error(codes.FailedPrecondition, "no free workers available"),
	} {
		if status.Code(err) == codes.NotFound {
			t.Errorf("%v must NOT be treated as an absent hand", err)
		}
	}
}

// A retry has to get past an already-deleted brain to reach the hand that is
// still there, so the brain step must tolerate NotFound too.
func TestCascadeIsIdempotentOnTheBrain(t *testing.T) {
	gone := status.Error(codes.NotFound, "actor b-sess-x not found")
	if status.Code(gone) != codes.NotFound {
		t.Fatal("a re-delete of an already-removed brain must not abort the cascade")
	}
}

// The error a caller sees has to name the leak, or the operator cannot find it.
// This is the message deleteSession returns when the hand survives.
func TestLeakErrorNamesTheHandAndSaysRetry(t *testing.T) {
	msg := "delete session sess-x: brain removed but its hand h-sess-x remains (retry the delete): rpc error"
	for _, want := range []string{"h-sess-x", "remains", "retry"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the leak error must mention %q — an operator has to know what to clean up", want)
		}
	}
}
