package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/channel/dingtalkapp"
	"github.com/zhoushoujianwork/memgov/internal/channel/dws"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// ensureAdapterRegistry installs the built-in factories at the composition
// root. channel itself stays platform-neutral; replacing this registry is
// enough for an embedding binary to select another platform implementation.
func (a *app) ensureAdapterRegistry() *channel.Registry {
	a.adapterMu.Lock()
	defer a.adapterMu.Unlock()
	if a.adapterRegistry != nil {
		return a.adapterRegistry
	}
	r := channel.NewRegistry()
	// These factories preserve the historical defaults. They are registered
	// here, rather than in channel, so importing the contract never imports a
	// platform SDK.
	_ = r.Register(core.ChannelDwsPersonal, func() channel.Adapter { return dws.New() })
	_ = r.Register(core.ChannelDingTalkApp, func() channel.Adapter { return dingtalkapp.New() })
	a.adapterRegistry = r
	return r
}

// adapterFor selects the adapter that matches the stored channel kind. A
// channel is never served by another kind's adapter, because that would mean
// using a different identity and permission set than the one it was
// configured with. Instances are cached per kind so callback-scoped platform
// state (for example a reply webhook) remains shared by receiver and workers.
func (a *app) adapterFor(c core.Channel) (channel.Adapter, error) {
	// Keep the existing injection points for offline tests and callers that need
	// to replace one of the built-in adapters without replacing the registry.
	if c.Kind == core.ChannelDwsPersonal && a.dwsAdapter != nil {
		return a.dwsAdapter, nil
	}
	if c.Kind == core.ChannelDingTalkApp {
		a.adapterMu.Lock()
		if a.appAdapter != nil {
			adapter := a.appAdapter
			a.adapterMu.Unlock()
			return adapter, nil
		}
		a.adapterMu.Unlock()
	}

	r := a.ensureAdapterRegistry()
	factory, ok := r.Lookup(c.Kind)
	if !ok {
		return nil, core.Fail("invalid_input", "unsupported channel kind %q", c.Kind)
	}

	a.adapterMu.Lock()
	defer a.adapterMu.Unlock()
	if c.Kind == core.ChannelDingTalkApp && a.appAdapter != nil {
		return a.appAdapter, nil
	}
	if a.adapterInstances == nil {
		a.adapterInstances = make(map[string]channel.Adapter)
	}
	if adapter := a.adapterInstances[c.Kind]; adapter != nil {
		return adapter, nil
	}
	adapter := factory()
	if adapter == nil {
		return nil, core.Fail("unavailable", "adapter factory for %q returned nil", c.Kind)
	}
	a.adapterInstances[c.Kind] = adapter
	if c.Kind == core.ChannelDingTalkApp {
		a.appAdapter = adapter
	}
	return adapter, nil
}

// collector opens the store and resolves the adapter for one channel. Contacting
// a platform only ever happens through this path.
func (a *app) collector(ctx context.Context, value string) (channel.Collector, core.Channel, *core.Store, error) {
	s, err := core.Open(ctx, a.dbPath(), false)
	if err != nil {
		return channel.Collector{}, core.Channel{}, nil, err
	}
	stored, err := core.ReadChannel(ctx, s.DB, value)
	if err != nil {
		s.Close()
		return channel.Collector{}, core.Channel{}, nil, err
	}
	adapter, err := a.adapterFor(stored)
	if err != nil {
		s.Close()
		return channel.Collector{}, core.Channel{}, nil, err
	}
	// The workspace name is resolved once here, so every write in this session is
	// scoped the same way a plain write command would be.
	if a.resolved, err = a.scope(ctx, s); err != nil {
		s.Close()
		return channel.Collector{}, core.Channel{}, nil, err
	}
	return channel.Collector{Store: s, Adapter: adapter}, stored, s, nil
}

// request describes the collection commands, which commit many independent
// transactions in one invocation: every event, the coverage record and the lease
// are separate writes. The request ID is deliberately left empty so each commit
// records its own request row; reusing one ID would make the second write
// collide and lose the first one's audit trail.
func (a *app) request() core.Request {
	return core.Request{Scope: a.resolved, Actor: a.actor}
}

