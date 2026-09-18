// Package runlog owns the disposable, redacted JSONL stream emitted by the AI
// runtime. It deliberately has no dependency on the authoritative database.
package runlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const SchemaVersion = 1

type Event struct {
	SchemaVersion int      `json:"schema_version"`
	Timestamp     string   `json:"timestamp"`
	Level         string   `json:"level"`
	Component     string   `json:"component"`
	Event         string   `json:"event"`
	RuntimeID     string   `json:"runtime_id"`
	BatchID       string   `json:"batch_id,omitempty"`
	TaskID        string   `json:"task_id,omitempty"`
	AttemptID     string   `json:"attempt_id,omitempty"`
	TraceID       string   `json:"trace_id,omitempty"`
	Status        string   `json:"status,omitempty"`
	DurationMS    int64    `json:"duration_ms,omitempty"`
	ErrorCode     string   `json:"error_code,omitempty"`
	Model         string   `json:"model,omitempty"`
	InputTokens   int64    `json:"input_tokens,omitempty"`
	OutputTokens  int64    `json:"output_tokens,omitempty"`
	CostUSD       float64  `json:"cost_usd,omitempty"`
	ToolKinds     []string `json:"tool_kinds,omitempty"`
	Summary       string   `json:"summary,omitempty"`
}

type Options struct {
	Retention time.Duration
	MaxBytes  int64
	FileBytes int64
	Now       func() time.Time
}

