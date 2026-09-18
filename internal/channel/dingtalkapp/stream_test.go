package dingtalkapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type fakeSessionClient struct {
	starts, closes int
	startErr       error
}

func (f *fakeSessionClient) Start(context.Context) error { f.starts++; return f.startErr }
func (f *fakeSessionClient) Close()                      { f.closes++ }

func TestSupervisedStreamClosesOnCancellationAndRotation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{"shutdown", true},
		{"bounded rotation", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &fakeSessionClient{}
			ready := 0
			err := superviseStreamSession(ctx, client, 20*time.Millisecond, func() {
				ready++
				if tc.cancel {
					cancel()
				}
			})
			if err != nil || client.starts != 1 || client.closes != 1 || ready != 1 {
				t.Fatalf("a supervised session remained subscribed: starts=%d closes=%d ready=%d error=%v",
					client.starts, client.closes, ready, err)
			}
		})
	}
}

func TestSupervisedStreamDoesNotClaimReadinessOrCloseAnUndialedClient(t *testing.T) {
	client := &fakeSessionClient{startErr: errors.New("dial failed")}
	ready := false
	err := superviseStreamSession(context.Background(), client, time.Second, func() { ready = true })
	if core.ErrorCode(err) != "unavailable" || ready || client.closes != 0 {
		t.Fatalf("failed dial was marked ready: ready=%t closes=%d error=%v", ready, client.closes, err)
	}
}
