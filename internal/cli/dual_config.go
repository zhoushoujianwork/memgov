package cli

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"gopkg.in/yaml.v3"
)

// Pointer scalars and nil slices preserve omitted values in the parsed YAML.
// Normalization returns an independent copy; it never changes live configuration.
type DataSourceConfig struct {
	Channel             string              `yaml:"channel" json:"channel"`
	Enabled             *bool               `yaml:"enabled" json:"enabled"`
	Archive             string              `yaml:"archive" json:"archive"`
	Groups              SourceGroupsConfig  `yaml:"groups" json:"groups"`
	ReconcileSeconds    *int                `yaml:"reconcile_seconds" json:"reconcile_seconds"`
	HistoryImport       HistoryImportConfig `yaml:"history_import" json:"history_import"`
	Direct              DirectSourceConfig  `yaml:"direct" json:"direct"`
	Retention           RetentionConfig     `yaml:"retention" json:"retention"`
	BackfillAfterEnable *bool               `yaml:"backfill_after_enable" json:"backfill_after_enable"`
}
type DirectSourceConfig struct {
	Enabled *bool `yaml:"enabled" json:"enabled"`
}
type RetentionConfig struct {
	Days *int `yaml:"days" json:"days"`
}
type SourceGroupsConfig struct {
	ActiveDays  *int     `yaml:"active_days" json:"active_days"`
	MemberRobot string   `yaml:"member_robot" json:"member_robot"`
	Ignore      []string `yaml:"ignore" json:"ignore"`
}
type HistoryImportConfig struct {
	Enabled *bool `yaml:"enabled" json:"enabled"`
	Days    *int  `yaml:"days" json:"days"`
}
type AgentDeclaration struct {
	Home            string                  `yaml:"home" json:"home"`
	Preset          string                  `yaml:"preset" json:"preset"`
	ClaudeProfile   string                  `yaml:"claude_profile" json:"claude_profile"`
	ExecutionModel  string                  `yaml:"execution_model" json:"execution_model"`
	MemoryScope     string                  `yaml:"memory_scope" json:"memory_scope"`
	Capabilities    []string                `yaml:"capabilities" json:"capabilities"`
	Directories     []string                `yaml:"directories" json:"directories"`
	Skills          core.RuntimeSkillPolicy `yaml:"skills" json:"skills"`
	ExternalActions string                  `yaml:"external_actions" json:"external_actions"`
	Bash            bool                    `yaml:"bash" json:"bash"`
}

// yaml.v3 accepts legacy boolean spellings such as "yes" when decoding into a
// bool. Bash is a permission grant, so accept only the explicit YAML literals.
func (a *AgentDeclaration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return dualInvalid("agents", "must be a mapping")
	}
	allowed := map[string]bool{"preset": true, "claude_profile": true, "execution_model": true,
		"memory_scope": true, "capabilities": true, "directories": true, "skills": true,
		"external_actions": true, "bash": true, "home": true}
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		if !allowed[key] || seen[key] {
			return dualInvalid("agents", "unknown or duplicate Agent field")
		}
		seen[key] = true
		if key == "bash" && (value.Kind != yaml.ScalarNode || value.Tag != "!!bool" || (value.Value != "true" && value.Value != "false")) {
			return dualInvalid("agents.bash", "must be literal true or false")
		}
	}
	type plain AgentDeclaration
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return dualInvalid("agents", "invalid Agent field type")
	}
	*a = AgentDeclaration(decoded)
	return nil
}

type ApplicationDeclarations struct {
	Proactive    *ProactiveApplication     `yaml:"proactive" json:"proactive"`
	GroupMention *GroupMentionApplication  `yaml:"group_mention" json:"group_mention"`
	OwnerPrivate *OwnerPrivateApplication  `yaml:"owner_private" json:"owner_private"`
	Bots         map[string]BotApplication `yaml:"bots" json:"bots,omitempty"`
}
type BotApplication struct {
	DefaultAgent string                   `yaml:"default_agent" json:"default_agent"`
	Owner        ApplicationOwner         `yaml:"owner" json:"owner"`
	OwnerPrivate *OwnerPrivateApplication `yaml:"owner_private" json:"owner_private,omitempty"`
	GroupMention *GroupMentionApplication `yaml:"group_mention" json:"group_mention,omitempty"`
}

