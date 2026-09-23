package cli

import (
	"context"
	"encoding/json"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// outboxCommands expose the reply-draft path. Nothing here sends except
// dispatch, and dispatch refuses unless sending was deliberately enabled, the
// capability was verified and the displayed digest still matches.
func (a *app) outboxCommands() {
	root := &cobra.Command{Use: "outbox", Short: "回复草稿、展示与投递状态"}
	var state string
	var limit int
	list := a.read("list <channel>", "列出草稿与状态计数（查询不发送）", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.DraftList(ctx, s.DB, args[0], state, limit)
	})
	list.Flags().StringVar(&state, "state", "", "按状态过滤：draft、ready、sending、accepted、stale、cancelled 等")
	list.Flags().IntVar(&limit, "limit", 50, "返回数量，1—500")
	root.AddCommand(list)
	root.AddCommand(a.read("show <draft>", "读取草稿本体", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.ReadDraft(ctx, s.DB, args[0])
	}))
	// preview is the display contract: content, target, identity, citations and
	// checks. Reading it sends nothing and records no approval.
	root.AddCommand(a.read("preview <draft>", "展示待发内容、目标、证据与检查结果（不发送）", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.PreviewDraft(ctx, s.DB, args[0])
	}))

	var digest string
	dispatch := a.write("dispatch <draft>", "显式发送该份草稿（需 --expected-digest）", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		// The second permission check runs here, inside the transaction. The
		// platform call itself is a separate step: authorizing records the attempt,
		// and the result is recorded afterwards through `outbox result`.
		d, err := tx.AuthorizeDispatch(ctx, args[0], digest)
		if err != nil {
			return nil, err
		}
		return map[string]any{"draft": d, "state": d.State, "sent": false,
			"note": "the dispatch was authorized and the attempt recorded; the transport call and its receipt are recorded separately"}, nil
	})
	dispatch.Flags().StringVar(&digest, "expected-digest", "", "展示时读到的 display_digest（必填）")
	root.AddCommand(dispatch)

	var receipt, detail string
	result := a.write("result <draft> <state>", "记录平台回执：accepted、retryable_failed、delivery_unknown、failed", cobra.ExactArgs(2), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.RecordDelivery(ctx, args[0], args[1], receipt, detail)
	})
	result.Flags().StringVar(&receipt, "receipt", "", "平台回执标识；accepted 必填")
	result.Flags().StringVar(&detail, "detail", "", "补充说明或错误信息")
	root.AddCommand(result)

	var reason string
	cancel := a.write("cancel <draft>", "取消尚未交付平台的草稿", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.CancelDraft(ctx, args[0], reason)
	})
	cancel.Flags().StringVar(&reason, "reason", "", "取消理由（必填）")
	root.AddCommand(cancel)

	var outcome, evidence string
	reconcile := a.write("reconcile <draft>", "为 delivery_unknown 记录可核验结论", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, _ json.RawMessage) (any, error) {
		return tx.Reconcile(ctx, args[0], outcome, receipt, evidence)
	})
	reconcile.Flags().StringVar(&outcome, "outcome", "", "accepted 或 not_sent")
	reconcile.Flags().StringVar(&receipt, "receipt", "", "平台回执标识；accepted 必填")
	reconcile.Flags().StringVar(&evidence, "evidence", "", "实际核对到的依据（必填）")
	root.AddCommand(reconcile)
	a.root.AddCommand(root)

	// The context and draft entry points belong to the reply flow; an external
	// Agent uses them to answer inside a target memgov itself established.
	reply := &cobra.Command{Use: "reply", Short: "受信请求上下文与回复草稿"}
	reply.AddCommand(a.write("open <channel>", "按可信连接建立请求上下文", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, raw json.RawMessage) (any, error) {
		var in core.ContextInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.OpenContext(ctx, args[0], in)
	}))
	reply.AddCommand(a.read("show <context>", "读取请求上下文", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.ReadContext(ctx, s.DB, args[0])
	}))
	reply.AddCommand(a.write("draft", "记录回复草稿（引用逐条重新检查）", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, raw json.RawMessage) (any, error) {
		var in core.DraftInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.Draft(ctx, in)
	}))
	a.root.AddCommand(reply)
}