func (a *app) channelCommands() {
	root := &cobra.Command{Use: "channel", Short: "消息通道配置与采集"}
	root.AddCommand(a.write("add", "注册消息通道（不连接平台）", cobra.NoArgs, func(ctx context.Context, tx *core.Tx, _ []string, raw json.RawMessage) (any, error) {
		var in core.ChannelInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.AddChannel(ctx, in)
	}))
	root.AddCommand(a.read("list", "列出通道及其会话绑定", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) {
		return core.ChannelList(ctx, s.DB)
	}))
	root.AddCommand(a.read("show <channel>", "读取通道配置、能力与绑定", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.ReadChannel(ctx, s.DB, args[0])
	}))
	root.AddCommand(a.read("plan <channel>", "离线说明将要订阅的范围与身份", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.ChannelPlan(ctx, s.DB, args[0])
	}))
	root.AddCommand(a.read("doctor <channel>", "离线检查通道配置与能力记录", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.ChannelDoctor(ctx, s.DB, args[0])
	}))
	root.AddCommand(a.read("status <channel>", "查看覆盖范围、水位与未闭合缺口", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.CoverageReport(ctx, s.DB, args[0])
	}))

	route := &cobra.Command{Use: "route", Short: "会话绑定与受众策略"}
	route.AddCommand(a.write("add <channel>", "绑定会话并设置受众与发送策略", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, raw json.RawMessage) (any, error) {
		var in core.RouteInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		c, err := core.ReadChannel(ctx, tx.Conn, args[0])
		if err != nil {
			return nil, err
		}
		return tx.AddRoute(ctx, c.ID, in)
	}))
	var reason string
	update := a.write("update <route-id>", "修改绑定策略（需要 --expected-version 与理由）", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, raw json.RawMessage) (any, error) {
		var in core.RouteInput
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return tx.UpdateRoute(ctx, args[0], a.expected, in, reason)
	})
	update.Flags().StringVar(&reason, "reason", "", "变更理由（必填）")
	route.AddCommand(update)
	root.AddCommand(route)

	// probe is the only place a capability becomes verified, and it contacts the
	// platform with the channel's own identity.
	root.AddCommand(a.simple("probe <channel>", "核对登录身份并记录实际验证到的能力", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		c, _, s, err := a.collector(ctx, args[0])
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return c.Probe(ctx, a.request(), args[0])
	}))

	var conversation, startAt, endAt string
	var pageLimit, maxItems int
	var span, overlap time.Duration
	pull := a.simple("pull <channel>", "按 [start,end) 回填一个历史窗口", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		c, stored, s, err := a.collector(ctx, args[0])
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if conversation == "" {
			return nil, core.Fail("invalid_input", "--conversation is required; a window is always read for one bound conversation")
		}
		start, end, err := resolveWindow(ctx, s, stored.ID, conversation, startAt, endAt, span, overlap)
		if err != nil {
			return nil, err
		}
		return c.Pull(ctx, a.request(), args[0], conversation, start, end, pageLimit, maxItems)
	})
	pull.Flags().StringVar(&conversation, "conversation", "", "已绑定的会话 ID")
	pull.Flags().StringVar(&startAt, "start", "", "窗口起点（RFC3339，含）")
	pull.Flags().StringVar(&endAt, "end", "", "窗口终点（RFC3339，不含）")
	pull.Flags().IntVar(&pageLimit, "page-limit", 5, "单次最多翻页数")
	pull.Flags().IntVar(&maxItems, "max-items", 200, "单次最多条数")
	pull.Flags().DurationVar(&span, "span", time.Hour, "未指定窗口时按水位推算的跨度")
	pull.Flags().DurationVar(&overlap, "overlap", 5*time.Minute, "与已覆盖范围的重叠，用于弥补非事务边界")
	root.AddCommand(pull)

	var ttl time.Duration
	run := a.simple("run <channel>", "前台运行接收会话（持有通道租约）", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		if err := a.requireStandalone(); err != nil {
			return nil, err
		}
		c, _, s, err := a.collector(ctx, args[0])
		if err != nil {
			return nil, err
		}
		defer s.Close()
		return c.Receive(ctx, a.request(), args[0], ttl)
	})
	run.Flags().DurationVar(&ttl, "lease-ttl", 15*time.Minute, "接收租约有效期")
	root.AddCommand(run)

	var file string
	ingest := a.simple("ingest <channel>", "离线导入规范化事件（不连接平台）", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		c, _, s, err := a.collector(ctx, args[0])
		if err != nil {
			return nil, err
		}
		defer s.Close()
		var r io.Reader = a.in
		if file != "" && file != "-" {
			f, err := os.Open(file)
			if err != nil {
				return nil, core.Fail("not_found", "could not open the event file: %v", err)
			}
			defer f.Close()
			r = f
		}
		return c.Ingest(ctx, a.request(), args[0], r)
	})
	ingest.Flags().StringVar(&file, "file", "-", "NDJSON 事件文件，- 表示 stdin")
	root.AddCommand(ingest)
	a.root.AddCommand(root)
}

