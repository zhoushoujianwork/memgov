// Package cli is the non-interactive, versioned command interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
	runtimeengine "github.com/zhoushoujianwork/memgov/internal/runtime"
)

var Version = "2.0.0-dev"

type Envelope struct {
	SchemaVersion int         `json:"schema_version"`
	RequestID     string      `json:"request_id"`
	OK            bool        `json:"ok"`
	Data          any         `json:"data,omitempty"`
	Error         *core.Error `json:"error,omitempty"`
	Cached        bool        `json:"cached,omitempty"`
}
type app struct {
	home, workspace, format, input, key, actor, configPath string
	// legacyConfigPath is populated only when the implicit canonical config is
	// missing and the pre-Owner-Assistant dual config is still present.  We do
	// not silently load it: doing so would make two files possible sources of
	// runtime authority.  The explicit migration command is the only path that
	// copies it into the canonical location.
	legacyConfigPath string
	human            bool
	timeout          time.Duration
	expected         int
	requestID        string
	in               io.Reader
	out, errOut      io.Writer
	cfg              Config
	root             *cobra.Command
	prepared         any
	payloadRead      bool
	cachedPayload    json.RawMessage
	resolved         string
	streamMode       bool
	streamRuntimeID  string
	// Adapters are injectable so the command surface can be exercised offline
	// against a fake platform instead of a real account.
	dwsAdapter, appAdapter channel.Adapter
	adapterMu              sync.Mutex
	runtimeWake            *runtimeengine.WakeBus
}

func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	return run(ctx, &app{in: in, out: out, errOut: errOut, requestID: core.NewID()}, args)
}

