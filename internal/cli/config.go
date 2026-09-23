package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
	"gopkg.in/yaml.v3"
)

type Config struct {
	DataSources      map[string]DataSourceConfig `yaml:"data_sources" json:"data_sources"`
	Agents           map[string]AgentDeclaration `yaml:"agents" json:"agents"`
	Applications     ApplicationDeclarations     `yaml:"applications" json:"applications"`
	DefaultWorkspace string                      `yaml:"default_workspace" json:"default_workspace"`
	Format           string                      `yaml:"format" json:"format,omitempty"`
	Timeout          string                      `yaml:"timeout" json:"timeout,omitempty"`
	Actor            string                      `yaml:"actor" json:"actor,omitempty"`
	RuntimeSetup     RuntimeSetupConfig          `yaml:"runtime_setup" json:"runtime_setup"`
	Logging          LoggingConfig               `yaml:"logging" json:"logging"`
	Channels         []core.ChannelInput         `yaml:"channels" json:"channels"`
}

// These are creation defaults, never an overlay on a running SQLite instance.
type RuntimeSetupConfig struct {
	core.Scheduling      `yaml:",inline"`
	Profile              string   `yaml:"profile" json:"profile,omitempty"`
	RobotCode            string   `yaml:"robot_code" json:"robot_code,omitempty"`
	RobotName            string   `yaml:"robot_name" json:"robot_name,omitempty"`
	Ignore               []string `yaml:"ignore" json:"ignore,omitempty"`
	DeliveryConversation string   `yaml:"delivery_conversation" json:"delivery_conversation,omitempty"`
	WorkspacePath        string   `yaml:"workspace_path" json:"workspace_path,omitempty"`
	WorkspaceName        string   `yaml:"workspace_name" json:"workspace_name,omitempty"`
	ChannelName          string   `yaml:"channel_name" json:"channel_name,omitempty"`
	AgentPreset          string   `yaml:"agent_preset" json:"agent_preset,omitempty"`
	AgentHarness         string   `yaml:"agent_harness" json:"agent_harness,omitempty"`
	ClaudeProfile        string   `yaml:"claude_profile" json:"claude_profile,omitempty"`
	AnalysisModel        string   `yaml:"analysis_model" json:"analysis_model,omitempty"`
	ExecutionModel       string   `yaml:"execution_model" json:"execution_model,omitempty"`
	Pilot                bool     `yaml:"pilot" json:"pilot"`
	ItemThreshold        int      `yaml:"item_threshold" json:"item_threshold,omitempty"`
	MaxWaitSeconds       int      `yaml:"max_wait_seconds" json:"max_wait_seconds,omitempty"`
	ReconcileSeconds     int      `yaml:"reconcile_seconds" json:"reconcile_seconds,omitempty"`
	Concurrency          int      `yaml:"concurrency" json:"concurrency,omitempty"`
}

type LoggingConfig struct {
	Retention string `yaml:"retention" json:"retention,omitempty"`
	MaxBytes  int64  `yaml:"max_bytes" json:"max_bytes,omitempty"`
	FileBytes int64  `yaml:"file_bytes" json:"file_bytes,omitempty"`
}

func (c LoggingConfig) options() runlog.Options {
	duration, _ := time.ParseDuration(c.Retention) // validated at load time
	return runlog.Options{Retention: duration, MaxBytes: c.MaxBytes, FileBytes: c.FileBytes}
}

