package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupRestoreCarriesPurgeAndLeavesSourceIndependent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	path := s.Path
	backup := filepath.Join(t.TempDir(), "snapshot.db")
	b, err := s.CreateBackup(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Export(ctx, s)
	if err != nil || len(p.Tables["sources"]) != 1 || len(p.Tables["revisions"]) != 1 {
		t.Fatal(p, err)
	}
	var plan PurgePlan
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		out, e := tx.PreviewPurge(ctx, m.ID, 1)
		if e == nil {
			plan = out.(map[string]any)["plan"].(PurgePlan)
		}
		return out, e
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.ApplyPurge(ctx, plan.ID, plan.Digest) })
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	req := Request{Scope: "global", Command: "backup.restore", Key: "restore-once", Input: b.SHA256}
	if _, err = RestoreBackup(ctx, path, backup, b.SHA256, req); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ReadMemory(ctx, restored.DB, m.ID, "global", 0); ErrorCode(err) != "not_found" {
		t.Fatal("restore resurrected body", err)
	}
	var n int
	restored.DB.QueryRow("SELECT count(*) FROM tombstones").Scan(&n)
	if n == 0 {
		t.Fatal("lost tombstones")
	}
	if _, err = PurgeStatus(ctx, restored.DB, plan.ID); err != nil {
		t.Fatal("lost purge receipt", err)
	}
	if err = restored.DB.QueryRow("SELECT count(*) FROM tombstones t LEFT JOIN operations o ON o.id=t.operation_id WHERE o.id IS NULL").Scan(&n); err != nil || n != 0 {
		t.Fatal("lost purge audit", n, err)
	}
	hits, err := Search(ctx, restored.DB, "发布", SearchOptions{})
	if err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}
	restored.Close()
	// Replaying a restore request must not overwrite later work.
	restored, _ = Open(ctx, path, false)
	_, err = restored.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.AddWorkspace(ctx, "later-work", "") })
	if err != nil {
		t.Fatal(err)
	}
	restored.Close()
	if _, err = RestoreBackup(ctx, path, backup, b.SHA256, req); err != nil {
		t.Fatal(err)
	}
	restored, _ = Open(ctx, path, false)
	defer restored.Close()
	if _, err = restored.Workspace(ctx, "later-work"); err != nil {
		t.Fatal("idempotent restore overwrote later work", err)
	}
}
func TestInvalidRestorePreservesCurrentDatabase(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	m := createMemory(t, s, fixtureMemory(t, s, "global"))
	path := s.Path
	s.Close()
	bad := filepath.Join(t.TempDir(), "bad.db")
	os.WriteFile(bad, []byte("not a database"), 0600)
	if _, err := RestoreBackup(ctx, path, bad, "wrong", Request{}); err == nil {
		t.Fatal("bad restore succeeded")
	}
	s, err := Open(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = ReadMemory(ctx, s.DB, m.ID, "global", 0); err != nil {
		t.Fatal("failed restore changed current database", err)
	}
}
