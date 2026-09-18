// Package tasklog stores bounded, disposable owner-only execution output.
// It is separate from structural diagnostics and authoritative task state.
package tasklog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const MaxFileBytes int64 = 4 << 20
const TailBytes int64 = 256 << 10
const Retention = 30 * 24 * time.Hour

type Event struct {
	Timestamp string `json:"timestamp"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	Offset    int64  `json:"offset,omitempty"`
}

type Writer struct {
	mu      sync.Mutex
	file    *os.File
	bytes   int64
	limited bool
}

var component = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

func path(home, runtimeID, taskID, attemptID string, create bool) (string, error) {
	for _, id := range []string{runtimeID, taskID, attemptID} {
		if !component.MatchString(id) {
			return "", fmt.Errorf("invalid output identity")
		}
	}
	root, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	dir := root
	for _, part := range []string{"runtime", "output", runtimeID, taskID} {
		dir = filepath.Join(dir, part)
		if create {
			if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
				return "", err
			}
		}
		st, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("unsafe output directory")
		}
	}
	name := filepath.Join(dir, attemptID+".jsonl")
	if st, err := os.Lstat(name); err == nil {
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("unsafe output file")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return name, nil
}

func Open(home, runtimeID, taskID, attemptID string) (*Writer, error) {
	name, err := path(home, runtimeID, taskID, attemptID, true)
	if err != nil {
		return nil, err
	}
	// Apply the global quota before opening an attempt; recent files are kept.
	maintain(filepath.Join(home, "runtime", "output"))
	file, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	st, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &Writer{file: file, bytes: st.Size(), limited: st.Size() >= MaxFileBytes-1024}, nil
}

var secret = regexp.MustCompile(`(?i)((?:access[_-]?token|auth[_-]?token|api[_-]?key|app[_-]?secret|client[_-]?secret|secret[_-]?access[_-]?key|password|sessionwebhook|authorization|token|secret|credentials)["']?\s*[:=]\s*["']?)([^\s,"';]+)`)
var bearer = regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[A-Za-z0-9._~+/-]+=*`)
var token = regexp.MustCompile(`\b(?:sk-(?:ant-)?[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]{8,}|AKIA[A-Z0-9]{16})\b`)
var url = regexp.MustCompile(`https?://[^\s"<>]+`)
var ansi = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07]*(?:\x07|\x1b\\\\))")

func SafeText(text string) string {
	text = bearer.ReplaceAllString(text, "Bearer [redacted]")
	text = secret.ReplaceAllString(text, "${1}[redacted]")
	text = token.ReplaceAllString(text, "[redacted]")
	text = url.ReplaceAllString(text, "[url-redacted]")
	text = ansi.ReplaceAllString(text, "")
	text = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		if r == 127 {
			return -1
		}
		return r
	}, text)
	runes := []rune(text)
	if len(runes) > 8000 {
		text = string(runes[:8000]) + "\n[本条输出已截断]"
	}
	return text
}

func (w *Writer) Emit(kind, text string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil || w.limited {
		return
	}
	event := Event{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Text: SafeText(text)}
	raw, _ := json.Marshal(event)
	raw = append(raw, '\n')
	if w.bytes+int64(len(raw)) > MaxFileBytes-1024 {
		event.Kind = "limit"
		event.Text = "本次尝试的过程输出已达到 4 MiB 上限；任务仍继续执行。"
		raw, _ = json.Marshal(event)
		raw = append(raw, '\n')
		w.limited = true
	}
	if n, err := w.file.Write(raw); err == nil {
		w.bytes += int64(n)
	}
}
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// Claude accepts only public message blocks, never thinking or input prompts.
func (w *Writer) Claude(raw []byte) {
	if w == nil {
		return
	}
	var e struct {
		Type, Subtype, Model, Result string
		IsError                      bool `json:"is_error"`
		Errors                       []string
		Message                      struct {
			Content []struct {
				Type, Text, Name string
				Input            json.RawMessage
				Content          json.RawMessage `json:"content"`
				IsError          bool            `json:"is_error"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	switch e.Type {
	case "system":
		if e.Subtype == "init" {
			w.Emit("status", "Claude 已连接 · "+e.Model)
		}
	case "assistant":
		for _, c := range e.Message.Content {
			switch c.Type {
			case "text":
				w.Emit("assistant", c.Text)
			case "tool_use":
				w.Emit("tool", c.Name+"\n"+safeInput(c.Input))
			}
		}
	case "user":
		for _, c := range e.Message.Content {
			if c.Type == "tool_result" {
				kind := "output"
				if c.IsError {
					kind = "error"
				}
				w.Emit(kind, publicContent(c.Content))
			}
		}
	case "result":
		if e.IsError || strings.HasPrefix(e.Subtype, "error") {
			w.Emit("error", e.Result+"\n"+strings.Join(e.Errors, "\n"))
		} else if e.Result != "" {
			w.Emit("result", e.Result)
		}
	}
}

func safeInput(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "[工具参数不可读]"
	}
	var sanitize func(any) any
	sanitize = func(value any) any {
		switch v := value.(type) {
		case string:
			return SafeText(v)
		case []any:
			for i := range v {
				v[i] = sanitize(v[i])
			}
		case map[string]any:
			for key, item := range v {
				v[key] = sanitize(item)
			}
		}
		return value
	}
	body, _ := json.Marshal(sanitize(value))
	return string(body)
}
func publicContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(raw, &blocks) != nil {
		return "[工具返回非文本内容]"
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type Tail struct {
	Events    []Event `json:"events"`
	Cursor    int64   `json:"cursor"`
	Truncated bool    `json:"truncated"`
	Available bool    `json:"available"`
}

func Read(home, runtimeID, taskID, attemptID string, cursor int64) (Tail, error) {
	out := Tail{Events: []Event{}}
	name, err := path(home, runtimeID, taskID, attemptID, false)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	file, err := os.Open(name)
	if err != nil {
		return out, err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return out, err
	}
	if time.Since(st.ModTime()) > Retention {
		return out, nil
	}
	out.Available = true
	size := st.Size()
	if cursor < 0 || cursor > size {
		cursor = 0
	}
	if size-cursor > TailBytes {
		cursor = size - TailBytes
		out.Truncated = true
	}
	raw := make([]byte, size-cursor)
	n, err := file.ReadAt(raw, cursor)
	if err != nil && !errors.Is(err, io.EOF) {
		return out, err
	}
	raw = raw[:n]
	if out.Truncated {
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			cursor += int64(i + 1)
			raw = raw[i+1:]
		} else {
			return out, nil
		}
	}
	for {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line := raw[:i]
		cursor += int64(i + 1)
		raw = raw[i+1:]
		var event Event
		if json.Unmarshal(line, &event) == nil {
			event.Offset = cursor
			event.Text = SafeText(event.Text)
			out.Events = append(out.Events, event)
		}
	}
	out.Cursor = cursor
	return out, nil
}