func (a *app) loadConfig(raw []byte) error {
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if err := d.Decode(&a.cfg); err != nil && err != io.EOF {
		// yaml type errors can echo values. Keep credentials out of diagnostics.
		return core.Fail("invalid_input", "invalid configuration: unknown field, duplicate key, invalid type or malformed YAML; for retired memory fields run config migrate-workspaces; see config.local.yaml.example")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return core.Fail("invalid_input", "configuration must contain a single YAML document")
	}
	for name, value := range map[string]string{"timeout": a.cfg.Timeout, "logging.retention": a.cfg.Logging.Retention} {
		if value != "" {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 {
				return core.Fail("invalid_input", "%s must be a positive duration, for example 2m or 720h", name)
			}
		}
	}
	if a.cfg.Format != "" && a.cfg.Format != "json" && a.cfg.Format != "text" {
		return core.Fail("invalid_input", "config format must be json or text")
	}
	if a.cfg.Logging.MaxBytes < 0 || a.cfg.Logging.FileBytes < 0 {
		return core.Fail("invalid_input", "logging byte limits must not be negative")
	}
	base := ""
	if a.configPath != "" {
		base = filepath.Dir(a.configPath)
	}
	if base == "" {
		base, _ = os.Getwd()
	}
	home, _ := os.UserHomeDir()
	for name, agent := range a.cfg.Agents {
		for i, value := range agent.Skills.Paths {
			if value == "~" {
				value = home
			} else if strings.HasPrefix(value, "~/") {
				value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
			}
			if !filepath.IsAbs(value) {
				value = filepath.Join(base, value)
			}
			absolute, err := filepath.Abs(value)
			if err != nil {
				return core.Fail("invalid_input", "agents.skills.paths: cannot resolve path")
			}
			agent.Skills.Paths[i] = filepath.Clean(absolute)
		}
		a.cfg.Agents[name] = agent
	}
	r := a.cfg.RuntimeSetup
	if r.ItemThreshold < 0 || r.ItemThreshold > 100 || r.MaxWaitSeconds < 0 || r.MaxWaitSeconds > 86400 ||
		r.ReconcileSeconds < 0 || r.ReconcileSeconds > 86400 || (r.ReconcileSeconds > 0 && r.ReconcileSeconds < 10) || r.Concurrency < 0 || r.Concurrency > 32 {
		return core.Fail("invalid_input", "runtime_setup: threshold 1..100, max wait 1..86400s, reconciliation 10..86400s, concurrency 1..32")
	}
	if err := r.Scheduling.Normalize(r.Concurrency); err != nil {
		return err
	}
	var tree yaml.Node
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return core.Fail("invalid_input", "invalid YAML")
	}
	if err := validateSchedulingNodes(&tree); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range a.cfg.Channels {
		if seen[c.Name] {
			return core.Fail("invalid_input", "duplicate channel name in configuration")
		}
		seen[c.Name] = true
		if err := core.ValidateChannelInput(c); err != nil {
			return err
		}
	}
	_, err := NormalizeDualModeConfig(a.cfg)
	return err
}

func (a *app) applyConfigDefaults() error {
	f := a.root.PersistentFlags()
	for name, value := range map[string]string{"format": a.cfg.Format, "timeout": a.cfg.Timeout, "actor": a.cfg.Actor} {
		if value != "" && !f.Changed(name) {
			if err := f.Lookup(name).Value.Set(value); err != nil {
				return core.Fail("invalid_input", "invalid config default for %s", name)
			}
		}
	}
	return nil
}

func (a *app) configStatus() string {
	if a.legacyConfigPath != "" {
		return "canonical_missing_legacy_present"
	}
	if a.configPath == canonicalConfigPath(a.home) {
		if _, err := os.Stat(a.configPath); err == nil {
			return "canonical"
		}
		return "canonical_missing"
	}
	return "explicit"
}

// migrateLegacyConfig performs an explicit, validated one-way copy from the
// old dual-mode filename to the sole implicit runtime filename.  The source is
// retained as a recovery copy; an existing destination is never overwritten.
func (a *app) migrateLegacyConfig() (any, error) {
	destination := canonicalConfigPath(a.home)
	if _, err := os.Stat(destination); err == nil {
		return nil, core.Fail("conflict", "canonical config already exists at %s; refusing to overwrite it", destination)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	source := a.legacyConfigPath
	if source == "" {
		source = legacyConfigPath(a.home)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, core.Fail("not_found", "legacy config not found at %s", source)
		}
		return nil, err
	}
	// Parse and normalize before creating the destination.  This prevents a
	// malformed or unsafe legacy file from becoming the new active authority.
	candidate := &app{configPath: source}
	if err := candidate.loadConfig(raw); err != nil {
		return nil, core.Fail("invalid_input", "legacy config cannot be migrated: %s", err)
	}
	if err := os.MkdirAll(a.home, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil, core.Fail("conflict", "canonical config appeared during migration; refusing to overwrite it")
		}
		return nil, err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	ok = true
	a.configPath = destination
	a.legacyConfigPath = source
	return map[string]any{"migrated": true, "source": source, "destination": destination, "source_retained": true}, nil
}

