package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func (a *app) restartRuntime(ctx context.Context, value string) error {
	pids, err := runtimeRunnerPIDs(ctx, value)
	if err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	s, err := core.Open(stopCtx, a.dbPath(), false)
	if err != nil {
		return err
	}
	_, err = s.Mutate(stopCtx, core.Request{Scope: "global", Command: "runtime.restart.stop", Actor: a.actor, Input: map[string]any{"runtime": value}}, func(tx *core.Tx) (any, error) {
		return tx.SetRuntimeStatus(stopCtx, value, "stopped", "")
	})
	closeErr := s.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = waitRuntimeRunnerPIDs(stopCtx, value, pids); err != nil {
		return err
	}
	return a.runRuntime(ctx, value)
}

func runtimeRunnerPIDs(ctx context.Context, runtimeName string) ([]int, error) {
	raw, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, core.Fail("unavailable", "runtime process list could not be read")
	}
	return parseRuntimeRunnerPIDs(string(raw), runtimeName, os.Getpid()), nil
}

func parseRuntimeRunnerPIDs(raw, runtimeName string, self int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || filepath.Base(fields[1]) != "memgov" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == self || seen[pid] {
			continue
		}
		for i := 2; i+2 < len(fields); i++ {
			if fields[i] == "runtime" && (fields[i+1] == "start" || fields[i+1] == "restart") && fields[i+2] == runtimeName {
				seen[pid] = true
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

func waitRuntimeRunnerPIDs(ctx context.Context, runtimeName string, initial []int) error {
	if len(initial) == 0 {
		return nil
	}
	pending := map[int]bool{}
	for _, pid := range initial {
		pending[pid] = true
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := runtimeRunnerPIDs(ctx, runtimeName)
		if err != nil {
			return err
		}
		alive := false
		for _, pid := range current {
			if pending[pid] {
				alive = true
				break
			}
		}
		if !alive {
			return nil
		}
		select {
		case <-ctx.Done():
			return core.Fail("unavailable", "runtime %s did not stop before the restart timeout", runtimeName)
		case <-ticker.C:
		}
	}
}
