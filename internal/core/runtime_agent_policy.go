package core

import (
	"context"
	"encoding/json"
)

// RuntimeAgentPolicy is resolved from the currently applied declaration for the
// task's exact group. Unmanaged runtimes retain their explicit runtime settings.
type RuntimeAgentPolicy struct {
	ExcludedMemoryCategories []string           `json:"excluded_memory_categories,omitempty"`
	SharedMemoryWorkspaces   []string           `json:"shared_memory_workspaces,omitempty"`
	Agent                    string             `json:"agent"`
	Preset                   string             `json:"preset"`
	ClaudeProfile            string             `json:"claude_profile"`
	ExecutionModel           string             `json:"execution_model"`
	MemoryScope              string             `json:"memory_scope"`
	Capabilities             []string           `json:"capabilities"`
	Directories              []string           `json:"directories"`
	Skills                   RuntimeSkillPolicy `json:"skills"`
	BashEnabled              bool               `json:"bash"`
	ExternalActions          string             `json:"external_actions"`
	Managed                  bool               `json:"managed"`
}

type RuntimeSkill struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Digest  string `json:"digest"`
	Summary string `json:"summary,omitempty"`
}

type RuntimeSkillPolicy struct {
	Inherit  string         `json:"inherit" yaml:"inherit"`
	Paths    []string       `json:"paths" yaml:"paths"`
	Resolved []RuntimeSkill `json:"resolved,omitempty" yaml:"-"`
}

func ResolveRuntimeTaskAgent(ctx context.Context, q Queryer, c RuntimeConfig, t RuntimeTask) (RuntimeAgentPolicy, error) {
	out := RuntimeAgentPolicy{Preset: c.AgentPreset, ClaudeProfile: c.ClaudeProfile, ExecutionModel: c.ExecutionModel, MemoryScope: c.MemoryScope, Capabilities: append([]string{}, c.AgentCapabilities...), Skills: RuntimeSkillPolicy{Inherit: "none", Paths: []string{}, Resolved: []RuntimeSkill{}}, BashEnabled: c.AgentBash, ExternalActions: c.ExternalActions}
	if out.ExternalActions == "" {
		out.ExternalActions = "owner_confirmation"
	}
	current, err := ReadRuntime(ctx, q, c.ID)
	if err != nil {
		return out, err
	}
	if (current.Version != c.Version && (c.ApplicationMode != "proactive" || runtimePolicyDigest(current) != runtimePolicyDigest(c))) || t.RuntimeID != c.ID {
		return out, Fail("conflict", "runtime policy changed before task execution")
	}
	route, err := ReadRoute(ctx, q, t.RouteID)
	if err != nil {
		return out, err
	}
	if route.ChannelID != c.ChannelID || route.Status != "active" || route.Mode == "ignore" {
		return out, Fail("denied", "task route is no longer authorized")
	}
	if c.ApplicationMode == "direct" {
		if _, err := RuntimeOwnerDirectProcessingRoute(ctx, q, c, t.RouteID); err != nil {
			return out, err
		}
	}
	if !runtimeExternalActionsAllowed(c.ApplicationMode, out.ExternalActions) {
		return out, Fail("denied", "external action policy does not match the execution origin")
	}
	applied, err := ReadAppliedConfig(ctx, q, 0)
	if err != nil {
		return out, err
	}
	managed := false
	for _, o := range applied.Objects {
		if o.Kind == "application" && o.ObjectID == c.ID && o.ObjectType == "runtime" {
			managed = true
			break
		}
	}
	if !managed {
		return out, nil
	}
	var declaration struct {
		Agents       map[string]RuntimeAgentPolicy `json:"agents"`
		Applications struct {
			OwnerPrivate *runtimeOwnerApplication `json:"owner_private"`
			Proactive    *struct {
				Enabled bool   `json:"enabled"`
				Agent   string `json:"agent"`
			} `json:"proactive"`
			Group *runtimeGroupApplication `json:"group_mention"`
			Bots  map[string]struct {
				DefaultAgent string                   `json:"default_agent"`
				OwnerPrivate *runtimeOwnerApplication `json:"owner_private"`
				Group        *runtimeGroupApplication `json:"group_mention"`
			} `json:"bots"`
		} `json:"applications"`
	}
	if err = json.Unmarshal(applied.Declaration, &declaration); err != nil {
		return out, err
	}
	channel, err := ReadChannel(ctx, q, c.ChannelID)
	if err != nil {
		return out, err
	}
	ownerApplication := declaration.Applications.OwnerPrivate
	groupApplication := declaration.Applications.Group
	if bot, exists := declaration.Applications.Bots[channel.Name]; exists {
		ownerApplication = bot.OwnerPrivate
		groupApplication = bot.Group
		if groupApplication != nil && groupApplication.DefaultAgent == "" {
			groupApplication.DefaultAgent = bot.DefaultAgent
		}
	}
	name := ""
	if c.ApplicationMode == "group_mention" {
		g := groupApplication
		if g == nil || !g.Enabled {
			return out, Fail("denied", "group application is disabled")
		}
		name = g.DefaultAgent
		for _, b := range g.Bindings {
			if b.ConversationID == route.ConversationID {
				name = b.Agent
				break
			}
		}
	} else if c.ApplicationMode == "direct" {
		p := ownerApplication
		if p == nil || !p.Enabled || p.Runtime != c.Name {
			return out, Fail("denied", "owner private application is disabled or references a different runtime")
		}
		name = p.Agent
		if name == "" {
			out.Skills.Inherit = "executor"
			out.Managed = true
			return out, nil
		}
	} else {
		p := declaration.Applications.Proactive
		if p == nil || !p.Enabled {
			return out, Fail("denied", "proactive application is disabled")
		}
		name = p.Agent
	}
	selected, ok := declaration.Agents[name]
	if !ok || selected.Preset == "" {
		return out, Fail("denied", "task Agent declaration is missing")
	}
	if c.ApplicationMode == "group_mention" && selected.MemoryScope != "conversation_published" {
		return out, Fail("denied", "group Agent cannot use owner memory")
	}
	if selected.ExternalActions == "" {
		selected.ExternalActions = "owner_confirmation"
	}
	if !runtimeExternalActionsAllowed(c.ApplicationMode, selected.ExternalActions) {
		return out, Fail("denied", "external action policy does not match the execution origin")
	}
	if selected.BashEnabled && len(selected.Directories) > 0 {
		return out, Fail("denied", "full Bash cannot be combined with bounded directory snapshots")
	}
	if c.ApplicationMode == "group_mention" {
		selected.SharedMemoryWorkspaces = append([]string(nil), groupApplication.SharedMemoryWorkspaces...)
		selected.ExcludedMemoryCategories = append([]string(nil), groupApplication.ExcludedMemoryCategories...)
	}
	selected.Agent, selected.Managed = name, true
	return selected, nil
}

