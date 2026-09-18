//go:build !windows

package processtree

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDeadlineReapsProcessGroupAndPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	// Both the leader and its child ignore graceful cancellation.
	b, err := Output(ctx, exec.CommandContext(ctx, "sh", "-c", `trap '' TERM; (trap '' TERM; sleep 60) & echo partial; wait`))
	if err == nil || !strings.Contains(string(b), "partial") {
		t.Fatalf("lost error or partial output: %q %v", b, err)
	}
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("reaping took %s", elapsed)
	}
}
