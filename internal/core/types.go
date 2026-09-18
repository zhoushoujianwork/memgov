// Package core implements memgov's SQLite-owned memory domain.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const SchemaVersion = 25

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string                    { return e.Message }
func Fail(code, format string, args ...any) error { return &Error{code, fmt.Sprintf(format, args...)} }
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal"
}
func NewID() string        { return uuid.NewString() }
func Now() string          { return time.Now().UTC().Format(time.RFC3339Nano) }
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("core: non-JSON value: %v", err))
	}
	return string(b)
}
func Digest(v any) string { return Hash([]byte(JSON(v))) }

type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}
type Evidence struct {
	SourceID   string `json:"source_id"`
	FragmentID string `json:"fragment_id"`
	SHA256     string `json:"sha256"`
	Quote      string `json:"quote,omitempty"`
}
type Hotword struct {
	Canonical string   `json:"canonical"`
	Aliases   []string `json:"aliases"`
	Meaning   string   `json:"meaning"`
}
type Memory struct {
	ID            string     `json:"id,omitempty"`
	Category      string     `json:"category"`
	Title         string     `json:"title"`
	Summary       string     `json:"summary"`
	Content       string     `json:"content"`
	WorkspaceID   string     `json:"workspace_id,omitempty"`
	Entities      []string   `json:"entities,omitempty"`
	Tags          []string   `json:"tags,omitempty"`
	Applicability []string   `json:"applicability,omitempty"`
	Hotword       *Hotword   `json:"hotword,omitempty"`
	ObservedAt    string     `json:"observed_at,omitempty"`
	ValidFrom     string     `json:"valid_from,omitempty"`
	ValidUntil    string     `json:"valid_until,omitempty"`
	Evidence      []Evidence `json:"evidence"`
	Status        string     `json:"status,omitempty"`
	Version       int        `json:"version,omitempty"`
}
type Change struct {
	ObjectType string `json:"object_type,omitempty"`
	ObjectID   string `json:"object_id,omitempty"`
	MemoryID   string `json:"memory_id,omitempty"`
	Before     int    `json:"before"`
	After      int    `json:"after"`
}
type Operation struct {
	ID        string   `json:"id"`
	RequestID string   `json:"request_id"`
	Kind      string   `json:"kind"`
	Actor     string   `json:"actor"`
	Reason    string   `json:"reason"`
	Changes   []Change `json:"changes"`
	CreatedAt string   `json:"created_at"`
}
type Request struct {
	ID      string
	Command string
	Scope   string
	Actor   string
	Key     string
	Input   any
}
type Result struct {
	Data   json.RawMessage
	Cached bool
}

func ValidateMemory(m Memory) error {
	if !contains([]string{"fact", "preference", "constraint", "decision", "procedure", "lesson"}, m.Category) {
		return Fail("invalid_input", "invalid category %q", m.Category)
	}
	for k, v := range map[string]string{"title": m.Title, "summary": m.Summary, "content": m.Content} {
		if strings.TrimSpace(v) == "" {
			return Fail("invalid_input", "%s is required", k)
		}
	}
	if m.Status != "" && !contains([]string{"active", "disputed", "retired", "superseded"}, m.Status) {
		return Fail("invalid_input", "invalid status %q", m.Status)
	}
	if len(m.Evidence) == 0 {
		return Fail("invalid_input", "at least one evidence fragment is required")
	}
	if m.Hotword != nil {
		if strings.TrimSpace(m.Hotword.Canonical) == "" || strings.TrimSpace(m.Hotword.Meaning) == "" || len(m.Hotword.Aliases) == 0 {
			return Fail("invalid_input", "hotword requires canonical, meaning and at least one alias")
		}
		if len([]rune(m.Hotword.Canonical)) > 120 || len([]rune(m.Hotword.Meaning)) > 500 || len(m.Hotword.Aliases) > 32 {
			return Fail("invalid_input", "hotword exceeds its size boundary")
		}
		seen := map[string]bool{strings.ToLower(strings.TrimSpace(m.Hotword.Canonical)): true}
		for _, alias := range m.Hotword.Aliases {
			alias = strings.TrimSpace(alias)
			key := strings.ToLower(alias)
			if alias == "" || len([]rune(alias)) > 120 || seen[key] {
				return Fail("invalid_input", "hotword aliases must be unique, nonempty and different from canonical")
			}
			seen[key] = true
		}
	}
	for _, v := range []string{m.ObservedAt, m.ValidFrom, m.ValidUntil} {
		if v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return Fail("invalid_input", "dates must use RFC3339: %q", v)
			}
		}
	}
	if m.ValidFrom != "" && m.ValidUntil != "" {
		a, _ := time.Parse(time.RFC3339, m.ValidFrom)
		b, _ := time.Parse(time.RFC3339, m.ValidUntil)
		if !b.After(a) {
			return Fail("invalid_input", "valid_until must be after valid_from")
		}
	}
	if m.Category == "procedure" {
		lower := strings.ToLower(m.Content)
		for _, words := range [][]string{{"前提", "prerequisite"}, {"步骤", "steps"}, {"验证", "verification"}} {
			found := false
			for _, word := range words {
				found = found || strings.Contains(lower, word)
			}
			if !found {
				return Fail("invalid_input", "procedure must include prerequisites, steps and verification; mark missing evidence as unknown")
			}
		}
	}
	return nil
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
