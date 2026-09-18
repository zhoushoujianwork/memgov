package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/observation"
)

// LaunchAgent is scoped to one canonical data directory and the current user.
// It never installs a root daemon or copies the caller's secret environment.
type LaunchAgent struct {
	Label     string `json:"label"`
	Path      string `json:"path"`
	LogPath   string `json:"log_path"`
	Installed bool   `json:"installed"`
	Loaded    bool   `json:"loaded"`
	domain    string
	run       func(context.Context, ...string) error
}

func UserAgent(home string) (*LaunchAgent, error) {
	if runtime.GOOS != "darwin" {
		return nil, core.Fail("unavailable", "system service installation currently supports macOS launchd only")
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	label := fmt.Sprintf("com.mikas.memgov.%x", sha256.Sum256([]byte(observation.CanonicalHome(home))))[:41]
	m := &LaunchAgent{Label: label, Path: filepath.Join(userHome, "Library", "LaunchAgents", label+".plist"), LogPath: filepath.Join(directory(home), "launchd.log"), domain: fmt.Sprintf("gui/%d", os.Getuid())}
	m.run = func(ctx context.Context, args ...string) error {
		out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl %s: %w: %s", args[0], err, bytes.TrimSpace(out))
		}
		return nil
	}
	return m, nil
}

func (m *LaunchAgent) target() string { return m.domain + "/" + m.Label }

func (m *LaunchAgent) Inspect(ctx context.Context) error {
	_, err := os.Stat(m.Path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	m.Installed = err == nil
	// print returns a nonzero status for a job that is not bootstrapped.
	m.Loaded = m.run(ctx, "print", m.target()) == nil
	return ctx.Err()
}

type AgentOptions struct {
	Executable, Home, Config, WorkingDirectory, SearchPath, UserHome string
	Port                                                             int
	NoUI                                                             bool
}

func (m *LaunchAgent) definition(o AgentOptions) ([]byte, error) {
	if !filepath.IsAbs(o.Executable) || !filepath.IsAbs(o.Home) || !filepath.IsAbs(o.Config) || !filepath.IsAbs(o.WorkingDirectory) {
		return nil, core.Fail("invalid_input", "launchd paths must be absolute")
	}
	if o.Port < 1 || o.Port > 65535 {
		return nil, core.Fail("invalid_input", "managed service requires a fixed --port in 1..65535")
	}
	args := []string{o.Executable, "service", "run", "--home", o.Home, "--config", o.Config, "--port", fmt.Sprint(o.Port)}
	if o.NoUI {
		args = append(args, "--no-ui")
	}
	data := struct {
		Label, Log, CWD, SearchPath, UserHome string
		Args                                  []string
	}{m.Label, m.LogPath, o.WorkingDirectory, o.SearchPath, o.UserHome, args}
	t := template.Must(template.New("plist").Funcs(template.FuncMap{"xml": func(s string) (string, error) {
		var b bytes.Buffer
		err := xml.EscapeText(&b, []byte(s))
		return b.String(), err
	}}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>{{xml .Label}}</string>
<key>ProgramArguments</key><array>{{range .Args}}<string>{{xml .}}</string>{{end}}</array>
<key>WorkingDirectory</key><string>{{xml .CWD}}</string>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>{{xml .SearchPath}}</string><key>HOME</key><string>{{xml .UserHome}}</string></dict>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>5</integer>
<key>ExitTimeOut</key><integer>30</integer>
<key>StandardOutPath</key><string>{{xml .Log}}</string>
<key>StandardErrorPath</key><string>{{xml .Log}}</string>
<key>ProcessType</key><string>Background</string>
</dict></plist>
`))
	var out bytes.Buffer
	err := t.Execute(&out, data)
	return out.Bytes(), err
}

func (m *LaunchAgent) Install(ctx context.Context, o AgentOptions) error {
	body, err := m.definition(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.LogPath), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(m.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	f.Close()
	if err := m.Stop(ctx); err != nil {
		return err
	}
	// Also migrate an existing, unmanaged unified process before bootstrap.
	if err := Stop(ctx, o.Home); err != nil {
		return err
	}
	if err := os.WriteFile(m.Path+".tmp", body, 0600); err != nil {
		return err
	}
	if err := os.Rename(m.Path+".tmp", m.Path); err != nil {
		return err
	}
	return m.Start(ctx)
}

func (m *LaunchAgent) Start(ctx context.Context) error {
	if err := m.Inspect(ctx); err != nil {
		return err
	}
	if !m.Installed {
		return core.Fail("not_found", "system service is not installed; use service install")
	}
	if err := m.run(ctx, "enable", m.target()); err != nil {
		return err
	}
	if m.Loaded {
		return m.run(ctx, "kickstart", m.target())
	}
	return m.run(ctx, "bootstrap", m.domain, m.Path)
}

func (m *LaunchAgent) Stop(ctx context.Context) error {
	if err := m.Inspect(ctx); err != nil {
		return err
	}
	if !m.Installed && !m.Loaded {
		return nil
	}
	if err := m.run(ctx, "disable", m.target()); err != nil {
		return err
	}
	if m.Loaded {
		return m.run(ctx, "bootout", m.target())
	}
	return nil
}

func (m *LaunchAgent) Uninstall(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	if err := os.Remove(m.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// WaitRunning verifies a fresh observation, not just launchctl accepting a job.
func WaitRunning(ctx context.Context, home, previousID string) (Snapshot, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		s, err := Read(home)
		if err != nil {
			return s, err
		}
		if s.ID != previousID && (s.State == "running" || s.State == "degraded") {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, core.Fail("unavailable", "system service did not become ready; inspect service status and runtime/service/launchd.log")
		case <-ticker.C:
		}
	}
}
