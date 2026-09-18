package core

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRevisionAuditAndReviewAreQueryable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var source Source
	_, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		var ingestErr error
		source, ingestErr = tx.Ingest(ctx, SourceInput{URI: "test://history", Content: "发布前检查权限。"})
		return source, ingestErr
	})
	if err != nil {
		t.Fatal(err)
	}
	fragment := source.Fragments[0]
	input := CandidateInput{Reason: "record reusable check", Memory: Memory{
		Category: "fact", Title: "部署检查", Summary: "发布前检查权限。", Content: "发布前检查权限。",
		Evidence: []Evidence{{SourceID: source.ID, FragmentID: fragment.ID, SHA256: fragment.SHA256}},
	}}
	var candidate Candidate
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		var submitErr error
		candidate, submitErr = tx.SubmitCandidate(ctx, input, "", "author")
		return candidate, submitErr
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		return tx.SubmitCandidateReview(ctx, candidate.ID, "reviewer", "accept", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Mutate(ctx, Request{Scope: "global"}, func(tx *Tx) (any, error) {
		return tx.ApplyCandidate(ctx, candidate.ID, candidate.Digest)
	})
	if err != nil {
		t.Fatal(err)
	}
	var applied struct {
		Memory Memory `json:"memory"`
	}
	if err = json.Unmarshal(result.Data, &applied); err != nil {
		t.Fatal(err)
	}
	memory := applied.Memory
	history, err := HistoryWithOperations(ctx, s.DB, memory.ID, "global")
	if err != nil || len(history) != 1 || history[0].Version != 1 || history[0].Operation.Kind != "candidate.apply" {
		t.Fatal(history, err)
	}
	reviews, err := CandidateReviews(ctx, s.DB, candidate.ID, "global")
	if err != nil || len(reviews) != 1 || reviews[0].Reviewer != "reviewer" || reviews[0].CandidateDigest != candidate.Digest {
		t.Fatal(reviews, err)
	}
}
