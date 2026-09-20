package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/runlog"
)

// debugExecutionInput dumps the full ExecutionInput handed to the Agent when
// MEMGOV_DEBUG_EXECUTION_INPUT=1. It writes to stderr and is a no-op otherwise,
// so it carries no cost in normal operation.
func debugExecutionInput(in ExecutionInput) {
	if os.Getenv("MEMGOV_DEBUG_EXECUTION_INPUT") == "" {
		return
	}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[debug] failed to marshal ExecutionInput: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "[debug] ExecutionInput task=%s attempt=%s:\n%s\n", in.Task.ID, in.SessionID, string(b))
}

// The direct lane is a conversation transport. The Agent receives the original
// message and decides which skills and tools to use; no intent/recall step runs.
func (s *Service) executeDirectTurn(ctx context.Context, cfg core.RuntimeConfig, task core.RuntimeTask, attempt core.RuntimeAttempt) {
	var err error
	fullTask, err := core.ReadRuntimeTask(ctx, s.Store.DB, task.ID)
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	task = fullTask
	var session core.RuntimeDirectSession
	err = s.mutate(ctx, "global", "runtime.direct.bind", func(tx *core.Tx) (any, error) {
		session, err = tx.BindRuntimeDirectTurn(ctx, cfg, task)
		return session, err
	})
	if err != nil {
		s.failTask(ctx, cfg, task, attempt, err)
		return
	}
	closeSession := func() {
		if closer, ok := s.Executor.(interface{ CloseDirectSession(string) }); ok {
			closer.CloseDirectSession(session.ID)
		}
	}
	fail := func(e error) { closeSession(); s.failTask(ctx, cfg, task, attempt, e) }
	var result core.RuntimeAttemptResult
	var directNativeID, persistedPolicyDigest string
	var directHistory []core.RuntimeMessage
	start := s.now()
	if session.Command != "" {
		switch session.Command {
		case "clear":
			if closer, ok := s.Executor.(interface{ CloseDirectSessions() }); ok {
				closer.CloseDirectSessions()
			}
			result.Result = "已开启新会话。接下来的消息不再携带上一段对话；长期记忆保持原有状态，由 Agent 按需使用。"
		case "status":
			result.Result, err = s.directAgentStatus(ctx, cfg, task)
			if err != nil {
				fail(err)
				return
			}
		}
		result.Usage = map[string]any{"model": "system"}
	} else {
		policy, preset, e := s.resolveTaskAgent(ctx, cfg, task)
		if e != nil {
			fail(e)
			return
		}
		err = s.mutate(ctx, "global", "runtime.attempt.agent", func(tx *core.Tx) (any, error) {
			return nil, tx.RecordRuntimeAttemptAgent(ctx, cfg, task, attempt.ID, policy, preset.Commit)
		})
		if err != nil {
			fail(err)
			return
		}
		workspace, e := core.RuntimeTaskWorkspace(ctx, s.Store.DB, task)
		if e != nil {
			fail(e)
			return
		}
		workdir := filepath.Join(s.Home, "runtime", "sessions", session.ID)
		if task.Resume != nil {
			previous, e := previousResumeAttempt(task)
			if e != nil || previous.WorkspaceDir != workdir {
				if e == nil {
					e = core.Fail("conflict", "original private working directory no longer matches")
				}
				fail(e)
				return
			}
			if _, e = os.Stat(workdir); e != nil {
				fail(core.Fail("not_found", "previous private working directory is unavailable"))
				return
			}
		}
		if e = os.MkdirAll(workdir, 0700); e != nil {
			fail(e)
			return
		}
		turnControl, _ := json.Marshal(map[string]string{"task_id": task.ID, "attempt_id": attempt.ID, "workspace_id": workspace.ID})
		if e = os.WriteFile(filepath.Join(workdir, ".memgov-turn.json"), turnControl, 0600); e != nil {
			fail(e)
			return
		}
		err = s.mutate(ctx, "global", "runtime.attempt.workspace", func(tx *core.Tx) (any, error) { return nil, tx.SetRuntimeAttemptWorkspace(ctx, attempt.ID, workdir) })
		if err != nil {
			fail(err)
			return
		}
		history, e := core.RuntimeDirectHistory(ctx, s.Store.DB, cfg, task, session.ID)
		if e != nil {
			fail(e)
			return
		}
		// Private turns do not implicitly query memgov memory. The explicit
		// memory skill remains available to the Agent when it is relevant.
		hotwords := ""
		channelPrompt, e := s.runtimeChannelSystemPrompt(ctx, cfg, task)
		if e != nil {
			fail(e)
			return
		}
		if e = core.RuntimeAttemptPolicyCurrent(ctx, s.Store.DB, attempt.ID, task.ID, task.Version); e != nil {
			fail(e)
			return
		}
		if current, e := core.RuntimeDirectTurnCurrent(ctx, s.Store.DB, task.ID); e != nil || !current {
			if e == nil {
				e = core.Fail("conflict", "direct session changed")
			}
			fail(e)
			return
		}
		execInput := ExecutionInput{Task: task, AttemptID: attempt.ID, SessionID: session.ID, NativeSessionID: attempt.ID, RecordSession: s.sessionRecorder(task, attempt), DirectoryPolicy: policy.Directories, AgentPolicyDigest: core.Digest(policy), Home: s.Home, AgentHome: effectiveAgentHome(s.Home, policy.Home, policy.Agent), WorkspaceID: workspace.ID, WorkspacePath: workspace.Path, WorkDir: workdir, Preset: preset, ApplicationMode: "direct", Capabilities: policy.Capabilities, BashEnabled: policy.BashEnabled, ExternalActions: policy.ExternalActions, ConversationContext: history, HotwordContext: hotwords, ChannelSystemPrompt: channelPrompt, Skills: policy.Skills, PolicyResolved: true, ExecutionModel: policy.ExecutionModel, ClaudeProfile: policy.ClaudeProfile}
		// A direct session has a stable logical ID and a separately persisted
		// native Claude ID. Resume only when the policy and accepted history are
		// exactly the context used to create the native session; otherwise start
		// a fresh native session while still supplying the durable replay history.
		if session.NativeSessionID != "" && session.NativePolicyDigest == directPolicyDigest(execInput, policy.ClaudeProfile, policy.ExecutionModel) && session.NativeContextDigest == directHistoryDigest(history) {
			execInput.NativeSessionID = session.NativeSessionID
			execInput.ResumeSessionID = session.NativeSessionID
		}
		directHistory = history
		directNativeID = execInput.NativeSessionID
		persistedPolicyDigest = directPolicyDigest(execInput, policy.ClaudeProfile, policy.ExecutionModel)
		result, err = s.Executor.Execute(ctx, execInput)
		if err == nil {
			err = s.checkTaskAgent(ctx, cfg, task, policy, preset)
		}
		if err != nil {
			fail(err)
			return
		}
	}
	var completed core.RuntimeTask
	err = s.mutate(ctx, "global", "runtime.direct.complete", func(tx *core.Tx) (any, error) {
		var e error
		completed, e = tx.CompleteRuntimeTask(ctx, task.ID, task.Version, attempt.ID, result, "")
		return completed, e
	})
	if err != nil {
		fail(err)
		return
	}
	input, output, cost := usageFields(result.Usage)
	model, _ := result.Usage["model"].(string)
	s.emit(ctx, runlog.Event{RuntimeID: cfg.ID, TaskID: task.ID, AttemptID: attempt.ID, Level: "info", Component: "execution", Event: "completed", Status: completed.Status, DurationMS: s.now().Sub(start).Milliseconds(), Model: model, InputTokens: input, OutputTokens: output, CostUSD: cost, ToolKinds: result.ToolKinds, Summary: "会话回复已完成"})
	delivered := s.deliver(ctx, cfg, task.ID)
	if delivered && session.Command == "" {
		// Make the native session reusable only after delivery acceptance, so
		// replay history and native context cannot diverge on a failed handoff.
		_ = s.mutate(ctx, "global", "runtime.direct.native_session", func(tx *core.Tx) (any, error) {
			acceptedHistory := append([]core.RuntimeMessage{}, directHistory...)
			acceptedHistory = append(acceptedHistory, task.Messages[len(task.Messages)-1], core.RuntimeMessage{Body: result.Result, SelfAuthored: true})
			return nil, tx.PersistRuntimeDirectNativeSession(ctx, session.ID, directNativeID, persistedPolicyDigest, directHistoryDigest(acceptedHistory))
		})
	}
	if !delivered {
		closeSession()
	}
	if completed.Status == "completed" {
		s.completionAcknowledgement(ctx, cfg, task.ID)
	}
}

