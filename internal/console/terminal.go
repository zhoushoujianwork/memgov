package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/tasklog"
)

func terminalTaskID(path string) string {
	if !strings.HasPrefix(path, "/api/v1/tasks/") || !strings.HasSuffix(path, "/terminal") {
		return ""
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/tasks/"), "/terminal")
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

func (s *Server) terminalAttempt(ctx context.Context, id, attemptID string) (core.RuntimeTask, core.RuntimeAttempt, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.RuntimeTask{}, core.RuntimeAttempt{}, err
	}
	defer tx.Rollback()
	task, err := core.ReadRuntimeTask(ctx, tx, id)
	if err != nil {
		return task, core.RuntimeAttempt{}, err
	}
	card, err := s.card(ctx, tx, id)
	if err != nil {
		return task, core.RuntimeAttempt{}, err
	}
	current, err := core.RuntimeTaskOutputCurrent(ctx, tx, id)
	if err != nil {
		return task, core.RuntimeAttempt{}, err
	}
	if card.Redacted || !current {
		return task, core.RuntimeAttempt{}, core.Fail("denied", "原请求已修改、到期、撤回或私聊已清空，执行过程不可用")
	}
	for i := len(task.Attempts) - 1; i >= 0; i-- {
		a := task.Attempts[i]
		if attemptID == "" || a.ID == attemptID {
			if a.TaskVersion != task.Version || a.Status == "stale" {
				return task, a, core.Fail("denied", "旧任务版本的执行输出已失效")
			}
			return task, a, nil
		}
	}
	return task, core.RuntimeAttempt{}, core.Fail("not_found", "execution attempt not found")
}

// Each short read rechecks source visibility; no long-lived SQLite snapshot.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request, id string) {
	task, attempt, err := s.terminalAttempt(r.Context(), id, r.URL.Query().Get("attempt"))
	if err != nil {
		s.reply(w, nil, err)
		return
	}
	rawCursor := r.Header.Get("Last-Event-ID")
	if rawCursor == "" {
		rawCursor = r.URL.Query().Get("cursor")
	}
	cursor := int64(0)
	if rawCursor != "" {
		cursor, err = strconv.ParseInt(rawCursor, 10, 64)
		if err != nil || cursor < 0 {
			s.reply(w, nil, core.Fail("invalid_input", "invalid output cursor"))
			return
		}
	}
	if _, ok := w.(http.Flusher); !ok {
		s.reply(w, nil, core.Fail("unavailable", "streaming unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(kind string, data any, offset int64) error {
		body, _ := json.Marshal(data)
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if offset > 0 {
			if _, err := fmt.Fprintf(w, "id: %d\n", offset); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, body); err != nil {
			return err
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		_ = controller.SetWriteDeadline(time.Time{})
		return nil
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		fresh, a, err := s.terminalAttempt(r.Context(), id, attempt.ID)
		if err != nil || fresh.Version != task.Version {
			_ = send("invalidated", map[string]string{"reason": "任务或原文已变化，请刷新后核对"}, 0)
			return
		}
		tail, err := tasklog.Read(s.opts.Home, task.RuntimeID, id, attempt.ID, cursor)
		if err != nil {
			_ = send("invalidated", map[string]string{"reason": "过程输出不可用"}, 0)
			return
		}
		if tail.Truncated {
			if send("notice", map[string]string{"text": "只展示最近 256 KiB 过程输出"}, 0) != nil {
				return
			}
		}
		for _, event := range tail.Events {
			if send("output", event, event.Offset) != nil {
				return
			}
		}
		cursor = tail.Cursor
		info := map[string]any{"attempt": a.ID, "status": a.Status, "available": tail.Available, "task_status": fresh.Status}
		if send("status", info, 0) != nil {
			return
		}
		// Attempt status comes from SQLite. A Claude result alone is not completion.
		if a.Status != "running" {
			_ = send("finished", info, 0)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
