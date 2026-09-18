package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zhoushoujianwork/memgov/internal/console"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

type agentEdit struct {
	Revision  string           `json:"revision"`
	Name      string           `json:"name"`
	Operation string           `json:"operation"`
	Agent     AgentDeclaration `json:"agent"`
}

type agentConfigEditor struct {
	home, path    string
	mu            sync.Mutex
	resolveSkills func(core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error)
}

func newAgentConfigEditor(home, path string, resolve func(core.RuntimeSkillPolicy) ([]core.RuntimeSkill, error)) *console.AgentConfigAccess {
	e := &agentConfigEditor{home: home, path: path, resolveSkills: resolve}
	return &console.AgentConfigAccess{List: e.list, Preview: e.preview, Save: e.save}
}

func (e *agentConfigEditor) read() ([]byte, *app, string, error) {
	path, err := filepath.EvalSymlinks(e.path)
	if err != nil {
		return nil, nil, "", core.Fail("not_found", "Agent config file unavailable")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, nil, "", core.Fail("invalid_input", "Agent config must be a regular YAML file no larger than 1 MiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", core.Fail("unavailable", "cannot read Agent config")
	}
	a := &app{home: e.home, configPath: e.path}
	if err := a.loadConfig(raw); err != nil {
		return nil, nil, "", err
	}
	return raw, a, path, nil
}

func agentReferences(d DualModeDeclaration, name string) []string {
	refs := []string{}
	if p := d.Applications.Proactive; p != nil && p.Agent == name {
		refs = append(refs, "主动值守")
	}
	if p := d.Applications.OwnerPrivate; p != nil && p.Agent == name {
		refs = append(refs, "本人私聊 · "+p.Runtime)
	}
	if g := d.Applications.GroupMention; g != nil {
		if g.DefaultAgent == name {
			refs = append(refs, "群 Agent 默认")
		}
		for _, b := range g.Bindings {
			if b.Agent == name {
				refs = append(refs, "群绑定 · "+b.ConversationID)
			}
		}
	}
	for channel, bot := range d.Applications.Bots {
		if bot.DefaultAgent == name {
			refs = append(refs, "机器人默认 · "+channel)
		}
		if o := bot.OwnerPrivate; o != nil && o.Agent == name {
			refs = append(refs, "机器人本人私聊 · "+channel)
		}
		if g := bot.GroupMention; g != nil {
			if g.DefaultAgent == name || g.Agent == name {
				refs = append(refs, "机器人群默认 · "+channel)
			}
			for _, binding := range g.Bindings {
				if binding.Agent == name {
					refs = append(refs, "机器人群绑定 · "+channel+" · "+binding.ConversationID)
				}
			}
		}
	}
	sort.Strings(refs)
	return refs
}

func currentAgentDeclaration(ctx context.Context, q core.Queryer) (DualModeDeclaration, error) {
	c, err := core.ReadAppliedConfig(ctx, q, 0)
	var d DualModeDeclaration
	if err == nil && len(c.Declaration) > 0 {
		err = json.Unmarshal(c.Declaration, &d)
	}
	return d, err
}

func agentDigest(a AgentDeclaration) string {
	a.Skills.Resolved = nil
	canonical := canonicalDeclaration(DualModeDeclaration{Agents: map[string]AgentDeclaration{"agent": a}})
	return core.Digest(canonical.Agents["agent"])
}

func (e *agentConfigEditor) list(ctx context.Context, q core.Queryer) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	raw, a, _, err := e.read()
	if err != nil {
		return map[string]any{"revision": "", "items": []any{}, "config_path": e.path, "error": err.Error(), "plan_command": e.command("config", "plan")}, nil
	}
	n, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		return nil, err
	}
	applied, err := currentAgentDeclaration(ctx, q)
	if err != nil {
		return nil, err
	}
	items := []any{}
	for _, name := range sortedNames(n.Declaration.Agents) {
		agent := n.Declaration.Agents[name]
		refs := agentReferences(n.Declaration, name)
		for _, r := range agentReferences(applied, name) {
			refs = append(refs, "已应用："+r)
		}
		before, ok := applied.Agents[name]
		items = append(items, map[string]any{"name": name, "agent": agent, "references": refs, "can_delete": len(refs) == 0, "pending": !ok || agentDigest(before) != agentDigest(agent)})
	}
	return map[string]any{"revision": core.Hash(raw), "items": items, "config_path": e.path, "plan_command": e.command("config", "plan")}, nil
}

func (e *agentConfigEditor) command(parts ...string) string {
	s := "memgov --home " + uiQuote(e.home) + " --config " + uiQuote(e.path)
	for _, part := range parts {
		s += " " + uiQuote(part)
	}
	return s
}
func uiQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func yamlField(n *yaml.Node, key string) (*yaml.Node, int) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1], i
		}
	}
	return nil, -1
}

