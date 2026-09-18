//go:build !windows

// Package processtree provides bounded cancellation for model/tool invocations.
package processtree

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func Fingerprint(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	return strings.TrimSpace(string(b)), err
}

// Alive checks an incarnation, not just a PID that might have been recycled.
func Alive(pid int, stamp string) bool {
	if pid <= 0 || stamp == "" {
		return false
	}
	current, err := Fingerprint(pid)
	return err == nil && current == stamp
}

func Reap(pid int, stamp string) error {
	if pid <= 1 {
		return nil
	}
	current, err := Fingerprint(pid)
	if err == nil && current != stamp {
		return fmt.Errorf("process identity changed; recovery requires inspection")
	}
	if err = syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return waitGroupGone(pid)
}

func waitGroupGone(pid int) error {
	until := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if time.Now().After(until) {
			return fmt.Errorf("process group %d exit not confirmed; lease remains quarantined", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Bind must be called before Start. Finish must be called after Wait to kill
// descendants holding pipes or continuing work after their parent exited.
func Bind(cmd *exec.Cmd) func() {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var mu sync.Mutex
	var timer *time.Timer
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		if cmd.Process == nil {
			return nil
		}
		pid := cmd.Process.Pid
		err := syscall.Kill(-pid, syscall.SIGTERM)
		timer = time.AfterFunc(5*time.Second, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err := waitGroupGone(cmd.Process.Pid)
			if v, ok := invocations.LoadAndDelete(cmd); ok && err != nil {
				r := v.(*recording)
				r.mu.Lock()
				r.err = err
				r.mu.Unlock()
			}
		}
	}
}
