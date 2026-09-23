package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestSourceFragmentCoverageAndDuplicateLocations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	body := ""
	for i := 0; i < 1500; i++ {
		body += fmt.Sprintf("第 %d 行记录。\n", i)
	}
	var source Source
	for _, uri := range []string{"fixture://a", "fixture://b"} {
		result, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) { return tx.Ingest(ctx, SourceInput{URI: uri, Content: body}) })
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(result.Data, &source); err != nil {
			t.Fatal(err)
		}
	}
	joined := ""
	for _, f := range source.Fragments {
		joined += f.Content
		if Hash([]byte(f.Content)) != f.SHA256 {
			t.Fatal("bad digest")
		}
	}
	if joined != body || len(source.Locations) != 2 || len(source.Fragments) < 2 {
		t.Fatal("lost fragments or duplicate source")
	}
}

func fixtureSource(t *testing.T, s *Store) Source {
	t.Helper()
	var out Source
	_, err := s.Mutate(context.Background(), Request{Scope: "global"}, func(tx *Tx) (any, error) {
		var e error
		out, e = tx.Ingest(context.Background(), SourceInput{URI: "test://source", Content: "release permission evidence"})
		return out, e
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
