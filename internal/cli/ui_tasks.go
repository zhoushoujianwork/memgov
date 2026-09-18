package cli

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func taskResumeControl(home string) func(context.Context, string, int) (any, error) {
	return func(ctx context.Context, id string, version int) (any, error) {
		s, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		result, err := s.Mutate(ctx, core.Request{ID: core.NewID(), Scope: "global", Actor: "console-owner", Command: "runtime.task.resume", Input: map[string]any{"task_id": id, "expected_version": version}}, func(tx *core.Tx) (any, error) {
			return tx.ResumeRuntimeTask(ctx, id, version)
		})
		if err != nil {
			return nil, err
		}
		var task core.RuntimeTask
		if err := json.Unmarshal(result.Data, &task); err != nil {
			return nil, err
		}
		config, err := core.ReadRuntime(ctx, s.DB, task.RuntimeID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"task_id": task.ID, "status": task.Status, "mode": task.Resume.Mode, "runtime_status": config.Status}, nil
	}
}