func run(ctx context.Context, a *app, args []string) int {
	root := a.command()
	root.SetArgs(args)
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		code := core.ErrorCode(err)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			code = "unavailable"
		}
		if code == "internal" && (strings.HasPrefix(err.Error(), "unknown ") || strings.Contains(err.Error(), "arg(s)") || strings.Contains(err.Error(), "flag")) {
			code = "invalid_input"
		}
		if a.streamMode {
			_ = json.NewEncoder(a.out).Encode(map[string]any{"schema_version": 1, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "level": "error", "component": "cli", "event": "stream_failed", "runtime_id": a.streamRuntimeID, "trace_id": a.streamRuntimeID, "status": "failed", "error_code": code, "summary": "运行时命令失败"})
		} else {
			_ = json.NewEncoder(a.out).Encode(Envelope{SchemaVersion: 1, RequestID: a.requestID, OK: false, Error: &core.Error{Code: code, Message: err.Error()}})
		}
		return exitCode(code)
	}
	return 0
}
func exitCode(code string) int {
	switch code {
	case "invalid_input":
		return 2
	case "conflict":
		return 3
	case "not_found":
		return 4
	case "unavailable":
		return 5
	case "denied":
		return 6
	default:
		return 1
	}
}
func (a *app) command() *cobra.Command {
	root := &cobra.Command{Use: "memgov", Short: "本机记忆治理：SQLite + CLI + 外部 Agent", SilenceUsage: true, SilenceErrors: true}
	a.root = root
	root.SetIn(a.in)
	root.SetOut(a.out)
	root.SetErr(a.errOut)
	f := root.PersistentFlags()
	f.StringVar(&a.home, "home", "", "数据根（MEMGOV_HOME 或 ~/.memgov）")
	f.StringVar(&a.workspace, "workspace", "", "工作区名称或 ID，global 表示全局")
	f.StringVar(&a.format, "format", "json", "输出 json、text；导出另支持 markdown")
	f.BoolVar(&a.human, "human", false, "以适合终端阅读的结构化格式输出")
	f.StringVar(&a.input, "input", "", "JSON 文件，- 表示 stdin")
	f.StringVar(&a.key, "idempotency-key", "", "写操作幂等键")
	f.StringVar(&a.actor, "actor", "cli", "调用方标识（用于审计）")
	f.StringVar(&a.configPath, "config", "", "配置文件路径")
	f.DurationVar(&a.timeout, "timeout", 120*time.Second, "执行期限")
	f.IntVar(&a.expected, "expected-version", 0, "期望的当前版本")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { return a.configure() }
	root.AddCommand(a.simple("version", "版本", cobra.NoArgs, func(context.Context, []string) (any, error) {
		return map[string]any{"version": Version, "schema_version": core.SchemaVersion}, nil
	}))
	root.AddCommand(a.simple("init", "初始化新版权威数据库", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		s, err := core.Open(ctx, a.dbPath(), true)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return s.Doctor(ctx)
	}))
	a.configCommands()
	root.AddCommand(a.read("doctor", "只读检查业务数据与完整性", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) { return s.Doctor(ctx) }))
	workspace := &cobra.Command{Use: "workspace", Short: "工作区"}
	workspace.AddCommand(a.read("list", "列出工作区", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) { return s.Workspaces(ctx) }))
	var path string
	add := a.write("add <name>", "注册工作区", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, raw json.RawMessage) (any, error) {
		return tx.AddWorkspace(ctx, args[0], path)
	})
	add.Flags().StringVar(&path, "path", "", "绑定项目目录")
	workspace.AddCommand(add)
	root.AddCommand(workspace)
	completion := &cobra.Command{Use: "completion <bash|zsh|fish|powershell>", Short: "生成 Shell 补全", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return root.GenBashCompletion(a.out)
		case "zsh":
			return root.GenZshCompletion(a.out)
		case "fish":
			return root.GenFishCompletion(a.out, true)
		case "powershell":
			return root.GenPowerShellCompletion(a.out)
		default:
			return core.Fail("invalid_input", "unsupported shell %q", args[0])
		}
	}}
	root.AddCommand(completion)
	a.memoryCommands()
	a.backupCommands()
	a.agentCommands()
	a.channelCommands()
	a.messageCommands()
	a.audienceCommands()
	a.outboxCommands()
	a.runtimeCommands()
	a.dataSourceCommands()
	root.AddCommand(a.uiCommand(), a.serviceCommand())
	return root
}
func (a *app) configure() error {
	if a.home == "" {
		a.home = os.Getenv("MEMGOV_HOME")
	}
	if a.home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		a.home = filepath.Join(home, ".memgov")
	}
	if a.home == "~" || strings.HasPrefix(a.home, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		a.home = filepath.Join(home, strings.TrimPrefix(a.home, "~/"))
	}
	var err error
	a.home, err = filepath.Abs(a.home)
	if err != nil {
		return err
	}
	explicit := a.configPath != ""
	a.legacyConfigPath = ""
	if a.configPath == "" {
		a.configPath = os.Getenv("MEMGOV_CONFIG")
		explicit = a.configPath != ""
	}
	if a.configPath == "" {
		a.configPath = canonicalConfigPath(a.home)
	}
	if raw, err := os.ReadFile(a.configPath); err == nil {
		if err = a.loadConfig(raw); err != nil {
			return err
		}
	} else if explicit || !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		// A legacy dual-mode file must never be selected implicitly.  Keep its
		// path available for an explicit, validated migration and for actionable
		// diagnostics in config show / service startup.
		legacy := legacyConfigPath(a.home)
		if _, legacyErr := os.Stat(legacy); legacyErr == nil {
			a.legacyConfigPath = legacy
		} else if !errors.Is(legacyErr, os.ErrNotExist) {
			return legacyErr
		}
	}
	if err := a.applyConfigDefaults(); err != nil {
		return err
	}
	if a.workspace == "" {
		a.workspace = os.Getenv("MEMGOV_WORKSPACE")
	}
	if a.key == "" {
		a.key = os.Getenv("MEMGOV_IDEMPOTENCY_KEY")
	}
	if a.timeout <= 0 {
		return core.Fail("invalid_input", "timeout must be positive")
	}
	if a.human {
		a.format = "text"
	}
	if a.format != "json" && a.format != "text" && a.format != "markdown" && a.format != "mermaid" {
		return core.Fail("invalid_input", "unsupported format %q", a.format)
	}
	return nil
}

// canonicalConfigPath is the one implicit runtime configuration location.
// Callers may still pass --config (or MEMGOV_CONFIG) for isolated development
// and tests, but a managed service never invents or probes another default.
func canonicalConfigPath(home string) string { return filepath.Join(home, "config.yaml") }

func legacyConfigPath(home string) string { return filepath.Join(home, "config.dual.yaml") }