func defaults(o Options) Options {
	if o.Retention <= 0 {
		o.Retention = 30 * 24 * time.Hour
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 1 << 30
	}
	if o.FileBytes <= 0 {
		o.FileBytes = 10 << 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

type Logger struct {
	mu        sync.Mutex
	dir       string
	runtimeID string
	stdout    io.Writer
	opts      Options
	file      *os.File
	path      string
	bytes     int64
	day       string
	sequence  int
}

func Open(home, runtimeID string, stdout io.Writer, opts Options) (*Logger, error) {
	if strings.TrimSpace(runtimeID) == "" {
		return nil, fmt.Errorf("runtime id is required")
	}
	dir := filepath.Join(home, "runtime", "logs", runtimeID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	l := &Logger{dir: dir, runtimeID: runtimeID, stdout: stdout, opts: defaults(opts)}
	if err := Sweep(dir, l.opts, ""); err != nil {
		return nil, err
	}
	if err := l.rotate(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) Path() string { l.mu.Lock(); defer l.mu.Unlock(); return l.path }

func (l *Logger) rotate() error {
	if l.file != nil {
		if err := errors.Join(l.file.Sync(), l.file.Close()); err != nil {
			return err
		}
	}
	now := l.opts.Now().UTC()
	l.day = now.Format("20060102")
	for {
		l.sequence++
		name := fmt.Sprintf("%s-%s-%d-%03d.jsonl", l.day, now.Format("150405.000000000"), os.Getpid(), l.sequence)
		path := filepath.Join(l.dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		l.file, l.path, l.bytes = f, path, 0
		return os.Chmod(path, 0600)
	}
}

var secretPattern = regexp.MustCompile(`(?i)(access[_-]?token|auth[_-]?token|api[_-]?key|app[_-]?secret|password|sessionwebhook)\s*[:=]\s*[^\s,;]+`)
var urlPattern = regexp.MustCompile(`https?://[^\s]+`)

func safeText(value string, max int) string {
	value = secretPattern.ReplaceAllString(value, "$1=[redacted]")
	value = urlPattern.ReplaceAllString(value, "[url-redacted]")
	value = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if len([]rune(value)) > max {
		value = string([]rune(value)[:max])
	}
	return value
}

func sanitize(e Event, runtimeID string, now time.Time) (Event, error) {
	if e.RuntimeID == "" {
		e.RuntimeID = runtimeID
	}
	if e.RuntimeID != runtimeID {
		return e, fmt.Errorf("runtime log event belongs to another runtime")
	}
	if e.Level == "" {
		e.Level = "info"
	}
	if !contains([]string{"debug", "info", "warn", "error"}, e.Level) {
		return e, fmt.Errorf("invalid log level")
	}
	if strings.TrimSpace(e.Component) == "" || strings.TrimSpace(e.Event) == "" {
		return e, fmt.Errorf("component and event are required")
	}
	e.SchemaVersion = SchemaVersion
	if e.Timestamp == "" {
		e.Timestamp = now.UTC().Format(time.RFC3339Nano)
	}
	if e.TraceID == "" {
		switch {
		case e.AttemptID != "":
			e.TraceID = e.AttemptID
		case e.BatchID != "":
			e.TraceID = e.BatchID
		case e.TaskID != "":
			e.TraceID = e.TaskID
		default:
			e.TraceID = e.RuntimeID
		}
	}
	if e.Status == "" {
		e.Status = "observed"
	}
	e.Component, e.Event, e.Status, e.ErrorCode, e.Model = safeText(e.Component, 64), safeText(e.Event, 64), safeText(e.Status, 64), safeText(e.ErrorCode, 64), safeText(e.Model, 100)
	e.Summary = safeText(e.Summary, 240)
	allowedTools := map[string]bool{"read": true, "edit": true, "write": true, "search": true, "git": true, "test": true, "build": true, "dws": true, "deploy": true, "business": true, "query": true, "skill": true, "local_tool": true, "glob": true, "grep": true, "other": true}
	seenTools := map[string]bool{}
	tools := make([]string, 0, len(e.ToolKinds))
	for _, raw := range e.ToolKinds {
		kind := strings.ToLower(safeText(raw, 64))
		if !allowedTools[kind] {
			kind = "other"
		}
		if !seenTools[kind] {
			seenTools[kind] = true
			tools = append(tools, kind)
		}
	}
	e.ToolKinds = tools
	if len(e.ToolKinds) > 32 {
		e.ToolKinds = e.ToolKinds[:32]
	}
	return e, nil
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (l *Logger) Emit(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.opts.Now().UTC()
	var err error
	e, err = sanitize(e, l.runtimeID, now)
	if err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if l.file == nil || l.day != now.Format("20060102") || l.bytes+int64(len(line)) > l.opts.FileBytes {
		if err = l.rotate(); err != nil {
			return err
		}
	}
	n, fileErr := l.file.Write(line)
	l.bytes += int64(n)
	var stdoutErr error
	if l.stdout != nil {
		_, stdoutErr = l.stdout.Write(line)
	}
	if fileErr != nil || stdoutErr != nil {
		return errors.Join(fileErr, stdoutErr)
	}
	return nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := errors.Join(l.file.Sync(), l.file.Close())
	l.file = nil
	return err
}

func (l *Logger) Maintain() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Sweep(l.dir, l.opts, l.path)
}

type FileInfo struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	ModifiedAt string `json:"modified_at"`
}

func Files(home, runtimeID string) ([]FileInfo, error) {
	dir := filepath.Join(home, "runtime", "logs", runtimeID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []FileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []FileInfo{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, FileInfo{Path: filepath.Join(dir, entry.Name()), Bytes: info.Size(), ModifiedAt: info.ModTime().UTC().Format(time.RFC3339Nano)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func Sweep(dir string, opts Options, active string) error {
	opts = defaults(opts)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	items := []item{}
	var total int64
	cutoff := opts.Now().UTC().Add(-opts.Retention)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		path := filepath.Join(dir, entry.Name())
		if path != active && info.ModTime().Before(cutoff) {
			if err = os.Remove(path); err != nil {
				return err
			}
			continue
		}
		items = append(items, item{path, info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	for _, it := range items {
		if total <= opts.MaxBytes {
			break
		}
		if it.path == active {
			continue
		}
		if err = os.Remove(it.path); err != nil {
			return err
		}
		total -= it.size
	}
	if total > opts.MaxBytes {
		return fmt.Errorf("runtime log quota is exhausted")
	}
	return nil
}

type Filter struct {
	Since     time.Time
	Until     time.Time
	Level     string
	Component string
	BatchID   string
	TaskID    string
	Limit     int
}

func matches(e Event, f Filter) bool {
	t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return false
	}
	if !f.Since.IsZero() && t.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !t.Before(f.Until) {
		return false
	}
	return (f.Level == "" || e.Level == f.Level) && (f.Component == "" || e.Component == f.Component) && (f.BatchID == "" || e.BatchID == f.BatchID) && (f.TaskID == "" || e.TaskID == f.TaskID)
}

func Show(home, runtimeID string, f Filter) ([]Event, error) {
	files, err := Files(home, runtimeID)
	if err != nil {
		return nil, err
	}
	out := []Event{}
	for _, file := range files {
		h, err := os.Open(file.Path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(h)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			var e Event
			if json.Unmarshal(scanner.Bytes(), &e) == nil && matches(e, f) {
				out = append(out, e)
			}
		}
		scanErr := scanner.Err()
		h.Close()
		if scanErr != nil {
			return nil, scanErr
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}

func Follow(ctx context.Context, home, runtimeID string, f Filter, out io.Writer) error {
	offsets := map[string]int64{}
	emit := func() error {
		files, err := Files(home, runtimeID)
		if err != nil {
			return err
		}
		for _, file := range files {
			h, err := os.Open(file.Path)
			if err != nil {
				return err
			}
			start := offsets[file.Path]
			if _, err = h.Seek(start, io.SeekStart); err != nil {
				h.Close()
				return err
			}
			scanner := bufio.NewScanner(h)
			scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
			consumed := start
			for scanner.Scan() {
				line := append([]byte(nil), scanner.Bytes()...)
				consumed += int64(len(line) + 1)
				var e Event
				if json.Unmarshal(line, &e) == nil && matches(e, f) {
					if _, err = out.Write(append(line, '\n')); err != nil {
						h.Close()
						return err
					}
				}
			}
			scanErr := scanner.Err()
			h.Close()
			if scanErr != nil {
				return scanErr
			}
			offsets[file.Path] = consumed
		}
		return nil
	}
	if err := emit(); err != nil {
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := emit(); err != nil {
				return err
			}
		}
	}
}
