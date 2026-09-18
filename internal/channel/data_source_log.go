package channel

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

// SourceLogger uses the same disposable JSONL schema and retention machinery as
// runtimes; runtime_id contains the internal source UUID for this stream.
type SourceLogger interface {
	Emit(runlog.Event) error
	Maintain() error
}

func sourceErrorCode(err error) string {
	if err == nil {
		return ""
	}
	switch code := core.ErrorCode(err); code {
	case "invalid_input", "conflict", "not_found", "unavailable", "denied", "internal", "history_cursor_stalled":
		return code
	default:
		return "internal"
	}
}
func (s *DataSourceService) logEvent(ctx context.Context, id, event, status, summary string, began time.Time, err error) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.Logger == nil || s.logFailed {
		return
	}
	level := "info"
	if err != nil {
		level = "warn"
	}
	e := runlog.Event{RuntimeID: id, Component: "data_source", Event: event, Status: status, Summary: summary, ErrorCode: sourceErrorCode(err), Level: level}
	if !began.IsZero() {
		e.DurationMS = max(1, time.Since(began).Milliseconds())
	}
	if err := s.Logger.Emit(e); err != nil {
		s.failLogging(ctx, id)
	}
}
func (s *DataSourceService) failLogging(ctx context.Context, id string) {
	s.logFailed = true
	if s.Diagnostic != nil {
		_, _ = io.WriteString(s.Diagnostic, "data source logging degraded\n")
	}
	// Store a health flag, never raw logger errors or log bodies. State commits
	// and collection continue even when disposable logging cannot be written.
	_ = s.mutate(ctx, "data-source.logging.degraded", func(tx *core.Tx) (any, error) { return nil, tx.SetDataSourceLogHealth(ctx, id, true) })
}
func (s *DataSourceService) maintainLogs(ctx context.Context, id string) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.Logger != nil && !s.logFailed {
		if err := s.Logger.Maintain(); err != nil {
			s.failLogging(ctx, id)
		}
	}
}
func sourceCountSummary(count int) string {
	return fmt.Sprintf("采集范围包含 %d 个会话", count)
}