func (a *app) requireCanonicalConfig(operation string) error {
	if a.legacyConfigPath == "" {
		return nil
	}
	return core.Fail("conflict", "%s requires the canonical config at %s; legacy config %s is present but is not loaded; run `memgov config migrate-legacy` first", operation, a.configPath, a.legacyConfigPath)
}
func (a *app) dbPath() string { return filepath.Join(a.home, "state.db") }
func (a *app) scope(ctx context.Context, s *core.Store) (string, error) {
	if a.workspace != "" {
		w, err := s.Workspace(ctx, a.workspace)
		return w.ID, err
	}
	cwd, _ := os.Getwd()
	ws, err := s.Workspaces(ctx)
	if err != nil {
		return "", err
	}
	best := core.Workspace{}
	for _, w := range ws {
		if w.Path != "" && (cwd == w.Path || strings.HasPrefix(cwd, w.Path+string(os.PathSeparator))) && len(w.Path) > len(best.Path) {
			best = w
		}
	}
	if best.ID != "" {
		return best.ID, nil
	}
	w, err := s.Workspace(ctx, a.cfg.DefaultWorkspace)
	return w.ID, err
}
func (a *app) emit(data any, cached bool) error {
	if a.format != "json" {
		if value, ok := data.(string); ok {
			_, err := fmt.Fprintln(a.out, value)
			return err
		}
		raw, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.out, string(raw))
		return err
	}
	return json.NewEncoder(a.out).Encode(Envelope{SchemaVersion: 1, RequestID: a.requestID, OK: true, Data: data, Cached: cached})
}
func (a *app) simple(use, short string, args cobra.PositionalArgs, fn func(context.Context, []string) (any, error)) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), a.timeout)
		defer cancel()
		data, err := fn(ctx, args)
		if err != nil {
			return err
		}
		return a.emit(data, false)
	}}
}
func (a *app) read(use, short string, args cobra.PositionalArgs, fn func(context.Context, *core.Store, []string) (any, error)) *cobra.Command {
	return a.simple(use, short, args, func(ctx context.Context, args []string) (any, error) {
		s, err := core.Open(ctx, a.dbPath(), false)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return fn(ctx, s, args)
	})
}
func (a *app) write(use, short string, args cobra.PositionalArgs, fn func(context.Context, *core.Tx, []string, json.RawMessage) (any, error)) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), a.timeout)
		defer cancel()
		raw, err := a.payload()
		if err != nil {
			return err
		}
		s, err := core.Open(ctx, a.dbPath(), false)
		if err != nil {
			return err
		}
		defer s.Close()
		scope, err := a.scope(ctx, s)
		if err != nil {
			return err
		}
		flags := map[string]string{}
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Name != "idempotency-key" && f.Name != "input" && f.Name != "format" {
				flags[f.Name] = f.Value.String()
			}
		})
		req := core.Request{ID: a.requestID, Command: cmd.CommandPath(), Scope: scope, Actor: a.actor, Key: a.key, Input: map[string]any{"args": args, "flags": flags, "payload": raw, "prepared": a.prepared}}
		result, err := s.Mutate(ctx, req, func(tx *core.Tx) (any, error) { return fn(ctx, tx, args, raw) })
		if err != nil {
			return err
		}
		return a.emit(result.Data, result.Cached)
	}}
}
func (a *app) payload() (json.RawMessage, error) {
	if a.payloadRead {
		return a.cachedPayload, nil
	}
	if a.input == "" {
		return json.RawMessage("{}"), nil
	}
	var r io.Reader = a.in
	var f *os.File
	if a.input != "-" {
		var err error
		f, err = os.Open(a.input)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, 32*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 32*1024*1024 {
		return nil, core.Fail("invalid_input", "input exceeds 32 MiB")
	}
	if !json.Valid(b) {
		return nil, core.Fail("invalid_input", "input must be a single valid JSON value")
	}
	a.payloadRead = true
	a.cachedPayload = json.RawMessage(b)
	return a.cachedPayload, nil
}
func decode(raw []byte, v any) error {
	if !utf8.Valid(raw) {
		return core.Fail("invalid_input", "JSON input must be valid UTF-8")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return core.Fail("invalid_input", "invalid input: %v", err)
	}
	return nil
}