// Prepare a new document while preserving unrelated declarations and comments.
func (e *agentConfigEditor) prepare(ctx context.Context, q core.Queryer, input json.RawMessage) ([]byte, []byte, string, any, error) {
	var edit agentEdit
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&edit); err != nil {
		return nil, nil, "", nil, core.Fail("invalid_input", "unknown or invalid Agent edit field")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, "", nil, core.Fail("invalid_input", "single Agent edit required")
	}
	if !declarationName.MatchString(edit.Name) || (edit.Operation != "update" && edit.Operation != "delete") || len(edit.Agent.Skills.Resolved) > 0 {
		return nil, nil, "", nil, core.Fail("invalid_input", "invalid Agent name, operation or runtime-managed skills")
	}
	raw, a, path, err := e.read()
	if err != nil {
		return nil, nil, "", nil, err
	}
	if edit.Revision == "" || edit.Revision != core.Hash(raw) {
		return nil, nil, "", nil, core.Fail("conflict", "配置已变化，请重新打开编辑器")
	}
	if _, ok := a.cfg.Agents[edit.Name]; !ok {
		return nil, nil, "", nil, core.Fail("not_found", "Agent declaration not found")
	}
	before, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		return nil, nil, "", nil, err
	}
	applied, err := currentAgentDeclaration(ctx, q)
	if err != nil {
		return nil, nil, "", nil, err
	}
	refs := agentReferences(before.Declaration, edit.Name)
	for _, r := range agentReferences(applied, edit.Name) {
		refs = append(refs, "已应用："+r)
	}
	if edit.Operation == "delete" && len(refs) > 0 {
		return nil, nil, "", nil, core.Fail("conflict", "Agent 仍被引用；先解除 YAML 和已应用配置中的绑定")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) != 1 {
		return nil, nil, "", nil, core.Fail("invalid_input", "invalid YAML document")
	}
	agents, _ := yamlField(doc.Content[0], "agents")
	if agents == nil || agents.Kind != yaml.MappingNode {
		return nil, nil, "", nil, core.Fail("invalid_input", "agents must be an explicit YAML mapping")
	}
	old, index := yamlField(agents, edit.Name)
	if old == nil || old.Kind == yaml.AliasNode || old.Anchor != "" {
		return nil, nil, "", nil, core.Fail("invalid_input", "edit a plain Agent mapping without YAML anchors")
	}
	if edit.Operation == "delete" {
		agents.Content = append(agents.Content[:index], agents.Content[index+2:]...)
	} else {
		var value yaml.Node
		if err := value.Encode(edit.Agent); err != nil {
			return nil, nil, "", nil, err
		}
		value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
		agents.Content[index+1] = &value
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, nil, "", nil, err
	}
	_ = encoder.Close()
	after := &app{home: e.home, configPath: e.path}
	if err := after.loadConfig(output.Bytes()); err != nil {
		return nil, nil, "", nil, err
	}
	n, err := NormalizeDualModeConfig(after.cfg)
	if err != nil {
		return nil, nil, "", nil, err
	}
	change := PlanChange{Kind: "agent", Name: edit.Name, Action: edit.Operation}
	if edit.Operation == "update" {
		selected := n.Declaration.Agents[edit.Name]
		resolve := e.resolveSkills
		if resolve == nil {
			resolve = resolveClaudeSkills
		}
		skills, err := resolve(selected.Skills)
		if err != nil {
			return nil, nil, "", nil, err
		}
		selected.Skills.Resolved = skills
		previous := before.Declaration.Agents[edit.Name]
		previous.Skills.Resolved, _ = resolve(previous.Skills)
		classifyPermissionChange(&change, previous, selected)
	}
	preview := map[string]any{"name": edit.Name, "operation": edit.Operation, "before": before.Declaration.Agents[edit.Name], "after": n.Declaration.Agents[edit.Name], "change": change, "references": refs, "pending_application": true, "plan_command": e.command("config", "plan"), "message": "仅保存 YAML；请通过 config plan 和 apply-runtime 校验并应用。正在运行的 Agent 不会自动修改。"}
	return raw, output.Bytes(), path, preview, nil
}

func (e *agentConfigEditor) preview(ctx context.Context, q core.Queryer, input json.RawMessage) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _, _, out, err := e.prepare(ctx, q, input)
	return out, err
}

func (e *agentConfigEditor) save(ctx context.Context, q core.Queryer, input json.RawMessage) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	lockPath, err := filepath.EvalSymlinks(e.path)
	if err != nil {
		return nil, core.Fail("not_found", "Agent config file unavailable")
	}
	lock, err := os.OpenFile(lockPath+".ui.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, core.Fail("unavailable", "cannot lock Agent config")
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, core.Fail("conflict", "另一管理台正在保存，请稍后重新预览")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	raw, body, path, preview, err := e.prepare(ctx, q, input)
	if err != nil {
		return nil, err
	}
	if path != lockPath {
		return nil, core.Fail("conflict", "配置链接已变化，请重新预览")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, core.Fail("unavailable", "Agent config unavailable")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".memgov-agent-*.yaml")
	if err != nil {
		return nil, core.Fail("unavailable", "cannot prepare Agent config file")
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(info.Mode().Perm()); err == nil {
		_, err = tmp.Write(body)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, core.Fail("unavailable", "cannot save Agent config file")
	}
	current, _, target, err := e.read()
	if err != nil || target != path || !bytes.Equal(current, raw) {
		return nil, core.Fail("conflict", "配置已变化，未保存；请重新预览")
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, core.Fail("unavailable", "cannot replace Agent config file")
	}
	return map[string]any{"saved": true, "revision": core.Hash(body), "preview": preview, "pending_application": true}, nil
}
