package core

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAppliedConfigVersionIdempotenceAndRollback(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	current, err := ReadAppliedConfig(ctx, s.DB, 0)
	if err != nil || current.Version != 0 {
		t.Fatalf("initial: %+v %v", current, err)
	}
	write := func(expected int, raw string, rollback bool) (AppliedConfig, error) {
		var got AppliedConfig
		_, err := s.Mutate(ctx, Request{Command: "config.test", Scope: "global"}, func(tx *Tx) (any, error) {
			var e error
			got, e = tx.CommitAppliedConfig(ctx, expected, 1, json.RawMessage(raw), nil)
			if e == nil && rollback {
				return nil, Fail("internal", "injected rollback")
			}
			return got, e
		})
		return got, err
	}
	a, err := write(0, `{"agents":{},"data_sources":{}}`, false)
	if err != nil || a.Version != 1 {
		t.Fatalf("first: %+v %v", a, err)
	}
	b, err := write(1, `{"data_sources":{},"agents":{}}`, false)
	if err != nil || b.ID != a.ID {
		t.Fatalf("canonical no-op: %+v %v", b, err)
	}
	if _, err = write(0, `{}`, false); ErrorCode(err) != "conflict" {
		t.Fatalf("stale expected: %v", err)
	}
	if _, err = write(1, `{"agents":{"new":{}}}`, true); err == nil {
		t.Fatal("missing rollback error")
	}
	latest, err := ReadAppliedConfig(ctx, s.DB, 0)
	if err != nil || latest.Version != 1 {
		t.Fatalf("rollback changed snapshot: %+v %v", latest, err)
	}
	if _, err = write(1, `null`, false); ErrorCode(err) != "invalid_input" {
		t.Fatalf("null accepted: %v", err)
	}
	c, err := write(1, `{"agents":{"new":{}}}`, false)
	if err != nil || c.Version != 2 {
		t.Fatalf("next version: %+v %v", c, err)
	}
	old, err := ReadAppliedConfig(ctx, s.DB, 1)
	if err != nil || old.Digest != a.Digest {
		t.Fatalf("history: %+v %v", old, err)
	}
}
