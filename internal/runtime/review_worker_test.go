package runtime

import (
	"context"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"testing"
	"time"
)

type reviewFailureModel struct {
	fakeMemoryModels
	mode string
}

func (m *reviewFailureModel) Review(ctx context.Context, _ core.RuntimeTask, _ core.CandidateInput) (ReviewResult, error) {
	switch m.mode {
	case "timeout":
		<-ctx.Done()
		return ReviewResult{}, ctx.Err()
	case "reject":
		return ReviewResult{Decision: "reject", Issues: []string{"not reusable"}}, nil
	default:
		return ReviewResult{}, core.Fail(m.mode, "injected review failure")
	}
}

func TestMemoryReviewFailurePreservesBusinessResult(t *testing.T) {
	for _, mode := range []string{"denied", "invalid_input", "evidence_unavailable", "conflict", "timeout", "reject"} {
		t.Run(mode, func(t *testing.T) {
			s, cfg, preset, _, _ := setupService(t)
			models := &reviewFailureModel{mode: mode}
			s.Analyzer, s.Executor, s.Reviewer = models, models, models
			ctx := context.Background()
			if _, e := s.Store.DB.Exec("UPDATE runtime_configs SET review_timeout_seconds=1 WHERE id=?", cfg.ID); e != nil {
				t.Fatal(e)
			}
			_, e := s.Store.Mutate(ctx, core.Request{Scope: "global", Command: "test.intake"}, func(tx *core.Tx) (any, error) {
				return tx.Intake(ctx, cfg.ChannelID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "fake", ParseVersion: "1", Origin: "stream", ProviderMessageID: "review-failure", ConversationID: "watch", Tenant: "corp", Sender: core.Sender{IDType: "user_id", IDValue: "owner"}, Body: "S3 proxy cannot issue public links", SentAt: core.Now()})
			})
			if e != nil {
				t.Fatal(e)
			}
			s.tick(ctx, cfg, preset)
			tasks, e := core.RuntimeTaskList(ctx, s.Store.DB, cfg.ID, "completed", 10)
			if e != nil || len(tasks) != 1 {
				t.Fatalf("business result lost: %+v %v", tasks, e)
			}
			want := "failed"
			if mode == "reject" {
				want = "rejected"
			}
			if tasks[0].MemoryStatus != want || tasks[0].Result == "" {
				t.Fatalf("outcomes conflated: %+v", tasks[0])
			}
			if mode == "timeout" && tasks[0].MemoryErrorCode != "memory_review_timeout" {
				t.Fatalf("timeout classification: %+v", tasks[0])
			}
			var active int
			if e = s.Store.DB.QueryRow("SELECT count(*) FROM runtime_work_leases WHERE released=0").Scan(&active); e != nil || active != 0 {
				t.Fatalf("leaked slot: %d %v", active, e)
			}
			// A second tick must not replay the failed review or apply rejected memory.
			s.tick(ctx, cfg, preset)
			var applied int
			if e = s.Store.DB.QueryRow("SELECT count(*) FROM candidates WHERE status='applied'").Scan(&applied); e != nil || applied != 0 {
				t.Fatalf("failed review applied: %d %v", applied, e)
			}
		})
	}
}

func TestReviewBudgetCannotExtendParentDeadline(t *testing.T) {
	s, cfg, _, _, _ := setupService(t)
	ctx := context.Background()
	id := core.NewID()
	if _, err := s.Store.DB.Exec(`INSERT INTO runtime_tasks(id,runtime_id,route_id,canonical_key,kind,title,instructions,status,created_at,updated_at) VALUES(?,?,?,'review-budget','task','done','done','completed',?,?)`, id, cfg.ID, cfg.RouteIDs[0], core.Now(), core.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO runtime_reviews(id,task_id,task_version,runtime_id,source_attempt_id,candidate_input,deadline_at,created_at) VALUES(?,?,1,?,'parent','null',?,?)`, core.NewID(), id, cfg.ID, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), core.Now()); err != nil {
		t.Fatal(err)
	}
	s.executeReview(ctx, cfg)
	var status, code string
	if err := s.Store.DB.QueryRow("SELECT memory_status,memory_error_code FROM runtime_tasks WHERE id=?", id).Scan(&status, &code); err != nil || status != "failed" || code != "review_budget_exhausted" {
		t.Fatalf("parent budget extended: %s %s %v", status, code, err)
	}
}
