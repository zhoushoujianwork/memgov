package processtree

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
)

type recorderKey struct{}
type Recorder func(pid int, started string) error
type recording struct {
	mu     sync.Mutex
	record Recorder
	err    error
}

var invocations sync.Map // *exec.Cmd -> *recording; removed by Bind's finish

func WithRecorder(ctx context.Context, record Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, &recording{record: record})
}

// A failed process cleanup quarantines its lease and conflicting directories.
func CheckQuiescence(ctx context.Context) error {
	if r, ok := ctx.Value(recorderKey{}).(*recording); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.err
	}
	return nil
}

type activityKey struct{}

func WithActivity(ctx context.Context, touch func()) context.Context {
	return context.WithValue(ctx, activityKey{}, touch)
}
func Activity(ctx context.Context) {
	if touch, ok := ctx.Value(activityKey{}).(func()); ok {
		touch()
	}
}

// Start records ownership before accepting any output. A failed durable
// registration terminates the invocation; callers must still call Wait.
func Start(ctx context.Context, cmd *exec.Cmd) error {
	if err := CheckQuiescence(ctx); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if r, ok := ctx.Value(recorderKey{}).(*recording); ok {
		invocations.Store(cmd, r)
		stamp, err := Fingerprint(cmd.Process.Pid)
		if err == nil {
			err = r.record(cmd.Process.Pid, stamp)
		}
		if err != nil {
			_ = cmd.Cancel()
			_ = cmd.Wait()
			return err
		}
	}
	return nil
}

func Output(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	finish := Bind(cmd)
	defer finish()
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	if cmd.Stderr == nil {
		cmd.Stderr = &stderr
	}
	if err := Start(ctx, cmd); err != nil {
		return out.Bytes(), err
	}
	err := cmd.Wait()
	if e, ok := err.(*exec.ExitError); ok {
		e.Stderr = stderr.Bytes()
	}
	return out.Bytes(), err
}
