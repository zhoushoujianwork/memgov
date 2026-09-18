package cli

import (
	"context"
	"encoding/json"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func (a *app) memoryCommands() {
	source := &cobra.Command{Use: "source", Short: "工作记录与来源证据"}
	ingest := a.write("ingest", "保存来源快照及片段", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, raw json.RawMessage) (any, error) {
		var in core.SourceInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.Ingest(ctx, in)
	})
	source.AddCommand(ingest)
	var sourceLimit int
	sourceList := a.read("list", "列出来源标识、指纹与采集时间", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		return core.SourceList(ctx, s.DB, scope, sourceLimit)
	})
	sourceList.Flags().IntVar(&sourceLimit, "limit", 50, "最多返回条数，1—1000")
	source.AddCommand(sourceList)
	source.AddCommand(a.read("show <id>", "读取来源与证据片段", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		return core.ReadSource(ctx, s.DB, args[0], scope)
	}))
	a.root.AddCommand(source)
	memory := &cobra.Command{Use: "memory", Aliases: []string{"m"}, Short: "正式记忆与历史"}
	var status, category string
	var all bool
	var limit int
	list := a.readCompatible("list", "列出记忆", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		switch status {
		case "active", "disputed", "retired", "superseded", "all":
		default:
			return nil, core.Fail("invalid_input", "unsupported memory status %q", status)
		}
		queryStatus := status
		if queryStatus == "all" {
			queryStatus = ""
		}
		items, err := core.ListMemories(ctx, s.DB, core.SearchOptions{Scope: scope, AllWorkspaces: all, Status: queryStatus, Category: category, Limit: limit})
		if err != nil {
			return nil, err
		}
		if a.human {
			return renderMemoryList(items), nil
		}
		return items, nil
	})
	list.Flags().StringVar(&status, "status", "active", "生命周期过滤：active、disputed、retired、superseded、all")
	list.Flags().StringVar(&category, "category", "", "分类过滤")
	list.Flags().BoolVar(&all, "all-workspaces", false, "显式查询所有工作区")
	list.Flags().IntVar(&limit, "limit", 50, "返回数量")
	memory.AddCommand(list)
	var version int
	show := a.read("show <id>", "读取完整记忆", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		return core.ReadMemory(ctx, s.DB, args[0], scope, version)
	})
	show.Flags().IntVar(&version, "version", 0, "历史版本，默认当前")
	memory.AddCommand(show)
	memory.AddCommand(a.read("history <id>", "版本历史", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		return core.HistoryWithOperations(ctx, s.DB, args[0], scope)
	}))
	var from, to int
	diff := a.read("diff <id>", "比较两个版本", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		before, err := core.ReadMemory(ctx, s.DB, args[0], scope, from)
		if err != nil {
			return nil, err
		}
		after, err := core.ReadMemory(ctx, s.DB, args[0], scope, to)
		return map[string]any{"before": before, "after": after, "changed": core.Digest(before) != core.Digest(after)}, err
	})
	diff.Flags().IntVar(&from, "from", 1, "起始版本")
	diff.Flags().IntVar(&to, "to", 0, "目标版本，默认当前")
	memory.AddCommand(diff)
	for _, spec := range [][2]string{{"retire", "retired"}, {"restore", "active"}} {
		spec := spec
		var reason string
		var revision int
		cmd := a.write(spec[0]+" <id>", "记录生命周期变更或恢复历史内容", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
			return tx.SetState(ctx, args[0], spec[1], reason, a.expected, revision)
		})
		cmd.Flags().StringVar(&reason, "reason", "", "变更理由")
		if spec[0] == "restore" {
			cmd.Flags().IntVar(&revision, "revision", 0, "恢复指定历史内容为新版本")
		}
		memory.AddCommand(cmd)
	}
	a.root.AddCommand(memory)
	a.governanceCommands(memory)
	candidate := &cobra.Command{Use: "candidate", Short: "新建与修订建议"}
	candidate.AddCommand(a.write("submit", "提交候选 JSON", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, raw json.RawMessage) (any, error) {
		var in core.CandidateInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.SubmitCandidate(ctx, in, "", a.actor)
	}))
	candidate.AddCommand(a.read("show <id>", "读取候选", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		return core.ReadCandidate(ctx, s.DB, args[0], scope)
	}))
	candidate.AddCommand(a.read("reviews <id>", "读取独立复核意见与绑定指纹", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		return core.CandidateReviews(ctx, s.DB, args[0], scope)
	}))
	var candidateStatus string
	cl := a.read("list", "列出候选", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		return core.CandidateList(ctx, s.DB, scope, candidateStatus)
	})
	cl.Flags().StringVar(&candidateStatus, "status", "", "候选状态")
	candidate.AddCommand(cl)
	candidate.AddCommand(a.read("diff <id>", "显示候选与当前内容", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		c, err := core.ReadCandidate(ctx, s.DB, args[0], scope)
		if err != nil {
			return nil, err
		}
		var before any
		if c.TargetID != "" {
			before, err = core.ReadMemory(ctx, s.DB, c.TargetID, scope, 0)
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"before": before, "after": c.Memory, "digest": c.Digest}, nil
	}))
	candidate.AddCommand(a.write("validate <id>", "校验结构、证据与版本", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		c, err := tx.ValidateCandidate(ctx, args[0])
		if err != nil {
			return nil, err
		}
		return map[string]any{"valid": true, "candidate": c, "semantic_review": "not performed by structural validation"}, nil
	}))
	var digest string
	apply := a.write("apply <id>", "应用已审阅候选", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.ApplyCandidate(ctx, args[0], digest)
	})
	apply.Flags().StringVar(&digest, "expected-digest", "", "已审阅候选的摘要指纹")
	candidate.AddCommand(apply)
	var reason string
	reject := a.write("reject <id>", "拒绝候选", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.RejectCandidate(ctx, args[0], reason)
	})
	reject.Flags().StringVar(&reason, "reason", "", "拒绝理由")
	candidate.AddCommand(reject)
	a.root.AddCommand(candidate)
	for _, name := range []string{"search", "recall"} {
		name := name
		var opts core.SearchOptions
		var budget int
		var explain bool
		cmd := a.read(name+" <query>", "检索记忆与证据；召回受状态和字符预算约束", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
			scope, err := a.scope(ctx, s)
			if err != nil {
				return nil, err
			}
			opts.Scope = scope
			if name == "search" {
				return core.Search(ctx, s.DB, args[0], opts)
			}
			r, err := core.Recall(ctx, s.DB, args[0], opts, budget, explain)
			if a.format == "markdown" || a.format == "text" {
				return r.Context, err
			}
			return r, err
		})
		cmd.Flags().BoolVar(&opts.AllWorkspaces, "all-workspaces", false, "显式查询所有工作区")
		cmd.Flags().IntVar(&opts.Limit, "limit", 20, "最多返回条数")
		cmd.Flags().StringVar(&opts.Category, "category", "", "分类过滤")
		if name == "search" {
			cmd.Flags().StringVar(&opts.Kind, "kind", "", "memory 或 source")
			cmd.Flags().StringVar(&opts.Status, "status", "", "生命周期过滤")
		} else {
			cmd.Flags().IntVar(&budget, "budget-chars", 12000, "上下文 Unicode 字符预算")
			cmd.Flags().BoolVar(&explain, "explain", false, "解释召回依据")
		}
		a.root.AddCommand(cmd)
	}
	index := &cobra.Command{Use: "index", Short: "仅维护派生索引"}
	index.AddCommand(a.write("rebuild", "从权威数据重建 FTS", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, _ json.RawMessage) (any, error) {
		return tx.Reindex(ctx)
	}))
	a.root.AddCommand(index)
}
