// Package console serves local owner views with read-only business queries.
package console

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

//go:embed static/*
var assets embed.FS

type Options struct {
	Home, ConfigPath, Version, Build, Host string
	ResolveSkills                          func(core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error)
	AgentConfig                            *AgentConfigAccess
	Restart                                func(context.Context, RestartRequest) (func(), error)
	ResumeTask                             func(context.Context, string, int) (any, error)
}

// RestartRequest keeps the chosen listener address across process replacement.
type RestartRequest struct {
	Host string
}

// Agent declarations are file edits only; runtime application stays explicit.
type AgentConfigAccess struct {
	List    func(context.Context, core.Queryer) (any, error)
	Preview func(context.Context, core.Queryer, json.RawMessage) (any, error)
	Save    func(context.Context, core.Queryer, json.RawMessage) (any, error)
}

type Server struct {
	db              *sql.DB
	opts            Options
	mu              sync.Mutex
	installedMod    time.Time
	installedSize   int64
	installedBuild  string
	tagMu           sync.Mutex
	tagResult       TagStatus
	tagExpires      time.Time
	tagClient       *http.Client
	runtimesMu      sync.Mutex
	runtimesResult  []RuntimeView
	runtimesExpires time.Time
}

func Open(ctx context.Context, opts Options) (*Server, error) {
	host, _, err := net.SplitHostPort(opts.Host)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return nil, core.Fail("invalid_input", "console requires a loopback IP address")
	}
	path := filepath.Join(opts.Home, "state.db")
	if _, err := os.Stat(path); err != nil {
		return nil, core.Fail("not_found", "database not initialized; run memgov init")
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(3000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	var version int
	if err = db.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		db.Close()
		return nil, core.Fail("conflict", "cannot inspect database schema")
	}
	if version != core.SchemaVersion {
		db.Close()
		return nil, core.Fail("conflict", "database schema differs from this binary; migrate explicitly before opening the console")
	}
	return &Server{db: db, opts: opts}, nil
}

func (s *Server) Close() error { return s.db.Close() }
func (s *Server) URL() string  { return "http://" + s.opts.Host + "/" }

func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(assets, "static")
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.Host != s.opts.Host {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && origin != "http://"+s.opts.Host {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
			http.Error(w, "cross-site request denied", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			restart := r.Method == http.MethodPost && r.URL.Path == "/api/v1/service/restart" && s.opts.Restart != nil
			resumeID := ""
			if r.Method == http.MethodPost && s.opts.ResumeTask != nil && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/") {
				segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/"), "/")
				if len(segments) == 2 && segments[0] != "" && segments[1] == "resume" {
					resumeID = segments[0]
				}
			}
			write := resumeID != "" || restart || (r.Method == http.MethodPost && s.opts.AgentConfig != nil && (r.URL.Path == "/api/v1/agent-config/preview" || r.URL.Path == "/api/v1/agent-config/save"))
			if r.Method != http.MethodGet && !write {
				http.Error(w, "read-only console", http.StatusMethodNotAllowed)
				return
			}
			if write {
				if origin != "http://"+s.opts.Host || r.Header.Get("X-Memgov-Console") != "1" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
					s.reply(w, nil, core.Fail("denied", "same-origin JSON console request required"))
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			}
			timeout := 5 * time.Second
			if r.URL.Path == "/api/v1/version/check" {
				timeout = 9 * time.Second
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			if resumeID != "" {
				var input struct {
					ExpectedVersion int `json:"expected_version"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ExpectedVersion < 1 {
					s.reply(w, nil, core.Fail("invalid_input", "current expected_version is required"))
					return
				}
				data, err := s.opts.ResumeTask(ctx, resumeID, input.ExpectedVersion)
				s.reply(w, data, err)
				return
			}
			if restart {
				var input struct{}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					s.reply(w, nil, core.Fail("invalid_input", "invalid restart request"))
					return
				}
				commit, err := s.opts.Restart(ctx, RestartRequest{Host: s.opts.Host})
				if err != nil {
					s.reply(w, nil, err)
					return
				}
				s.reply(w, map[string]string{"state": "restarting"}, nil)
				// Send the acknowledgement before shutting down this HTTP server.
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				commit()
				return
			}
			if id := terminalTaskID(r.URL.Path); id != "" {
				s.terminal(w, r, id)
				return
			}
			if r.URL.Path == "/api/v1/version/check" {
				s.reply(w, s.checkTag(ctx), nil)
				return
			}
			data, err := s.query(ctx, r)
			s.reply(w, data, err)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only console", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/tasks", "/memories", "/running", "/settings", "/tasks/", "/memories/", "/running/", "/settings/":
			index := r.Clone(r.Context())
			index.URL.Path = "/"
			files.ServeHTTP(w, index)
		case "/", "/index.html":
			files.ServeHTTP(w, r)
		default:
			// Serve only Vite's embedded production assets, never web source files.
			if strings.HasPrefix(r.URL.Path, "/assets/") && path.Clean(r.URL.Path) == r.URL.Path {
				if info, err := fs.Stat(static, strings.TrimPrefix(r.URL.Path, "/")); err == nil && !info.IsDir() {
					files.ServeHTTP(w, r)
					return
				}
			}
			http.NotFound(w, r)
		}
	})
}

func (s *Server) reply(w http.ResponseWriter, data any, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	result := map[string]any{"schema_version": 1, "sampled_at": time.Now().UTC(), "ok": err == nil}
	if err != nil {
		code := core.ErrorCode(err)
		status := http.StatusInternalServerError
		switch code {
		case "denied":
			status = 403
		case "invalid_input":
			status = 400
		case "not_found":
			status = 404
		case "conflict":
			status = 409
		}
		message := err.Error()
		if code == "internal" {
			message = "local query failed; inspect database and log availability"
		}
		result["error"] = map[string]string{"code": code, "message": message}
		w.WriteHeader(status)
	} else {
		result["data"] = data
	}
	_ = json.NewEncoder(w).Encode(result)
}