func (s *Service) directAgentStatus(ctx context.Context, cfg core.RuntimeConfig, task core.RuntimeTask) (string, error) {
	current, err := core.ReadRuntime(ctx, s.Store.DB, cfg.ID)
	if err != nil {
		return "", err
	}
	policy, preset, err := s.resolveTaskAgent(ctx, cfg, task)
	if err != nil {
		return "", err
	}
	policy.Skills, err = resolveClaudeSkillPolicy(policy.Skills)
	if err != nil {
		return "", err
	}
	agentName := policy.Agent
	if agentName == "" {
		agentName = "owner-default"
	}
	capabilities := append([]string{}, policy.Capabilities...)
	sort.Strings(capabilities)
	skills := make([]string, 0, len(policy.Skills.Resolved)+1)
	if hasAgentCapability(policy.Capabilities, "memory_read") {
		skills = append(skills, "memgov-memory（运行时管理）")
	}
	for _, skill := range policy.Skills.Resolved {
		skills = append(skills, skill.Name)
	}
	skillCount := len(skills)
	skillText := "无"
	if skillCount > 0 {
		skillText = strings.Join(skills, "、")
	}
	commit := preset.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	inherit := policy.Skills.Inherit
	if inherit == "" {
		inherit = "none"
	}
	hotwordWrite := hasAgentCapability(policy.Capabilities, "memory_read") && hasAgentCapability(policy.Capabilities, "local_write")
	collection := "私聊增量采集：未关联"
	if source, bound, sourceErr := core.RuntimeContextDataSource(ctx, s.Store.DB, cfg); sourceErr == nil && bound {
		collection = s.directCollectionStatus(ctx, source)
	} else if current.ContextChannelID != "" {
		if source, e := core.ReadDataSourceByChannel(ctx, s.Store.DB, current.ContextChannelID); e == nil {
			collection = s.directCollectionStatus(ctx, source)
		}
	}
	return fmt.Sprintf("Agent 状态\n运行：%s\nAgent：%s\nPreset：%s@%s\n执行模型：%s（Claude 配置：%s）\n技能继承：%s\n技能（%d）：%s\n能力：%s\nBash：%t\n外部操作：%s\n记忆范围：%s\n热词纠正写入：%t\n声明目录：%d\n%s\n会话：已就绪\n说明：普通私聊不会自动召回全部记忆；Agent 会按需调用 memgov-memory。", current.Status, agentName, preset.Name, commit, policy.ExecutionModel, policy.ClaudeProfile, inherit, skillCount, skillText, strings.Join(capabilities, "、"), policy.BashEnabled, policy.ExternalActions, policy.MemoryScope, hotwordWrite, len(policy.Directories), collection), nil
}

func formatDirectCollectionStatus(source core.DataSource) string {
	state := "关闭"
	if source.DirectEnabled {
		state = "开启"
	}
	return fmt.Sprintf("私聊增量采集：%s；启用时间：%s；发现覆盖至：%s；保留：%d 天；最近收流：%s；最近清理：%s（%d 条）；收流错误：%s；清理错误：%s", state, emptyStatus(source.DirectEnabledAt), emptyStatus(source.DirectDiscoveryCoveredUntil), source.RetentionDays, emptyStatus(source.LastDirectReceivedAt), emptyStatus(source.LastRetentionAt), source.LastRetentionCount, emptyStatus(source.LastErrorCode), emptyStatus(source.RetentionErrorCode))
}

func (s *Service) directCollectionStatus(ctx context.Context, source core.DataSource) string {
	text := formatDirectCollectionStatus(source)
	gaps, err := core.DataSourceGapCount(ctx, s.Store.DB, source)
	if err != nil {
		return text + "；缺口：查询失败"
	}
	return fmt.Sprintf("%s；七天窗口缺口：%d", text, gaps)
}

func emptyStatus(value string) string {
	if value == "" {
		return "无"
	}
	return value
}
