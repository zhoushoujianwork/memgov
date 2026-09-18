package channel

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

type unavailableSourceLog struct{}

func (unavailableSourceLog) Emit(runlog.Event) error {
	return errors.New("access_token=secret chat raw stderr")
}
func (unavailableSourceLog) Maintain() error { return nil }

func logSourceFixture(t *testing.T) (Collector, core.DataSource) {
	t.Helper()
	collector, c := testCollector(t, &fakeAdapter{})
	ctx := context.Background()
	var d core.DataSource
	_, err := collector.Store.Mutate(ctx, core.Request{Scope: "global", Command: "test.source"}, func(tx *core.Tx) (any, error) {
		var e error
		d, e = tx.ConfigureDataSource(ctx, core.DataSourceInput{Name: "source", Channel: c.ID, Workspace: "global"})
		if e != nil {
			return nil, e
		}
		return tx.SetDataSourceStatus(ctx, d.ID, "running")
	})
	if err != nil {
		t.Fatal(err)
	}
	return collector, d
}
func TestSourceLogFailurePreservesCollectionAndHealthAfterCheckpoint(t *testing.T) {
	collector, d := logSourceFixture(t)
	ctx := context.Background()
	var diagnostic bytes.Buffer
	service := DataSourceService{Store: collector.Store, Logger: unavailableSourceLog{}, Diagnostic: &diagnostic}
	service.logEvent(ctx, d.ID, "reconciled", "complete", "有界历史补漏检查结束", time.Now(), nil)
	_, err := collector.Store.Mutate(ctx, core.Request{Scope: "global", Command: "test.checkpoint"}, func(tx *core.Tx) (any, error) { return nil, tx.CheckpointDataSource(ctx, d.ID, 1, "") })
	if err != nil {
		t.Fatal(err)
	}
	state, err := core.ReadDataSource(ctx, collector.Store.DB, d.ID)
	if err != nil || state.Status != "running" || state.ReconcileCursor != 1 {
		t.Fatalf("business state interrupted: %+v %v", state, err)
	}
	health, err := core.DataSourceLogHealth(ctx, collector.Store.DB, d.ID)
	if err != nil || health != "degraded" {
		t.Fatalf("health %s %v", health, err)
	}
	if diagnostic.String() != "data source logging degraded\n" {
		t.Fatalf("unsafe diagnostic: %q", diagnostic.String())
	}
}
func TestSourceLogsExposeOnlyClassifiedErrorsWithDurations(t *testing.T) {
	collector, d := logSourceFixture(t)
	ctx := context.Background()
	home := t.TempDir()
	var stdout bytes.Buffer
	logger, err := runlog.Open(home, d.ID, &stdout, runlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	service := DataSourceService{Store: collector.Store, Logger: logger}
	malicious := core.Fail("access_token=secret", "conversation-id / body / full-model-output / raw-stderr")
	service.logEvent(ctx, d.ID, "history_step", "failed", "历史导入步骤失败", time.Now().Add(-time.Second), malicious)
	events, err := runlog.Show(home, d.ID, runlog.Filter{Level: "warn", Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("logs: %+v %v", events, err)
	}
	if events[0].ErrorCode != "internal" || events[0].DurationMS < 1000 || events[0].SchemaVersion != 1 {
		t.Fatalf("event: %+v", events[0])
	}
	for _, secret := range []string{"access_token", "secret", "conversation-id", "full-model-output", "raw-stderr"} {
		if strings.Contains(stdout.String(), secret) {
			t.Fatalf("log leaked %q", secret)
		}
	}
	content, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != stdout.String() {
		t.Fatal("stdout and file differ")
	}
	info, err := os.Stat(logger.Path())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
}
