package turns

import (
	"context"
	"testing"
	"time"
)

// A heartbeat write must neither hang forever on a stuck database nor die
// with the request that started the turn.
func TestHeartbeatContext_BoundedAndDetached(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, stop := heartbeatContext(parent)
	defer stop()
	if err := ctx.Err(); err != nil {
		t.Fatalf("heartbeat context must outlive the request context, got %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("heartbeat query has no deadline")
	}
	if left := time.Until(deadline); left <= 0 || left > heartbeatQueryTimeout {
		t.Fatalf("deadline in %v, want within %v", left, heartbeatQueryTimeout)
	}
}
