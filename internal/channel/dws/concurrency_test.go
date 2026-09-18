package dws

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestConcurrencySharedAndDeadlineBounded(t *testing.T) {
	var active, peak atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	run := func(ctx context.Context, _ ...string) ([]byte, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Error("missing request deadline")
		}
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return envelope(`{}`), nil
		}
	}
	a, b := &Adapter{Run: run, Timeout: time.Hour}, &Adapter{Run: run, Timeout: time.Hour}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			adapter := a
			if i%2 == 1 {
				adapter = b
			}
			if _, e := adapter.run(context.Background(), "read"); e != nil {
				t.Error(e)
			}
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("pool failed to start two requests")
		}
	}
	select {
	case <-entered:
		t.Error("more than two DWS requests entered")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if peak.Load() != 2 {
		t.Fatalf("peak=%d", peak.Load())
	}
}
