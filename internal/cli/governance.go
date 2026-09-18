package cli

import (
	"context"
	"encoding/json"
	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func (a *app) governanceCommands(memory *cobra.Command) {
	merge := &cobra.Command{Use: "merge", Short: "版本绑定的原子合并与撤销"}
	merge.AddCommand(a.write("preview", "校验合并目标、全部源版本和证据", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, raw json.RawMessage) (any, error) {
		var in core.MergeInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.PreviewMerge(ctx, in)
	}))
	var digest string
	apply := a.write("apply <plan-id>", "原子创建目标并替代源记忆", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.ApplyMerge(ctx, args[0], digest)
	})
	apply.Flags().StringVar(&digest, "expected-digest", "", "已审阅合并计划指纹")
	merge.AddCommand(apply)
	var reason string
	undo := a.write("undo <plan-id>", "追加版本撤销合并，有后续修改时返回冲突", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.UndoMerge(ctx, args[0], reason)
	})
	undo.Flags().StringVar(&reason, "reason", "", "撤销理由")
	merge.AddCommand(undo)
	memory.AddCommand(merge)
	for _, name := range []string{"links", "graph"} {
		var depth, limit int
		cmd := a.read(name+" <id>", "证据支持、来源派生和替代关系", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
			scope, err := a.scope(ctx, s)
			if err != nil {
				return nil, err
			}
			g, err := core.MemoryGraph(ctx, s.DB, args[0], scope, depth, limit)
			if a.format == "mermaid" {
				return g.Mermaid(), err
			}
			return g, err
		})
		cmd.Flags().IntVar(&depth, "depth", 1, "关系深度，最多 5")
		cmd.Flags().IntVar(&limit, "limit", 100, "最多关系条数，最多 1000")
		if name == "links" {
			for _, action := range []string{"conflict", "resolve"} {
				action := action
				var otherVersion int
				var reason string
				change := a.write(action+" <id> <other-id>", "记录或解决冲突关系；绑定双方版本", cobra.ExactArgs(2), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
					return tx.MemoryConflict(ctx, args[0], args[1], a.expected, otherVersion, action == "resolve", reason)
				})
				change.Flags().IntVar(&otherVersion, "other-version", 0, "另一记忆的期望版本")
				change.Flags().StringVar(&reason, "reason", "", "关系变更依据")
				cmd.AddCommand(change)
			}
		}
		memory.AddCommand(cmd)
	}
	purge := &cobra.Command{Use: "purge", Short: "清除正文副本并保留非正文墓碑"}
	purge.AddCommand(a.write("preview <memory-id>", "枚举关联正文、共享证据与潜在外部残留", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.PreviewPurge(ctx, args[0], a.expected)
	}))
	var purgeDigest string
	var logical bool
	pa := a.simple("apply <plan-id>", "应用清除或恢复尚未完成的数据库整理", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		s, err := core.OpenMaintenance(ctx, a.dbPath())
		if err != nil {
			return nil, err
		}
		defer s.Close()
		scope, err := a.scope(ctx, s)
		if err != nil {
			return nil, err
		}
		r, err := s.Mutate(ctx, core.Request{ID: a.requestID, Scope: scope, Command: "purge.apply", Actor: a.actor, Key: a.key, Input: map[string]any{"id": args[0], "digest": purgeDigest}}, func(tx *core.Tx) (any, error) { return tx.ApplyPurge(ctx, args[0], purgeDigest) })
		if err != nil {
			return nil, err
		}
		if logical {
			return r.Data, nil
		}
		return s.CompactPurge(ctx, args[0])
	})
	pa.Flags().StringVar(&purgeDigest, "expected-digest", "", "清除预览指纹")
	pa.Flags().BoolVar(&logical, "logical-only", false, "只提交逻辑清除，稍后重复 apply 完成整理")
	purge.AddCommand(pa)
	purge.AddCommand(a.read("status <plan-id>", "逻辑清除与数据库整理状态", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.PurgeStatus(ctx, s.DB, args[0])
	}))
	memory.AddCommand(purge)
}
