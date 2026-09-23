package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRetentionPreservesTaskConclusionAndState(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeFixture(t, 1, 30)
	task := createRuntimeTask(t, f, "retention")
	var source DataSource
	runtimeMutate(t, f.s, "retention.source", func(tx *Tx) (any, error) {
		if _, err := tx.SetChannelCapabilities(ctx, f.channel.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); err != nil {
			return nil, err
		}
		var err error
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "work", Channel: f.channel.ID, Workspace: "global", RetentionDays: 7})
		if err != nil {
			return nil, err
		}
		source, err = tx.SyncDataSourceGroups(ctx, source.ID, []string{f.watch.ConversationID})
		return source, err
	})
	raw := "please do retention"
	conclusion := "资源已切换并完成验证。"
	draftID := NewID()
	if _, err := f.s.DB.ExecContext(ctx, "INSERT INTO outbox(id,channel_id,route_id,route_version,job_id,conversation_id,audience_key,content,input_digest,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", draftID, f.channel.ID, f.direct.ID, f.direct.Version, task.ID, f.direct.ConversationID, f.direct.AudienceKey, conclusion+raw, Hash([]byte(raw)), Now(), Now()); err != nil {
		t.Fatal(err)
	}
	cacheRequest := Request{Scope: "global", Command: "retention.cache", Key: "snapshot"}
	if _, err := f.s.Mutate(ctx, cacheRequest, func(tx *Tx) (any, error) {
		return map[string]any{"id": "a-metadata-stays", "body": raw, "result": conclusion + raw}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.ExecContext(ctx, "INSERT INTO runtime_pending_actions(id,task_id,task_version,kind,target,payload,payload_digest,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)", NewID(), task.ID, task.Version, "message", "target", raw, Hash([]byte(raw)), "pending", Now(), Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.ExecContext(ctx, "UPDATE runtime_tasks SET status='completed',result_summary=? WHERE id=?", conclusion+"原请求："+raw, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.ExecContext(ctx, "UPDATE messages SET sent_at=? WHERE id IN (SELECT message_id FROM runtime_task_messages WHERE task_id=?)", time.Now().AddDate(0, 0, -8).Format(time.RFC3339Nano), task.ID); err != nil {
		t.Fatal(err)
	}
	before, err := ReadRuntimeTask(ctx, f.s.DB, task.ID)
	if err != nil || !strings.Contains(before.ResultSummary, conclusion) || strings.Contains(before.ResultSummary, raw) {
		t.Fatalf("pending task expiry: %+v %v", before, err)
	}
	if len(before.Actions) != 1 || before.Actions[0].Payload != "" {
		t.Fatal("pending action exposed expired raw text")
	}
	draft, err := ReadDraft(ctx, f.s.DB, draftID)
	if err != nil || strings.Contains(draft.Content, raw) || !strings.Contains(draft.Content, conclusion) {
		t.Fatalf("expired draft: %+v %v", draft, err)
	}
	cached, err := f.s.Mutate(ctx, cacheRequest, func(tx *Tx) (any, error) { t.Fatal("expired cache must not re-execute"); return nil, nil })
	if err != nil || !cached.Cached || strings.Contains(JSON(cached.Data), raw) || !strings.Contains(JSON(cached.Data), "a-metadata-stays") {
		t.Fatalf("expired cached response: %+v %v", cached, err)
	}
	epoch, err := DataSourceRetentionEpoch(ctx, f.s.DB, source)
	if err != nil || epoch == "" {
		t.Fatalf("read expiry did not advance context epoch: %s %v", epoch, err)
	}
	runtimeMutate(t, f.s, "retention.apply", func(tx *Tx) (any, error) { return tx.ApplyDataSourceRetention(ctx, source.ID, time.Now(), 200) })
	after, err := ReadRuntimeTask(ctx, f.s.DB, task.ID)
	if err != nil || after.Status != "completed" || !strings.Contains(after.ResultSummary, conclusion) || strings.Contains(after.ResultSummary, raw) {
		t.Fatalf("retained task conclusion: %+v %v", after, err)
	}
	if after.Actions[0].Payload != "" || after.Actions[0].Status != "stale" {
		t.Fatal("expired action retained executable payload")
	}
}

func TestLargerBackfillWindowClosesContainedGap(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, r := fixtureChannel(t, s, ChannelDwsPersonal, "cid:gap")
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(time.Hour)
	runtimeMutate(t, s, "gap.partial", func(tx *Tx) (any, error) {
		return tx.RecordCoverage(ctx, CoverageWindow{ChannelID: c.ID, ConversationID: r.ConversationID, StartAt: start.Format(time.RFC3339), EndAt: start.Add(30 * time.Minute).Format(time.RFC3339), StopReason: "disconnected"})
	})
	var watermark Watermark
	runtimeMutate(t, s, "gap.complete", func(tx *Tx) (any, error) {
		var err error
		watermark, err = tx.RecordCoverage(ctx, CoverageWindow{ChannelID: c.ID, ConversationID: r.ConversationID, StartAt: start.Format(time.RFC3339), EndAt: end.Format(time.RFC3339), Complete: true})
		return watermark, err
	})
	if watermark.GapUnresolved {
		t.Fatalf("contained gap remained unresolved: %+v", watermark)
	}
}

func TestDirectCollectionStartsAtEnableBoundaryAndRetentionRedactsRawText(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group")
	direct, history := true, false
	var source DataSource
	runtimeMutate(t, s, "direct.source", func(tx *Tx) (any, error) {
		var err error
		if _, err = tx.SetChannelCapabilities(ctx, c.ID, Capabilities{Verified: map[string]bool{"receive": true, "history": true}}, "fake"); err != nil {
			return nil, err
		}
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "work", Channel: c.ID, Workspace: "global", DirectEnabled: &direct, HistoryEnabled: &history, RetentionDays: 7})
		if err != nil {
			return nil, err
		}
		return tx.SetDataSourceStatus(ctx, source.ID, "running")
	})
	source, _ = ReadDataSource(ctx, s.DB, source.ID)
	enabled, err := time.Parse(time.RFC3339, source.DirectEnabledAt)
	if err != nil || !source.DirectEnabled || source.RetentionDays != 7 {
		t.Fatalf("direct source: %+v %v", source, err)
	}
	event := NormalizedEvent{Kind: EventMessage, Adapter: "dws", ParseVersion: "dws/1", Origin: "stream", ProviderEventID: "evt-direct", ProviderMessageID: "msg-direct", ConversationID: "cid:peer", ConversationType: "direct", Tenant: c.Tenant, Sender: Sender{IDType: "open_id", IDValue: "peer-1", DisplayName: "同事甲"}, Body: "请跟进这个需求", SentAt: enabled.Add(time.Second).Format(time.RFC3339Nano), EventAt: enabled.Add(2 * time.Second).Format(time.RFC3339Nano)}
	runtimeMutate(t, s, "direct.before-enable", func(tx *Tx) (any, error) {
		old := event
		old.ProviderEventID, old.ProviderMessageID, old.SentAt = "evt-old", "msg-old", enabled.Add(-time.Second).Format(time.RFC3339Nano)
		_, e := tx.EnsureDataSourceDirectConversation(ctx, source.ID, old)
		if ErrorCode(e) != "denied" {
			t.Fatalf("pre-enable direct message admitted: %v", e)
		}
		return nil, nil
	})
	var intakeResult IntakeResult
	runtimeMutate(t, s, "direct.intake", func(tx *Tx) (any, error) {
		if _, err := tx.EnsureDataSourceDirectConversation(ctx, source.ID, event); err != nil {
			return nil, err
		}
		var err error
		intakeResult, err = tx.Intake(ctx, c.ID, event)
		return intakeResult, err
	})
	query, err := MessageQuery(ctx, s.DB, c.ID, MessageQueryInput{ConversationType: "direct", ContactIDType: "open_id", ContactIDValue: "peer-1", Query: "同事甲", Limit: 10})
	if err != nil || len(query.Messages) != 1 || query.Messages[0].ID != intakeResult.MessageID || query.DirectEnabledAt == "" || query.RetentionDays != 7 {
		t.Fatalf("direct query: %+v %v", query, err)
	}
	source, _ = ReadDataSource(ctx, s.DB, source.ID)
	preview, err := PreviewDataSourceRetention(ctx, s.DB, source.ID, 7, time.Now().AddDate(0, 0, 8))
	if err != nil || preview.Eligible != 1 {
		t.Fatalf("retention preview: %+v %v", preview, err)
	}
	ownerRoutes, err := ReadOwnerSourceProcessingRoutes(ctx, s.DB, source, time.Now())
	if err != nil || len(ownerRoutes) != 1 {
		t.Fatalf("owner direct scope: %v %v", ownerRoutes, err)
	}
	groupRoutes, err := ReadSourcePositiveProcessingRoutes(ctx, s.DB, source, time.Now())
	if err != nil || len(groupRoutes) != 0 {
		t.Fatalf("group scope exposed private route: %v %v", groupRoutes, err)
	}
	oldSent := time.Now().UTC().AddDate(0, 0, -8).Format(time.RFC3339Nano)
	if _, err = s.DB.ExecContext(ctx, "UPDATE messages SET sent_at=? WHERE id=?", oldSent, intakeResult.MessageID); err != nil {
		t.Fatal(err)
	}
	readBeforeCleanup, err := ReadSource(ctx, s.DB, intakeResult.SourceID, "global")
	if err != nil || readBeforeCleanup.Availability != "expired" || len(readBeforeCleanup.Locations) == 0 || len(readBeforeCleanup.Fragments) == 0 || readBeforeCleanup.Fragments[0].Content != "" {
		t.Fatalf("source read leaked pending expiration: %+v %v", readBeforeCleanup, err)
	}
	messageBeforeCleanup, err := ReadMessage(ctx, s.DB, intakeResult.MessageID)
	if err != nil || messageBeforeCleanup.Body != "" || messageBeforeCleanup.Revisions[0].Body != "" {
		t.Fatalf("message read leaked pending expiration: %+v %v", messageBeforeCleanup, err)
	}
	searchBeforeCleanup, err := Search(ctx, s.DB, event.Body, SearchOptions{Kind: "source", Scope: "global"})
	if err != nil || len(searchBeforeCleanup) != 0 {
		t.Fatalf("search leaked expired source: %+v %v", searchBeforeCleanup, err)
	}
	var retention RetentionResult
	runtimeMutate(t, s, "direct.retention", func(tx *Tx) (any, error) {
		var err error
		retention, err = tx.ApplyDataSourceRetention(ctx, source.ID, time.Now(), 20)
		return retention, err
	})
	if retention.Expired != 1 {
		t.Fatalf("retention: %+v", retention)
	}
	message, err := ReadMessage(ctx, s.DB, intakeResult.MessageID)
	if err != nil || message.Availability != "expired" || message.Body != "" || len(message.Revisions) != 1 || message.Revisions[0].Body != "" {
		t.Fatalf("expired message: %+v %v", message, err)
	}
	var content string
	var redacted int
	var state, uri string
	if err = s.DB.QueryRowContext(ctx, "SELECT content,redacted FROM sources WHERE id=?", intakeResult.SourceID).Scan(&content, &redacted); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRowContext(ctx, "SELECT state FROM source_availability WHERE source_id=?", intakeResult.SourceID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRowContext(ctx, "SELECT uri FROM source_locations WHERE source_id=?", intakeResult.SourceID).Scan(&uri); err != nil {
		t.Fatal(err)
	}
	if content != "" || redacted != 1 || state != "expired" || uri == "" {
		t.Fatalf("expired evidence content=%q redacted=%d state=%q uri=%q", content, redacted, state, uri)
	}
	query, err = MessageQuery(ctx, s.DB, c.ID, MessageQueryInput{ConversationType: "direct", Limit: 10})
	if err != nil || len(query.Messages) != 0 {
		t.Fatalf("expired content remained queryable: %+v %v", query.Messages, err)
	}
	issues, err := checkDerivedAndEvidence(ctx, s.DB)
	if err != nil || len(issues) != 0 {
		t.Fatalf("expiration broke memory integrity: %v %v", issues, err)
	}
}

func TestGroupRefreshPreservesOnlyDirectRoutesOwnedBySource(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDwsPersonal, "cid:group")
	direct, history := true, false
	var source DataSource
	runtimeMutate(t, s, "direct.scope", func(tx *Tx) (any, error) {
		var err error
		source, err = tx.ConfigureDataSource(ctx, DataSourceInput{Name: "work", Channel: c.ID, Workspace: "global", DirectEnabled: &direct, HistoryEnabled: &history})
		if err != nil {
			return nil, err
		}
		source, err = tx.SetDataSourceStatus(ctx, source.ID, "running")
		if err != nil {
			return nil, err
		}
		if _, err = tx.AdmitDataSourceDirectConversation(ctx, source.ID, "cid:peer", "同事"); err != nil {
			return nil, err
		}
		if _, err = tx.AddRoute(ctx, c.ID, RouteInput{ConversationID: "owner-delivery", ConversationType: "direct", Mode: "notify", SendPolicy: "dispatch_only"}); err != nil {
			return nil, err
		}
		return tx.SyncDataSourceGroups(ctx, source.ID, []string{"cid:group2"})
	})
	source, _ = ReadDataSource(ctx, s.DB, source.ID)
	if len(source.RouteIDs) != 2 {
		t.Fatalf("source inherited delivery direct or lost owned direct: %+v", source.RouteIDs)
	}
	for _, id := range source.RouteIDs {
		r, _ := ReadRoute(ctx, s.DB, id)
		if r.ConversationID == "owner-delivery" {
			t.Fatal("owner delivery route entered acquisition scope")
		}
	}
}