func (tx *Tx) RecordRuntimeAttemptAgent(ctx context.Context, c RuntimeConfig, t RuntimeTask, attemptID string, policy RuntimeAgentPolicy, presetCommit string) error {
	current, err := ResolveRuntimeTaskAgent(ctx, tx.Conn, c, t)
	if err != nil {
		return err
	}
	if Digest(current) != Digest(policy) {
		return Fail("conflict", "task Agent policy changed")
	}
	result, err := tx.Conn.ExecContext(ctx, "UPDATE runtime_attempts SET model=?,preset_name=?,preset_commit=?,input_digest=? WHERE id=? AND task_id=? AND task_version=? AND status='running' AND EXISTS(SELECT 1 FROM runtime_tasks WHERE id=? AND version=? AND status='running')", policy.ExecutionModel, policy.Preset, presetCommit, Digest(map[string]any{"title": t.Title, "instructions": t.Instructions, "version": t.Version, "agent_policy": policy}), attemptID, t.ID, t.Version, t.ID, t.Version)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return Fail("conflict", "task attempt is no longer current")
	}
	return nil
}

type runtimeOwnerApplication struct {
	Enabled bool   `json:"enabled"`
	Runtime string `json:"runtime"`
	Agent   string `json:"agent"`
}
type runtimeGroupApplication struct {
	Enabled                  bool     `json:"enabled"`
	DefaultAgent             string   `json:"default_agent"`
	SharedMemoryWorkspaces   []string `json:"shared_memory_workspaces"`
	ExcludedMemoryCategories []string `json:"excluded_memory_categories"`
	Bindings                 []struct {
		ConversationID string `json:"conversation_id"`
		Agent          string `json:"agent"`
	} `json:"bindings"`
}

func runtimeExternalActionsAllowed(mode, policy string) bool {
	return policy == "owner_confirmation" || mode == "direct" && policy == "owner_request" || mode == "proactive" && policy == "owner_delegated"
}
