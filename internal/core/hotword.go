package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type HotwordCaptureInput struct {
	AttemptID string   `json:"attempt_id"`
	Canonical string   `json:"canonical"`
	Aliases   []string `json:"aliases"`
	Meaning   string   `json:"meaning"`
}

// HotwordContext returns only active, currently valid hotwords. When audience
// is set, the same publication checks used by group memory recall run before a
// hotword can enter the prompt.
func HotwordContext(ctx context.Context, q Queryer, scope string, audience *Audience, budget int) (string, error) {
	if budget <= 0 {
		return "", Fail("invalid_input", "hotword budget must be positive")
	}
	rows, err := q.QueryContext(ctx, "SELECT id,workspace_id,document FROM memories WHERE workspace_id IN (?,'global') AND status='active' ORDER BY CASE WHEN workspace_id=? THEN 0 ELSE 1 END,updated_at DESC,id", scopeID(scope), scopeID(scope))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type item struct {
		id, workspace string
		memory        Memory
	}
	items := []item{}
	now := time.Now()
	for rows.Next() {
		var it item
		var raw string
		if err = rows.Scan(&it.id, &it.workspace, &raw); err != nil {
			return "", err
		}
		if err = jsonUnmarshalMemory(raw, &it.memory); err != nil {
			return "", err
		}
		if it.memory.Hotword == nil || !memoryValidAt(it.memory, now) {
			continue
		}
		if audience != nil {
			d, checkErr := CheckDisclosure(ctx, q, *audience, it.id)
			if checkErr != nil {
				return "", checkErr
			}
			if !d.Allowed {
				continue
			}
		}
		items = append(items, it)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	var out strings.Builder
	used := 0
	for _, it := range items {
		h := it.memory.Hotword
		block := fmt.Sprintf("- %s ⇐ %s：%s\n", h.Canonical, strings.Join(h.Aliases, "、"), h.Meaning)
		blockSize := utf8.RuneCountInString(block)
		if used+blockSize > budget {
			continue
		}
		out.WriteString(block)
		used += blockSize
	}
	return out.String(), nil
}

func memoryValidAt(m Memory, now time.Time) bool {
	if m.ValidFrom != "" {
		t, err := time.Parse(time.RFC3339, m.ValidFrom)
		if err == nil && now.Before(t) {
			return false
		}
	}
	if m.ValidUntil != "" {
		t, err := time.Parse(time.RFC3339, m.ValidUntil)
		if err == nil && !now.Before(t) {
			return false
		}
	}
	return true
}

func jsonUnmarshalMemory(raw string, m *Memory) error {
	return json.Unmarshal([]byte(raw), m)
}

// CaptureRuntimeHotword applies a verified owner's explicit correction through
// Source-backed Candidate -> Review -> Apply. The current message must contain
// the canonical term and every alias, so model inference alone cannot persist a
// guessed mapping.
func (tx *Tx) CaptureRuntimeHotword(ctx context.Context, taskID string, in HotwordCaptureInput) (Memory, error) {
	var empty Memory
	canonical, meaning := strings.TrimSpace(in.Canonical), strings.TrimSpace(in.Meaning)
	aliases := append([]string{}, in.Aliases...)
	for i := range aliases {
		aliases[i] = strings.TrimSpace(aliases[i])
		if aliases[i] == "" {
			return empty, Fail("invalid_input", "hotword aliases cannot be empty")
		}
	}
	if in.AttemptID == "" || canonical == "" || meaning == "" || len(aliases) == 0 {
		return empty, Fail("invalid_input", "attempt_id, canonical, aliases and meaning are required")
	}
	var workspace, mode, owner, body, messageID, sourceID, fragmentID, fragmentDigest string
	var taskVersion, attemptTaskVersion int
	err := tx.Conn.QueryRowContext(ctx, `SELECT r.workspace_id,c.application_mode,c.owner_principal_id,mr.body,m.id,
coalesce(so.source_id,''),coalesce(f.id,''),coalesce(f.digest,''),t.version,a.task_version
FROM runtime_tasks t JOIN runtime_configs c ON c.id=t.runtime_id JOIN channel_routes r ON r.id=t.route_id
JOIN runtime_attempts a ON a.id=? AND a.task_id=t.id JOIN runtime_task_messages tm ON tm.task_id=t.id
JOIN messages m ON m.id=tm.message_id JOIN message_revisions mr ON mr.message_id=m.id AND mr.revision=m.current_revision
LEFT JOIN source_origins so ON so.message_id=m.id AND so.revision=m.current_revision
LEFT JOIN fragments f ON f.source_id=so.source_id
WHERE t.id=? AND t.status='running' AND a.status='running' AND m.sender_principal=c.owner_principal_id
AND m.self_authored=0 AND m.availability='available' ORDER BY f.rowid LIMIT 1`, in.AttemptID, taskID).Scan(
		&workspace, &mode, &owner, &body, &messageID, &sourceID, &fragmentID, &fragmentDigest, &taskVersion, &attemptTaskVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, Fail("denied", "hotword capture requires the current verified owner turn")
	}
	if err != nil {
		return empty, err
	}
	if mode != "direct" || owner == "" || taskVersion != attemptTaskVersion || sourceID == "" || fragmentID == "" {
		return empty, Fail("denied", "hotword capture requires current direct-message evidence")
	}
	task, err := ReadRuntimeTask(ctx, tx.Conn, taskID)
	if err != nil {
		return empty, err
	}
	if err = tx.CheckRuntimeAttemptPolicy(ctx, in.AttemptID, taskID, taskVersion); err != nil {
		return empty, err
	}
	config, err := ReadRuntime(ctx, tx.Conn, task.RuntimeID)
	if err != nil {
		return empty, err
	}
	policy, err := ResolveRuntimeTaskAgent(ctx, tx.Conn, config, task)
	if err != nil {
		return empty, err
	}
	if !contains(policy.Capabilities, "memory_read") || !contains(policy.Capabilities, "local_write") {
		return empty, Fail("denied", "Agent policy does not allow hotword memory updates")
	}
	lowerBody := strings.ToLower(body)
	if !strings.Contains(lowerBody, strings.ToLower(canonical)) {
		return empty, Fail("denied", "owner message does not contain the canonical term")
	}
	for _, alias := range aliases {
		if !strings.Contains(lowerBody, strings.ToLower(alias)) {
			return empty, Fail("denied", "owner message does not contain every proposed alias")
		}
	}
	sort.Slice(aliases, func(i, j int) bool { return strings.ToLower(aliases[i]) < strings.ToLower(aliases[j]) })
	evidence := Evidence{SourceID: sourceID, FragmentID: fragmentID, SHA256: fragmentDigest}
	memory := Memory{Category: "fact", Title: "热词：" + canonical, Summary: meaning, Content: fmt.Sprintf("标准名称：%s\n常见语音转写或别称：%s\n含义：%s", canonical, strings.Join(aliases, "、"), meaning), WorkspaceID: workspace, Entities: []string{canonical}, Tags: []string{"hotword", "speech-to-text"}, Applicability: []string{"语音转文本纠错", "名称消歧"}, Hotword: &Hotword{Canonical: canonical, Aliases: aliases, Meaning: meaning}, Evidence: []Evidence{evidence}, Status: "active"}
	action, target, expected := "create", "", 0
	rows, err := tx.Conn.QueryContext(ctx, "SELECT id,document FROM memories WHERE workspace_id=? AND status='active'", scopeID(workspace))
	if err != nil {
		return empty, err
	}
	for rows.Next() {
		var id, raw string
		var existing Memory
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return empty, err
		}
		if jsonUnmarshalMemory(raw, &existing) == nil && existing.Hotword != nil && strings.EqualFold(existing.Hotword.Canonical, canonical) {
			action, target, expected = "update", id, existing.Version
			seen, merged := map[string]bool{}, []string{}
			for _, v := range append(append([]string{}, existing.Hotword.Aliases...), aliases...) {
				v = strings.TrimSpace(v)
				key := strings.ToLower(v)
				if key != "" && !seen[key] && !strings.EqualFold(v, canonical) {
					seen[key] = true
					merged = append(merged, v)
				}
			}
			sort.Slice(merged, func(i, j int) bool { return strings.ToLower(merged[i]) < strings.ToLower(merged[j]) })
			memory.Hotword.Aliases = merged
			memory.Evidence = append([]Evidence{}, existing.Evidence...)
			foundEvidence := false
			for _, prior := range existing.Evidence {
				if prior.SourceID == evidence.SourceID && prior.FragmentID == evidence.FragmentID && prior.SHA256 == evidence.SHA256 {
					foundEvidence = true
					break
				}
			}
			if !foundEvidence {
				memory.Evidence = append(memory.Evidence, evidence)
			}
			break
		}
	}
	rows.Close()
	input := CandidateInput{Action: action, TargetID: target, ExpectedVersion: expected, Memory: memory, Reason: "verified owner corrected a speech-to-text term"}
	candidate, err := tx.SubmitCandidate(ctx, input, "", "runtime-hotword")
	if err != nil {
		return empty, err
	}
	if _, err = tx.SubmitCandidateReview(ctx, candidate.ID, "runtime-hotword-explicit-correction", "accept", nil); err != nil {
		return empty, err
	}
	value, err := tx.ApplyCandidate(ctx, candidate.ID, candidate.Digest)
	if err != nil {
		return empty, err
	}
	result := value.(map[string]any)
	applied, ok := result["memory"].(Memory)
	if !ok {
		return empty, Fail("internal", "hotword apply returned an invalid memory")
	}
	return applied, nil
}