type OwnerPrivateApplication struct {
	Enabled *bool  `yaml:"enabled" json:"enabled"`
	Runtime string `yaml:"runtime" json:"runtime"`
	Agent   string `yaml:"agent" json:"agent"`
}
type ApplicationOwner struct {
	IDType  string `yaml:"id_type" json:"id_type"`
	IDValue string `yaml:"id_value" json:"id_value"`
}
type ApplicationBatch struct {
	Items          *int `yaml:"items" json:"items"`
	MaxWaitSeconds *int `yaml:"max_wait_seconds" json:"max_wait_seconds"`
}
type ProactiveApplication struct {
	core.Scheduling `yaml:",inline"`
	Concurrency     int              `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	Enabled         *bool            `yaml:"enabled" json:"enabled"`
	Source          string           `yaml:"source" json:"source"`
	Owner           ApplicationOwner `yaml:"owner" json:"owner"`
	Agent           string           `yaml:"agent" json:"agent"`
	Focus           string           `yaml:"focus" json:"focus"`
	Batch           ApplicationBatch `yaml:"batch" json:"batch"`
	AnalysisModel   string           `yaml:"analysis_model" json:"analysis_model"`
	Delivery        string           `yaml:"delivery" json:"delivery"`
}
type GroupMentionApplication struct {
	Owner                    ApplicationOwner    `yaml:"-" json:"owner,omitempty"`
	ExcludedMemoryCategories []string            `yaml:"excluded_memory_categories" json:"excluded_memory_categories,omitempty"`
	SharedMemoryWorkspaces   []string            `yaml:"shared_memory_workspaces" json:"shared_memory_workspaces,omitempty"`
	Enabled                  *bool               `yaml:"enabled" json:"enabled"`
	Source                   string              `yaml:"source" json:"source"`
	Channel                  string              `yaml:"channel" json:"channel"`
	Trigger                  string              `yaml:"trigger" json:"trigger"`
	DefaultAgent             string              `yaml:"default_agent" json:"default_agent"`
	Agent                    string              `yaml:"agent" json:"agent,omitempty"`
	ReplyPolicy              string              `yaml:"reply_policy" json:"reply_policy"`
	Bindings                 []GroupAgentBinding `yaml:"bindings" json:"bindings"`
}
type GroupAgentBinding struct {
	ConversationID string `yaml:"conversation_id" json:"conversation_id"`
	Agent          string `yaml:"agent" json:"agent"`
}
type DualModeDeclaration struct {
	DataSources  map[string]DataSourceConfig `json:"data_sources"`
	Agents       map[string]AgentDeclaration `json:"agents"`
	Applications ApplicationDeclarations     `json:"applications"`
}
type ConfigDiagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
}
type ConfigReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type DualModeValidation struct {
	Declaration          DualModeDeclaration `json:"declaration"`
	Diagnostics          []ConfigDiagnostic  `json:"diagnostics"`
	UnresolvedReferences []ConfigReference   `json:"unresolved_references"`
}

var dataSourceDeclarationName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var declarationName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var aliasName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)
var stableConfigID = regexp.MustCompile(`^[A-Za-z0-9._:$+=/-]{1,200}$`)

func defaultBool(v **bool, fallback bool) {
	if *v == nil {
		*v = &fallback
	}
}
func defaultInt(v **int, fallback int) {
	if *v == nil {
		*v = &fallback
	}
}
func defaultString(v *string, fallback string) {
	if *v == "" {
		*v = fallback
	}
}
func dualInvalid(path, problem string) error {
	return core.Fail("invalid_input", "%s: %s", path, problem)
}
func bounded(v *int, min, max int) bool { return v != nil && *v >= min && *v <= max }

// NormalizeDualModeConfig is strictly offline: channel/preset/profile identity,
// filesystem access, tenant checks, and live permissions belong to plan/apply.
func NormalizeDualModeConfig(c Config) (DualModeValidation, error) {
	out := DualModeValidation{Diagnostics: []ConfigDiagnostic{}, UnresolvedReferences: []ConfigReference{}}
	raw, err := json.Marshal(DualModeDeclaration{DataSources: c.DataSources, Agents: c.Agents, Applications: c.Applications})
	if err != nil {
		return out, dualInvalid("configuration", "cannot normalize declaration")
	}
	if err := json.Unmarshal(raw, &out.Declaration); err != nil {
		return out, err
	}
	d := &out.Declaration
	refs := map[string]ConfigReference{}
	ref := func(kind, name string) error {
		if !declarationName.MatchString(name) {
			return dualInvalid(kind, "reference must be a valid configuration name")
		}
		refs[kind+":"+name] = ConfigReference{Kind: kind, Name: name}
		return nil
	}
	for name, s := range d.DataSources {
		if !dataSourceDeclarationName.MatchString(name) {
			return out, dualInvalid("data_sources", "invalid declaration name")
		}
		if err := ref("channel", s.Channel); err != nil {
			return out, err
		}
		if s.Groups.MemberRobot != "" {
			if err := ref("channel", s.Groups.MemberRobot); err != nil {
				return out, dualInvalid("data_sources.groups.member_robot", "must reference an application channel when configured")
			}
		}
		defaultBool(&s.Enabled, true)
		defaultString(&s.Archive, "sqlite")
		defaultInt(&s.Groups.ActiveDays, 30)
		defaultInt(&s.ReconcileSeconds, 300)
		defaultBool(&s.HistoryImport.Enabled, false)
		defaultInt(&s.HistoryImport.Days, 30)
		defaultBool(&s.Direct.Enabled, false)
		defaultInt(&s.Retention.Days, 7)
		defaultBool(&s.BackfillAfterEnable, true)
		if s.Archive != "sqlite" {
			return out, dualInvalid("data_sources.archive", "must be sqlite")
		}
		if !bounded(s.Groups.ActiveDays, 1, 30) || !bounded(s.HistoryImport.Days, 1, 30) || !bounded(s.Retention.Days, 1, 30) || !bounded(s.ReconcileSeconds, 10, 86400) {
			return out, dualInvalid("data_sources", "active/history days must be 1..30 and reconciliation 10..86400 seconds")
		}
		if *s.Direct.Enabled && *s.HistoryImport.Enabled {
			return out, dualInvalid("data_sources.history_import", "must be disabled when direct incremental collection is enabled")
		}
		seen := map[string]bool{}
		for _, id := range s.Groups.Ignore {
			if strings.TrimSpace(id) == "" || len(id) > 200 || strings.ContainsAny(id, "\r\n\x00") || seen[id] {
				return out, dualInvalid("data_sources.groups.ignore", "entries must be unique nonempty group names or IDs")
			}
			seen[id] = true
		}
		d.DataSources[name] = s
	}
	if err := normalizeBotApplications(d, &out.Diagnostics); err != nil {
		return out, err
	}
	if p := d.Applications.Proactive; p != nil && p.Enabled != nil && *p.Enabled && p.Agent == "" {
		p.Agent = "proactive-owner"
		if d.Agents == nil {
			d.Agents = map[string]AgentDeclaration{}
		}
		if _, exists := d.Agents[p.Agent]; exists {
			return out, dualInvalid("applications.proactive.agent", "reserved default Agent name conflicts; select an explicit agent")
		}
		d.Agents[p.Agent] = fullOwnerAgent("owner_delegated", AgentDeclaration{})
	}
	for name, a := range d.Agents {
		if !declarationName.MatchString(name) {
			return out, dualInvalid("agents", "invalid declaration name")
		}
		if a.Home != "" && (!filepath.IsAbs(a.Home) || strings.ContainsAny(a.Home, "\r\n\x00")) {
			return out, dualInvalid("agents.home", "must be an absolute path after configuration loading")
		}
		if a.Home != "" {
			a.Home = filepath.Clean(a.Home)
		}
		defaultString(&a.Preset, "claude-default")
		defaultString(&a.MemoryScope, "conversation_published")
		defaultString(&a.ExternalActions, "owner_confirmation")
		if err := ref("preset", a.Preset); err != nil {
			return out, err
		}
		if a.ClaudeProfile != "" {
			if !aliasName.MatchString(a.ClaudeProfile) {
				return out, dualInvalid("agents.claude_profile", "must be a shell alias name")
			}
			if err := ref("claude_profile", a.ClaudeProfile); err != nil {
				return out, err
			}
		}
		if a.ExecutionModel == "" && a.ClaudeProfile != "" {
			a.ExecutionModel = "profile"
		}
		if a.ExecutionModel != "" && !modelName.MatchString(a.ExecutionModel) {
			return out, dualInvalid("agents.execution_model", "invalid model identifier")
		}
		if a.ExecutionModel == "profile" && a.ClaudeProfile == "" {
			return out, dualInvalid("agents.execution_model", "profile requires claude_profile")
		}
		if a.MemoryScope != "owner_authorized" && a.MemoryScope != "conversation_published" {
			return out, dualInvalid("agents.memory_scope", "must be owner_authorized or conversation_published")
		}
		if a.ExternalActions != "owner_confirmation" && a.ExternalActions != "owner_request" && a.ExternalActions != "owner_delegated" {
			return out, dualInvalid("agents.external_actions", "must be owner_confirmation, owner_request or owner_delegated")
		}
		if a.Capabilities == nil {
			a.Capabilities = []string{"conversation_history_read", "memory_read"}
		}
		seen := map[string]bool{}
		for _, cap := range a.Capabilities {
			switch cap {
			case "conversation_history_read", "memory_read", "artifact_create", "local_read", "local_write", "local_test":
			default:
				return out, dualInvalid("agents.capabilities", "unsupported capability")
			}
			if seen[cap] {
				return out, dualInvalid("agents.capabilities", "duplicate capability")
			}
			seen[cap] = true
		}
		seen = map[string]bool{}
		for i, path := range a.Directories {
			if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
				return out, dualInvalid("agents.directories", "must contain absolute paths")
			}
			path = filepath.Clean(path)
			if seen[path] {
				return out, dualInvalid("agents.directories", "duplicate directory")
			}
			seen[path] = true
			a.Directories[i] = path
		}
		if a.Skills.Inherit != "" && a.Skills.Inherit != "executor" && a.Skills.Inherit != "none" {
			return out, dualInvalid("agents.skills.inherit", "must be executor or none")
		}
		seen = map[string]bool{}
		for _, path := range a.Skills.Paths {
			if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || seen[path] {
				return out, dualInvalid("agents.skills.paths", "must contain unique absolute paths")
			}
			seen[path] = true
		}
		d.Agents[name] = a
	}
	sourceRef := func(name string, required bool) error {
		if name == "" && !required {
			return nil
		}
		s, ok := d.DataSources[name]
		if !ok {
			return dualInvalid("applications.source", "source must be declared in data_sources")
		}
		if required && !*s.Enabled {
			return dualInvalid("applications.source", "enabled application requires an enabled data source")
		}
		return nil
	}
	agentRef := func(name string, required, group bool) error {
		if name == "" && !required {
			return nil
		}
		a, ok := d.Agents[name]
		if !ok {
			return dualInvalid("applications.agent", "agent must be declared in agents")
		}
		if group && a.MemoryScope != "conversation_published" {
			return dualInvalid("applications.group_mention", "group agents require conversation_published memory")
		}
		return nil
	}
	if d.Applications.Proactive == nil {
		d.Applications.Proactive = &ProactiveApplication{}
	}
	p := d.Applications.Proactive
	defaultBool(&p.Enabled, false)
	defaultString(&p.Focus, "owner_relevant_work")
	if p.Focus == "owner_related_quick_tasks" {
		p.Focus = "owner_relevant_work"
		out.Diagnostics = append(out.Diagnostics, ConfigDiagnostic{Code: "proactive_focus_migrated", Path: "applications.proactive.focus", Summary: "Legacy quick-task focus now evaluates relevant work and may launch investigation before every input is known."})
	}
	defaultString(&p.AnalysisModel, "haiku")
	defaultString(&p.Delivery, "record_only")
	if p.Delivery == "owner_direct" {
		p.Delivery = "record_only"
		out.Diagnostics = append(out.Diagnostics, ConfigDiagnostic{Code: "proactive_delivery_migrated", Path: "applications.proactive.delivery", Summary: "Legacy owner_direct delivery is normalized to record_only; background results are recorded without automatic notification."})
	}
	defaultInt(&p.Batch.Items, 20)
	defaultInt(&p.Batch.MaxWaitSeconds, 30)
	// An old declaration with neither spelling retains its serial execution
	// policy. Cyber opts into four explicitly; migrations do not amplify load.
	if p.Concurrency == 0 && p.ExecutionConcurrency == 0 {
		p.Concurrency = 1
	}
	if err := p.Scheduling.Normalize(p.Concurrency); err != nil {
		return out, err
	}
	if p.Focus != "owner_relevant_work" || p.Delivery != "record_only" || !modelName.MatchString(p.AnalysisModel) || !bounded(p.Batch.Items, 1, 100) || !bounded(p.Batch.MaxWaitSeconds, 1, 86400) {
		return out, dualInvalid("applications.proactive", "invalid focus, delivery, model or batch limits")
	}
	if err := sourceRef(p.Source, *p.Enabled); err != nil {
		return out, err
	}
	if err := agentRef(p.Agent, *p.Enabled, false); err != nil {
		return out, err
	}
	if p.Agent != "" && d.Agents[p.Agent].ExternalActions == "owner_request" {
		return out, dualInvalid("applications.proactive.agent", "owner_request is only allowed for owner_private")
	}
	if *p.Enabled || p.Owner.IDType != "" || p.Owner.IDValue != "" {
		if (p.Owner.IDType != "user_id" && p.Owner.IDType != "staff_id") || !stableConfigID.MatchString(p.Owner.IDValue) {
			return out, dualInvalid("applications.proactive.owner", "requires a stable user_id or staff_id")
		}
	}
	if d.Applications.GroupMention == nil {
		d.Applications.GroupMention = &GroupMentionApplication{}
	}
	g := d.Applications.GroupMention
	if g.Agent != "" {
		if g.DefaultAgent != "" && g.DefaultAgent != g.Agent {
			return out, dualInvalid("applications.group_mention.agent", "conflicts with default_agent")
		}
		g.DefaultAgent, g.Agent = g.Agent, ""
	}
	if len(g.ExcludedMemoryCategories) > 1 || (len(g.ExcludedMemoryCategories) == 1 && g.ExcludedMemoryCategories[0] != "preference") {
		return out, dualInvalid("applications.group_mention.excluded_memory_categories", "仅支持 [preference] 或 []")
	}
	if len(g.SharedMemoryWorkspaces) > 1 || (len(g.SharedMemoryWorkspaces) == 1 && g.SharedMemoryWorkspaces[0] != "global") {
		return out, dualInvalid("applications.group_mention.shared_memory_workspaces", "currently supports only [global] or []")
	}
	defaultBool(&g.Enabled, false)
	defaultString(&g.Trigger, "mention")
	defaultString(&g.ReplyPolicy, "reply_to_trigger")
	if g.Trigger != "mention" || g.ReplyPolicy != "reply_to_trigger" {
		return out, dualInvalid("applications.group_mention", "requires mention and reply_to_trigger")
	}
	if err := sourceRef(g.Source, false); err != nil {
		return out, err
	}
	if err := agentRef(g.DefaultAgent, *g.Enabled, true); err != nil {
		return out, err
	}
	if g.DefaultAgent != "" && d.Agents[g.DefaultAgent].ExternalActions != "owner_confirmation" {
		return out, dualInvalid("applications.group_mention.default_agent", "group agents require owner_confirmation")
	}
	if *g.Enabled || g.Channel != "" {
		if err := ref("channel", g.Channel); err != nil {
			return out, err
		}
	}
	ignored := map[string]bool{}
	for _, id := range d.DataSources[g.Source].Groups.Ignore {
		ignored[id] = true
	}
	seen := map[string]bool{}
	for _, b := range g.Bindings {
		if !stableConfigID.MatchString(b.ConversationID) || seen[b.ConversationID] {
			return out, dualInvalid("applications.group_mention.bindings", "requires unique stable conversation IDs")
		}
		seen[b.ConversationID] = true
		if err := agentRef(b.Agent, true, true); err != nil {
			return out, err
		}
		if d.Agents[b.Agent].ExternalActions != "owner_confirmation" {
			return out, dualInvalid("applications.group_mention.bindings", "owner_request is only allowed for owner_private")
		}
		if ignored[b.ConversationID] {
			out.Diagnostics = append(out.Diagnostics, ConfigDiagnostic{Code: "binding_ignored", Path: "applications.group_mention.bindings", Summary: "Explicit binding is inactive because the group is ignored."})
		}
	}
	if d.Applications.OwnerPrivate == nil {
		d.Applications.OwnerPrivate = &OwnerPrivateApplication{}
	}
	o := d.Applications.OwnerPrivate
	defaultBool(&o.Enabled, false)
	if o.Runtime != "" && !dataSourceDeclarationName.MatchString(o.Runtime) {
		return out, dualInvalid("applications.owner_private.runtime", "must name an existing direct runtime")
	}
	if *o.Enabled && o.Runtime == "" {
		o.Runtime = "owner-private" // Compatibility binding; users start only the unified service.
	}
	if err := agentRef(o.Agent, false, false); err != nil {
		return out, err
	}
	if o.Agent != "" && d.Agents[o.Agent].ExternalActions == "owner_delegated" {
		return out, dualInvalid("applications.owner_private.agent", "owner_delegated is only allowed for proactive")
	}
	if o.Agent != "" && d.Agents[o.Agent].MemoryScope != "owner_authorized" {
		return out, dualInvalid("applications.owner_private.agent", "owner_private requires owner_authorized memory")
	}
	if err := validateBotApplications(d, ref, agentRef, sourceRef, &out.Diagnostics); err != nil {
		return out, err
	}
	for name, a := range d.Agents {
		if a.Skills.Inherit == "" {
			a.Skills.Inherit = "none"
			if ownerAgentBound(d.Applications, name) {
				a.Skills.Inherit = "executor"
			}
		}
		if a.Skills.Paths == nil {
			a.Skills.Paths = []string{}
		}
		if a.Skills.Resolved == nil {
			a.Skills.Resolved = []core.RuntimeSkill{}
		}
		d.Agents[name] = a
	}
	for name, a := range d.Agents {
		if a.ExternalActions == "owner_request" && !ownerAgentBound(d.Applications, name) {
			return out, dualInvalid("agents.external_actions", "owner_request requires an owner_private binding")
		}
	}
	for name, a := range d.Agents {
		if a.ExternalActions == "owner_delegated" && p.Agent != name {
			return out, dualInvalid("agents.external_actions", "owner_delegated requires a proactive binding")
		}
	}
	for _, r := range refs {
		out.UnresolvedReferences = append(out.UnresolvedReferences, r)
	}
	sort.Slice(out.UnresolvedReferences, func(i, j int) bool {
		a, b := out.UnresolvedReferences[i], out.UnresolvedReferences[j]
		return a.Kind+":"+a.Name < b.Kind+":"+b.Name
	})
	return out, nil
}
