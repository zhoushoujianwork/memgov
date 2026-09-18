package runtime

import (
	"context"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
	"time"
)

func memoryFailure(err error) string {
	switch core.ErrorCode(err) {
	case "denied":
		return "memory_permission_denied"
	case "invalid_input":
		return "memory_invalid_candidate"
	case "evidence_unavailable", "not_found":
		return "memory_evidence_unavailable"
	case "conflict":
		return "memory_version_conflict"
	case "review_timeout":
		return "memory_review_timeout"
	case "unavailable":
		return "memory_review_unavailable"
	default:
		return "memory_" + core.ErrorCode(err)
	}
}

func (s *Service) executeReview(ctx context.Context, cfg core.RuntimeConfig) {
	if !s.reserveSlot(cfg, "execution") {
		return
	}
	reserved := cfg.ApplicationMode == "proactive" && s.concurrent
	ready, err := core.RuntimeReviewReady(ctx, s.Store.DB, cfg.ID)
	if err != nil || !ready {
		if reserved {
			s.releaseSlot(cfg, "execution")
		}
		return
	}
	var job core.RuntimeReview
	err = s.mutate(core.WithInMemoryCapacity(ctx), "global", "runtime.review.claim", func(tx *core.Tx) (any, error) { var e error; job, e = tx.ClaimRuntimeReview(ctx, cfg); return job, e })
	if err != nil || job.ID == "" {
		if reserved {
			s.releaseSlot(cfg, "execution")
		}
		return
	}
	if s.concurrent {
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			defer s.wakeWorker()
			defer s.releaseSlot(cfg, "execution")
			s.runReview(ctx, cfg, job)
		}()
	} else {
		s.runReview(ctx, cfg, job)
		if reserved {
			s.releaseSlot(cfg, "execution")
		}
	}
}

func (s *Service) runReview(parent context.Context, cfg core.RuntimeConfig, job core.RuntimeReview) {
	ctx, finish := s.workContext(parent, job.ExecutionID, "", 0)
	defer finish()
	start := time.Now()
	var candidate core.Candidate
	var review ReviewResult
	task, err := core.ReadRuntimeTask(ctx, s.Store.DB, job.TaskID)
	if err == nil {
		err = core.CheckRuntimeReview(ctx, s.Store.DB, job)
	}
	var route core.Route
	if err == nil {
		route, err = core.ReadRoute(ctx, s.Store.DB, task.RouteID)
	}
	if err == nil && job.Candidate == nil {
		err = core.Fail("invalid_input", "memory proposal omitted candidate")
	}
	if err == nil {
		err = s.mutate(ctx, route.WorkspaceID, "runtime.memory.candidate", func(tx *core.Tx) (any, error) {
			if e := core.CheckRuntimeReview(ctx, tx.Conn, job); e != nil {
				return nil, e
			}
			var e error
			candidate, e = tx.SubmitCandidate(ctx, *job.Candidate, "", "runtime-executor")
			if e != nil {
				return nil, e
			}
			_, e = tx.Conn.ExecContext(ctx, "UPDATE runtime_reviews SET candidate_id=? WHERE id=? AND execution_id=?", candidate.ID, job.ID, job.ExecutionID)
			return candidate, e
		})
	}
	if err == nil {
		review, err = s.Reviewer.Review(ctx, task, *job.Candidate)
	}
	if ctx.Err() != nil {
		err = workError(ctx, "review")
	}
	if err == nil {
		err = s.mutate(ctx, route.WorkspaceID, "runtime.memory.review", func(tx *core.Tx) (any, error) {
			if e := core.CheckRuntimeReview(ctx, tx.Conn, job); e != nil {
				return nil, e
			}
			c, e := tx.SubmitCandidateReview(ctx, candidate.ID, "runtime-reviewer:"+job.ID, review.Decision, review.Issues)
			if e != nil {
				return nil, e
			}
			status := "rejected"
			if review.Decision == "accept" {
				if _, e = tx.ApplyCandidate(ctx, c.ID, c.Digest); e != nil {
					return nil, e
				}
				status = "applied"
			}
			return nil, tx.FinishRuntimeReview(ctx, job, candidate.ID, status, "", review.Usage)
		})
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		code := memoryFailure(err)
		_ = s.mutate(cleanup, "global", "runtime.memory.failed", func(tx *core.Tx) (any, error) {
			return nil, tx.FinishRuntimeReview(cleanup, job, candidate.ID, "failed", code, review.Usage)
		})
		s.emit(cleanup, runlog.Event{RuntimeID: cfg.ID, TaskID: job.TaskID, AttemptID: job.ExecutionID, Component: "memory", Event: "failed", Level: "error", ErrorCode: code, DurationMS: time.Since(start).Milliseconds(), Summary: "记忆沉淀失败；已完成的业务结果保留"})
		return
	}
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: job.TaskID, AttemptID: job.ExecutionID, Component: "memory", Event: "reviewed", Level: "info", Status: review.Decision, Model: review.Usage.Model, InputTokens: review.Usage.InputTokens, OutputTokens: review.Usage.OutputTokens, CostUSD: review.Usage.CostUSD, DurationMS: time.Since(start).Milliseconds(), Summary: "独立记忆审查已记录"})
}