// resolveWindow builds the half-open range. When no explicit range is given it
// continues from the watermark with a deliberate overlap, because the boundary
// between receiving and committing is not transactional.
func resolveWindow(ctx context.Context, s *core.Store, channelID, conversation, startAt, endAt string, span, overlap time.Duration) (time.Time, time.Time, error) {
	if startAt == "" && endAt == "" {
		return core.NextWindow(ctx, s.DB, channelID, conversation, time.Now(), span, overlap)
	}
	if startAt == "" || endAt == "" {
		return time.Time{}, time.Time{}, core.Fail("invalid_input", "--start and --end must be given together, or neither")
	}
	start, err := time.Parse(time.RFC3339, startAt)
	if err != nil {
		return time.Time{}, time.Time{}, core.Fail("invalid_input", "--start must use RFC3339")
	}
	end, err := time.Parse(time.RFC3339, endAt)
	if err != nil {
		return time.Time{}, time.Time{}, core.Fail("invalid_input", "--end must use RFC3339")
	}
	return start, end, nil
}

func (a *app) messageCommands() {
	root := &cobra.Command{Use: "message", Short: "消息记录、身份与撤回治理"}
	var conversation string
	var limit int
	list := a.read("list <channel>", "列出消息（撤回内容不展示正文）", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.MessageList(ctx, s.DB, args[0], conversation, limit)
	})
	list.Flags().StringVar(&conversation, "conversation", "", "限定会话 ID")
	list.Flags().IntVar(&limit, "limit", 50, "返回数量，1—500")
	root.AddCommand(list)
	var queryConversation, queryConversationType, queryContactType, queryContactValue, query, since, until string
	var queryLimit int
	queryCommand := a.read("query <channel>", "查询本地已观测增量会话及其覆盖水位", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.MessageQuery(ctx, s.DB, args[0], core.MessageQueryInput{Conversation: queryConversation, ConversationType: queryConversationType, ContactIDType: queryContactType, ContactIDValue: queryContactValue, Query: query, Since: since, Until: until, Limit: queryLimit})
	})
	queryCommand.Flags().StringVar(&queryConversation, "conversation", "", "限定会话 ID")
	queryCommand.Flags().StringVar(&queryConversationType, "type", "", "限定会话类型：direct 或 group")
	queryCommand.Flags().StringVar(&queryContactType, "contact-id-type", "", "稳定联系人标识类型")
	queryCommand.Flags().StringVar(&queryContactValue, "contact-id", "", "稳定联系人标识值")
	queryCommand.Flags().StringVar(&query, "query", "", "匹配正文或已观测的发送者显示名")
	queryCommand.Flags().StringVar(&since, "since", "", "起始时间（RFC3339，含）")
	queryCommand.Flags().StringVar(&until, "until", "", "结束时间（RFC3339，不含）")
	queryCommand.Flags().IntVar(&queryLimit, "limit", 50, "返回数量，1—500")
	root.AddCommand(queryCommand)
	root.AddCommand(a.read("show <message>", "读取消息、修订与来源", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.ReadMessage(ctx, s.DB, args[0])
	}))
	root.AddCommand(a.read("association <message>", "查看跨通道平台已核验消息对应或未解决状态", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.MessageAssociationStatus(ctx, s.DB, args[0])
	}))

	inbox := &cobra.Command{Use: "inbox", Short: "接收事件与处理状态"}
	var status string
	var inboxLimit int
	inboxList := a.read("list <channel>", "列出接收事件及状态计数", cobra.ExactArgs(1), func(ctx context.Context, s *core.Store, args []string) (any, error) {
		return core.InboxList(ctx, s.DB, args[0], status, inboxLimit)
	})
	inboxList.Flags().StringVar(&status, "status", "", "按状态过滤：received、applied、rejected、pending_original")
	inboxList.Flags().IntVar(&inboxLimit, "limit", 50, "返回数量，1—500")
	inbox.AddCommand(inboxList)
	root.AddCommand(inbox)

	identity := &cobra.Command{Use: "identity", Short: "主体与可验证身份映射"}
	var basis string
	link := a.write("link <channel>", "以可验证依据合并两个平台标识", cobra.ExactArgs(1), func(ctx context.Context, tx *core.Tx, args []string, raw json.RawMessage) (any, error) {
		var in struct {
			From core.Sender `json:"from"`
			To   core.Sender `json:"to"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		c, err := core.ReadChannel(ctx, tx.Conn, args[0])
		if err != nil {
			return nil, err
		}
		return tx.LinkIdentity(ctx, c.Tenant, in.From, in.To, basis)
	})
	link.Flags().StringVar(&basis, "basis", "", "映射依据：platform_directory、operator_confirmed、same_open_id")
	identity.AddCommand(link)
	root.AddCommand(identity)
	a.root.AddCommand(root)
}

// audienceCommands expose the disclosure boundary. Publishing permits disclosure
// for one audience and version; it never sends anything.
