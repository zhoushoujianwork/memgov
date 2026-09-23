// Package core implements memgov's SQLite-owned operational state.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const SchemaVersion = 27

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

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
