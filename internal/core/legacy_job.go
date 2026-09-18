package core

// The processing CLI has been removed. These private shapes remain only so a
// sensitive purge can redact job payloads stored by earlier releases.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type legacyJobInput struct {
	SourceIDs        []string `json:"source_ids,omitempty"`
	ContextSourceIDs []string `json:"context_source_ids,omitempty"`
	CandidateIDs     []string `json:"candidate_ids,omitempty"`
	Issues           []string `json:"issues,omitempty"`
}

type legacyJob struct {
	ID     string
	Input  legacyJobInput
	Output json.RawMessage
}

const legacyJobColumns = "id,input,output"

func scanLegacyJob(row scanner) (legacyJob, error) {
	var job legacyJob
	var input, output string
	if err := row.Scan(&job.ID, &input, &output); err != nil {
		return job, err
	}
	if err := json.Unmarshal([]byte(input), &job.Input); err != nil {
		return job, err
	}
	job.Output = json.RawMessage(output)
	return job, nil
}

func readLegacyJob(ctx context.Context, q Queryer, id string) (legacyJob, error) {
	job, err := scanLegacyJob(q.QueryRowContext(ctx, "SELECT "+legacyJobColumns+" FROM jobs WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return job, Fail("not_found", "stored job not found")
	}
	return job, err
}
