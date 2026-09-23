// Package scenario implements the read-only DingTalk scenario driver.
package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type Request struct {
	Person string `json:"person"`
	Start  string `json:"start"`
	End    string `json:"end"`
	// Profile pins every dws call to one account. It must exactly match a
	// profile value returned by dws profile list.
	Profile string `json:"profile,omitempty"`
}
type Finding struct {
	Topic    string   `json:"topic"`
	Status   string   `json:"status"`
	Evidence []string `json:"evidence"`
}
type Result struct {
	Status     string    `json:"status"`
	SelfID     string    `json:"self_id"`
	PersonID   string    `json:"person_id"`
	Complete   bool      `json:"complete"`
	Findings   []Finding `json:"findings"`
	Candidates []string  `json:"candidates"`
}
type Runner func(context.Context, ...string) ([]byte, error)
type Person struct {
	ID string `json:"userId"`
}
type Message struct {
	ID      string `json:"messageId"`
	Text    string `json:"text"`
	Time    string `json:"time"`
	Created string `json:"createTime"`
}

// Command uses argv directly; message text is never executable input.
func Command(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dws", args...)
	cmd.WaitDelay = time.Second
	b, err := cmd.Output()
	if ctx.Err() != nil {
		return b, ctx.Err()
	}
	return b, err
}

// call accepts plain JSON and the actual dws success/result envelope. An
// unexpected envelope is an error, never an empty successful lookup.
func call(ctx context.Context, run Runner, args ...string) (json.RawMessage, string) {
	b, err := run(ctx, append(args, "--format", "json")...)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return nil, "timeout"
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		if err != nil {
			return nil, "cli_error"
		}
		return nil, "invalid_response"
	}
	var detail struct {
		Code string `json:"code"`
	}
	if raw, ok := obj["error"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &detail) != nil {
			return nil, "invalid_response"
		}
		switch detail.Code {
		case "auth_required", "permission_denied", "timeout":
			return nil, detail.Code
		default:
			return nil, "cli_error"
		}
	}
	if err != nil {
		return nil, "cli_error"
	}
	if raw, ok := obj["success"]; ok {
		var success bool
		if json.Unmarshal(raw, &success) != nil || !success {
			return nil, "cli_error"
		}
		raw, ok = obj["result"]
		if !ok || string(raw) == "null" {
			return nil, "invalid_response"
		}
		return raw, ""
	}
	return b, ""
}

func scopedCall(ctx context.Context, run Runner, profile string, args ...string) (json.RawMessage, string) {
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	return call(ctx, run, args...)
}
func people(raw json.RawMessage) ([]Person, error) {
	var out []Person
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
	} else {
		var envelope struct {
			Items *[]Person `json:"items"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, err
		}
		if envelope.Items == nil {
			return nil, errors.New("missing people")
		}
		out = *envelope.Items
	}
	seen := map[string]bool{}
	unique := []Person{}
	for _, p := range out {
		if strings.TrimSpace(p.ID) == "" {
			return nil, errors.New("missing ID")
		}
		if !seen[p.ID] {
			unique = append(unique, p)
			seen[p.ID] = true
		}
	}
	return unique, nil
}

func Run(ctx context.Context, req Request, run Runner) Result {
	return runWithCache(ctx, req, run, defaultIdentityCache())
}

func runWithCache(ctx context.Context, req Request, run Runner, cache identityCache) Result {
	r := Result{Status: "invalid_input"}
	start, e1 := time.Parse(time.RFC3339, req.Start)
	end, e2 := time.Parse(time.RFC3339, req.End)
	if strings.TrimSpace(req.Person) == "" || e1 != nil || e2 != nil || !start.Before(end) {
		return r
	}
	self, profile, status := currentIdentity(ctx, run, req.Profile, cache)
	if status != "" {
		r.Status = status
		return r
	}
	r.SelfID = self.ID
	raw, status := scopedCall(ctx, run, profile, "aisearch", "person", "--query", req.Person, "--dimension", "name")
	if status != "" {
		r.Status = status
		return r
	}
	persons, err := people(raw)
	if err != nil {
		r.Status = "invalid_response"
		return r
	}
	if len(persons) == 0 {
		r.Status = "person_not_found"
		return r
	}
	if len(persons) > 1 {
		for _, p := range persons {
			r.Candidates = append(r.Candidates, p.ID)
		}
		_, status = scopedCall(ctx, run, profile, "contact", "user", "get", "--ids", strings.Join(r.Candidates, ","))
		if status != "" {
			r.Status = status
			return r
		}
		r.Status = "needs_clarification"
		return r
	}
	r.PersonID = persons[0].ID
	raw, status = scopedCall(ctx, run, profile, "chat", "+chat-messages", "--user", r.PersonID, "--start", req.Start, "--end", req.End, "--order", "asc", "--page-all", "--page-limit", "5", "--max-items", "150", "--limit", "30")
	if status != "" {
		r.Status = status
		return r
	}
	var ledger struct {
		Messages    *[]Message `json:"messages"`
		Complete    *bool      `json:"complete"`
		HasMore     *bool      `json:"hasMore"`
		Partial     bool       `json:"partial"`
		Truncated   bool       `json:"truncated"`
		FailedCount int        `json:"failedCount"`
	}
	if json.Unmarshal(raw, &ledger) != nil || ledger.Messages == nil || ledger.Complete == nil || ledger.HasMore == nil {
		r.Status = "invalid_response"
		return r
	}
	r.Complete = *ledger.Complete && !*ledger.HasMore && !ledger.Partial && !ledger.Truncated && ledger.FailedCount == 0
	messages := []Message{}
	seen := map[string]bool{}
	for _, m := range *ledger.Messages {
		if m.ID == "" {
			r.Complete = false
			r.Status = "invalid_response"
			return r
		}
		stamp := m.Time
		if stamp == "" {
			stamp = m.Created
		}
		when, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			when, err = time.ParseInLocation("2006-01-02 15:04:05", stamp, start.Location())
		}
		if err != nil {
			r.Complete = false
			r.Status = "invalid_response"
			return r
		}
		if when.Before(start) || !when.Before(end) {
			continue
		}
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		m.Time = when.UTC().Format(time.RFC3339Nano)
		messages = append(messages, m)
	}
	sort.SliceStable(messages, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, messages[i].Time)
		b, _ := time.Parse(time.RFC3339Nano, messages[j].Time)
		return a.Before(b)
	})
	r.Findings = Extract(messages)
	switch {
	case !r.Complete:
		r.Status = "partial"
	case len(messages) == 0:
		r.Status = "no_messages"
	case len(r.Findings) == 0:
		r.Status = "needs_analysis"
	default:
		r.Status = "ok"
	}
	return r
}

// Main is shared with the executable so input validation can be tested without
// reading fixture paths, environment answers or the runtime database or private Agent workspaces.
func Main(ctx context.Context, in io.Reader, out io.Writer, run Runner) error {
	b, err := io.ReadAll(io.LimitReader(in, 65537))
	if err != nil {
		return err
	}
	req := Request{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if len(b) > 65536 || dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
		return json.NewEncoder(out).Encode(Result{Status: "invalid_input"})
	}
	return json.NewEncoder(out).Encode(Run(ctx, req, run))
}
