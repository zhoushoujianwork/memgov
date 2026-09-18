package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

func hotwordMemory(t *testing.T, s *Store, scope, canonical string, aliases ...string) Memory {
	t.Helper()
	m := fixtureMemory(t, s, scope)
	m.Title = "热词：" + canonical
	m.Summary = canonical + " 项目"
	m.Content = canonical + " 的语音转写名称映射：" + strings.Join(aliases, "、")
	m.Tags = []string{"hotword", "speech-to-text"}
	m.Hotword = &Hotword{Canonical: canonical, Aliases: aliases, Meaning: canonical + " 项目"}
	return createMemory(t, s, m)
}

func TestHotwordContextPrefersWorkspaceAndHonorsRetirementAndBudget(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	var workspace Workspace
	if _, err := s.Mutate(ctx, Request{Scope: "global", Command: "workspace.add"}, func(tx *Tx) (any, error) {
		var err error
		workspace, err = tx.AddWorkspace(ctx, "project", "")
		return workspace, err
	}); err != nil {
		t.Fatal(err)
	}
	global := hotwordMemory(t, s, "global", "global-tool", "格罗博")
	hotwordMemory(t, s, workspace.ID, "memgov", "MEMGO GO", "Memo Go")
	hotwordMemory(t, s, workspace.ID, "memo-service", "Memo Go")
	got, err := HotwordContext(ctx, s.DB, workspace.ID, nil, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "MEMGO GO") || !strings.Contains(got, "global-tool") || strings.Index(got, "memgov") > strings.Index(got, "global-tool") || strings.Count(got, "Memo Go") != 2 {
		t.Fatalf("workspace hotwords were not prioritized: %q", got)
	}
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "retire"}, func(tx *Tx) (any, error) {
		return tx.SetState(ctx, global.ID, "retired", "obsolete", global.Version, 0)
	}); err != nil {
		t.Fatal(err)
	}
	got, err = HotwordContext(ctx, s.DB, workspace.ID, nil, 30)
	if err != nil || strings.Contains(got, "global-tool") || len([]rune(got)) > 30 {
		t.Fatalf("retirement or budget failed: %q %v", got, err)
	}
}

func TestHotwordContextRequiresPublicationForExactGroup(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, route := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group1")
	m := hotwordMemory(t, s, "global", "memgov", "Memo Go")
	audience, err := AudienceFor(ctx, s.DB, c.Name, route.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := HotwordContext(ctx, s.DB, "global", &audience, 4000); err != nil || got != "" {
		t.Fatalf("unpublished hotword leaked: %q %v", got, err)
	}
	if _, err = s.Mutate(ctx, Request{Scope: "global", Command: "publish"}, func(tx *Tx) (any, error) {
		return tx.Publish(ctx, c.Name, route.ConversationID, m.ID, "该群使用这个项目名")
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := HotwordContext(ctx, s.DB, "global", &audience, 4000); err != nil || !strings.Contains(got, "Memo Go") {
		t.Fatalf("published hotword unavailable: %q %v", got, err)
	}
}

func TestCaptureRuntimeHotwordRequiresCurrentOwnerCorrectionAndAppliesMemory(t *testing.T) {
	ctx := context.Background()
	f, app, inbox, _ := crossConfirmationFixture(t)
	var cfg RuntimeConfig
	runtimeMutate(t, f.s, "hotword.direct.configure", func(tx *Tx) (any, error) {
		if _, err := tx.Conn.ExecContext(ctx, "UPDATE channel_routes SET conversation_id=? WHERE id=?", f.owner.IDValue, inbox.ID); err != nil {
			return nil, err
		}
		var err error
		cfg, err = tx.ConfigureRuntime(ctx, RuntimeConfigInput{Name: "hotword-owner", Channel: app.ID, RouteIDs: []string{inbox.ID}, DeliveryRouteID: inbox.ID, Owner: f.owner, ApplicationMode: "direct"})
		return cfg, err
	})
	runtimeMutate(t, f.s, "hotword.direct.start", func(tx *Tx) (any, error) {
		return tx.SetRuntimeStatus(ctx, cfg.ID, "running", "")
	})
	inbox, _ = ReadRoute(ctx, f.s.DB, inbox.ID)
	intakeRuntimeMessage(t, f.s, app, inbox.ConversationID, "hotword-correction", f.owner, "MEMGO GO 和 Memo Go 都是 memgov，这个项目的记忆治理工具。", time.Now().Add(2*time.Hour))
	runtimeMutate(t, f.s, "hotword.direct.sync", func(tx *Tx) (any, error) { return tx.SyncRuntimeMessages(ctx, cfg.ID) })
	var batch RuntimeBatch
	runtimeMutate(t, f.s, "hotword.direct.batch", func(tx *Tx) (any, error) {
		var err error
		batch, err = tx.ClaimRuntimeBatch(ctx, cfg.ID, time.Now().Add(3*time.Hour))
		return batch, err
	})
	var tasks []RuntimeTask
	runtimeMutate(t, f.s, "hotword.direct.task", func(tx *Tx) (any, error) {
		var err error
		tasks, err = tx.CompleteRuntimeBatch(ctx, batch, RuntimeAnalysis{Decisions: []RuntimeDecision{{Kind: "task", CanonicalKey: "hotword", Title: "记住热词", Instructions: batch.Messages[0].Body, MessageIDs: []string{batch.Messages[0].ID}}}})
		return tasks, err
	})
	var task RuntimeTask
	var attempt RuntimeAttempt
	runtimeMutate(t, f.s, "hotword.direct.claim", func(tx *Tx) (any, error) {
		var err error
		task, attempt, err = tx.ClaimRuntimeTask(ctx, cfg.ID, NewID(), "model", "preset", "commit", "")
		return task, err
	})
	var applied Memory
	runtimeMutate(t, f.s, "hotword.capture", func(tx *Tx) (any, error) {
		var err error
		applied, err = tx.CaptureRuntimeHotword(ctx, task.ID, HotwordCaptureInput{AttemptID: attempt.ID, Canonical: "memgov", Aliases: []string{"Memo Go"}, Meaning: "记忆治理工具"})
		return applied, err
	})
	if applied.Hotword == nil || applied.Hotword.Canonical != "memgov" || len(applied.Evidence) == 0 || applied.Version != 1 {
		t.Fatalf("explicit correction was not applied: %+v", applied)
	}
	runtimeMutate(t, f.s, "hotword.capture.update", func(tx *Tx) (any, error) {
		var err error
		applied, err = tx.CaptureRuntimeHotword(ctx, task.ID, HotwordCaptureInput{AttemptID: attempt.ID, Canonical: "memgov", Aliases: []string{"MEMGO GO"}, Meaning: "记忆治理工具"})
		return applied, err
	})
	if applied.Version != 2 || len(applied.Hotword.Aliases) != 2 {
		t.Fatalf("second explicit correction did not update and merge aliases: %+v", applied)
	}
	runtimeMutate(t, f.s, "hotword.guess.reject", func(tx *Tx) (any, error) {
		_, err := tx.CaptureRuntimeHotword(ctx, task.ID, HotwordCaptureInput{AttemptID: attempt.ID, Canonical: "other", Aliases: []string{"guessed"}, Meaning: "guess"})
		if ErrorCode(err) != "denied" {
			t.Fatalf("model guess without owner text was accepted: %v", err)
		}
		return nil, nil
	})
}