func (c RuntimeSetupConfig) apply(cmd *cobra.Command) error {
	values := map[string]string{
		"profile": c.Profile, "robot-code": c.RobotCode, "robot-name": c.RobotName, "delivery-conversation": c.DeliveryConversation,
		"workspace-path": c.WorkspacePath, "workspace-name": c.WorkspaceName, "channel-name": c.ChannelName,
		"agent-preset": c.AgentPreset, "agent-harness": c.AgentHarness, "claude-profile": c.ClaudeProfile,
		"analysis-model": c.AnalysisModel, "execution-model": c.ExecutionModel,
		"pilot": strconv.FormatBool(c.Pilot), "item-threshold": strconv.Itoa(c.ItemThreshold),
		"max-wait-seconds": strconv.Itoa(c.MaxWaitSeconds), "reconcile-seconds": strconv.Itoa(c.ReconcileSeconds),
		"concurrency":               strconv.Itoa(c.Concurrency),
		"analysis-concurrency":      strconv.Itoa(c.AnalysisConcurrency),
		"execution-concurrency":     strconv.Itoa(c.ExecutionConcurrency),
		"analysis-timeout-seconds":  strconv.Itoa(c.AnalysisTimeoutSeconds),
		"execution-timeout-seconds": strconv.Itoa(c.ExecutionTimeoutSeconds),
	}
	for name, value := range values {
		if value != "" && !cmd.Flags().Changed(name) {
			if err := cmd.Flags().Lookup(name).Value.Set(value); err != nil {
				return core.Fail("invalid_input", "invalid runtime_setup value for %s", name)
			}
		}
	}
	if c.Ignore != nil && !cmd.Flags().Changed("ignore") {
		return cmd.Flags().Lookup("ignore").Value.(interface{ Replace([]string) error }).Replace(c.Ignore)
	}
	return nil
}

func (a *app) configCommands() {
	root := &cobra.Command{Use: "config", Short: "YAML 系统默认值与消息通道配置"}
	root.AddCommand(a.simple("migrate-workspaces", "Archive and remove retired memory settings and AgentHome notes", cobra.NoArgs, func(context.Context, []string) (any, error) {
		return a.migrateWorkspaceConfig()
	}))
	root.AddCommand(a.simple("show", "显示配置和生效的系统默认值（不读取密钥）", cobra.NoArgs, func(context.Context, []string) (any, error) {
		result := map[string]any{"home": a.home, "db_path": a.dbPath(), "config_path": a.configPath, "config_status": a.configStatus(),
			"default_workspace": a.cfg.DefaultWorkspace, "timeout": a.timeout.String(), "format": a.format, "actor": a.actor,
			"runtime_setup": a.cfg.RuntimeSetup, "logging": a.cfg.Logging, "channels": a.cfg.Channels,
			"data_sources": a.cfg.DataSources, "agents": a.cfg.Agents, "applications": a.cfg.Applications}
		if a.legacyConfigPath != "" {
			result["legacy_config_path"] = a.legacyConfigPath
		}
		return result, nil
	}))
	root.AddCommand(a.simple("validate", "离线检查 YAML 字段、系统参数与通道身份；路由在 apply 时校验", cobra.NoArgs, func(context.Context, []string) (any, error) {
		if err := a.requireCanonicalConfig("config validate"); err != nil {
			return nil, err
		}
		v, err := NormalizeDualModeConfig(a.cfg)
		if err != nil {
			return nil, err
		}
		return map[string]any{"valid": true, "config_path": a.configPath, "config_status": a.configStatus(), "channel_count": len(a.cfg.Channels), "route_validation": "on_apply", "declaration": v.Declaration, "diagnostics": v.Diagnostics, "unresolved_references": v.UnresolvedReferences, "application_status": "not_applied"}, nil
	}))
	root.AddCommand(a.simple("migrate-legacy", "验证并复制旧 config.dual.yaml 到唯一的 config.yaml（保留源文件）", cobra.NoArgs, func(context.Context, []string) (any, error) {
		return a.migrateLegacyConfig()
	}))
	root.AddCommand(a.dualPlanCommand())
	root.AddCommand(a.dualApplyCommand())
	var expectedRoute int
	var reason string
	apply := a.write("apply <channel-name>", "将一个 YAML 通道配置写入 SQLite（不连接钉钉）", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.ApplyChannelConfig(ctx, a.prepared.(core.ChannelInput), a.expected, expectedRoute, reason)
	})
	apply.PreRunE = func(_ *cobra.Command, args []string) error {
		if err := a.requireCanonicalConfig("config apply"); err != nil {
			return err
		}
		if a.input != "" {
			return core.Fail("invalid_input", "config apply reads --config; --input is not supported")
		}
		for _, c := range a.cfg.Channels {
			if c.Name == args[0] {
				// Bind file contents, not just its path, into the idempotency digest.
				a.prepared = c
				return nil
			}
		}
		return core.Fail("not_found", "channel %q is not declared in the selected config file", args[0])
	}
	apply.Flags().IntVar(&expectedRoute, "expected-route-version", 0, "更新已有路由时必须指定其当前版本")
	apply.Flags().StringVar(&reason, "reason", "", "更新已有通道的理由")
	root.AddCommand(apply)
	a.root.AddCommand(root)
}
