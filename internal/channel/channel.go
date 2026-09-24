// Package channel defines the narrow contract every message adapter implements.
// Adapters translate a platform into normalized events and delivery attempts.
// They never touch memories, permissions or task tables: the core hands them a
// frozen receive configuration or a delivery request and nothing more.
package channel

import (
	"context"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// Config is the frozen receive configuration handed to an adapter. It carries
// identity expectations and bound conversations, never a secret value.
type Config struct {
	ChannelID     string
	ChannelName   string
	Kind          string
	Tenant        string
	AuthNamespace string
	IDNamespace   string
	Identity      core.ChannelIdentity
	CredentialRef string
	Conversations []string
	// DirectConversations lets adapters open a dedicated direct-message
	// subscription without guessing conversation kinds from provider IDs.
	DirectConversations []string
	// CollectAllDirect subscribes to real one-to-one provider conversations.
	// It is set only by an explicitly enabled data source.
	CollectAllDirect bool
	CollectGroups    bool
	Tool             string
}

// Window is a half-open history range. The end is exclusive so two adjacent
// windows can never double-count or skip a message at the boundary.
type Window struct {
	ConversationID   string
	ConversationType string
	Start            time.Time
	End              time.Time
	PageLimit        int
	MaxItems         int
	Cursor           string
}

// WindowResult separates "the range was fully read" from "reading stopped".
// Complete is true only when every page of the range succeeded, so hitting a
// page or item cap is partial rather than "no more messages".
type WindowResult struct {
	Events     []core.NormalizedEvent
	Complete   bool
	StopReason string
	NextCursor string
	Pages      int
	Truncated  bool
}

// ReceiverOptions describes one live receive session. Ready is closed only after
// the adapter observed the platform's own readiness signal, never after a sleep.
type ReceiverOptions struct {
	Ready  func(detail map[string]any)
	Handle func(context.Context, core.NormalizedEvent) error
	// Reject records an event that could not be parsed or exceeded limits. A
	// parse failure is isolated, never swallowed into an empty message.
	Reject      func(context.Context, RejectedEvent) error
	ConfirmCard func(context.Context, core.RuntimeCardCallback) (string, error)
}

type RejectedEvent struct {
	Reason  string
	Detail  string
	Payload string
}

// SendRequest is one delivery attempt. The adapter reports what the platform
// actually confirmed; an unclear outcome must be reported as unknown, never
// optimistically as accepted.
type SendRequest struct {
	ConversationID string
	Transport      string
	Content        string
	Format         string
	IdempotencyKey string
	ReplyTo        string
}
type SendResult struct {
	State   string
	Receipt string
	Detail  string
}

type ReactionRequest struct {
	ConversationID string
	MessageID      string
	Emoji          string
}

// Reactor is optional. A runtime uses it for lightweight progress feedback;
// reaction failure never changes the durable task result.
type Reactor interface {
	AddReaction(context.Context, Config, ReactionRequest) error
	RemoveReaction(context.Context, Config, ReactionRequest) error
}

// Adapter is deliberately narrow. An unsupported method returns a typed
// "unavailable" error instead of quietly switching to another identity.
type Adapter interface {
	Name() string
	ProbeCapabilities(ctx context.Context, cfg Config) (core.Capabilities, error)
	ReadWindow(ctx context.Context, cfg Config, w Window) (WindowResult, error)
	RunReceiver(ctx context.Context, cfg Config, opts ReceiverOptions) error
	Send(ctx context.Context, cfg Config, req SendRequest) (SendResult, error)
}

// Reconciler is optional. An adapter that cannot confirm a past delivery simply
// does not implement it, which keeps an unknown outcome unknown.
type Reconciler interface {
	Reconcile(ctx context.Context, cfg Config, receipt string) (SendResult, error)
}

// OwnerMessageStatusReader only queries a previous current-user send task. It
// must never resend a message while resolving delayed provider message IDs.
type OwnerMessageStatusReader interface {
	ReadOwnerMessageStatus(context.Context, Config, string) (SendResult, error)
}

type GroupConversation struct {
	ID   string
	Name string
}

type BotUser struct {
	ID   string `json:"user_id"`
	Name string `json:"name"`
}

type DirectConversation struct {
	ID          string
	DisplayName string
	Robot       bool
}

// DirectConversationLister discovers direct conversations with activity inside
// an explicit bounded window. It must report incomplete pagination honestly.
type DirectConversationLister interface {
	ListDirectConversations(context.Context, Config, time.Time, time.Time, int, int) ([]DirectConversation, bool, string, error)
}

// GroupLister is implemented by personal-account adapters that can enumerate
// the owner's current groups. The runtime uses it to include newly joined groups
// without removing explicit ignore routes.
type GroupLister interface {
	ListGroupConversations(ctx context.Context, cfg Config) ([]GroupConversation, error)
}

func Unsupported(adapter, capability string) error {
	return core.Fail("unavailable", "%s does not support %s; no other identity is substituted", adapter, capability)
}

// ConfigFor builds the frozen configuration from the stored channel. Only the
// credential reference travels, so a secret never reaches an adapter argument.
func ConfigFor(c core.Channel) Config {
	conversations := make([]string, 0, len(c.Routes))
	direct := make([]string, 0, 1)
	for _, r := range c.Routes {
		if r.Status == "active" && r.Mode != "ignore" {
			conversations = append(conversations, r.ConversationID)
			if r.ConversationType == "direct" {
				direct = append(direct, r.ConversationID)
			}
		}
	}
	return Config{ChannelID: c.ID, ChannelName: c.Name, Kind: c.Kind, Tenant: c.Tenant,
		AuthNamespace: c.AuthNamespace, IDNamespace: c.IDNamespace, Identity: c.Identity,
		CredentialRef: c.CredentialRef, Conversations: conversations, DirectConversations: direct, Tool: c.ToolVersion}
}

// GroupDiscovery separates verified positive groups from evidence that the
// entire scope was searched. Excluded contains only explicitly proven absence.
type GroupDiscovery struct {
	Groups   []GroupConversation
	Complete bool
	Excluded []string
}
type DetailedGroupLister interface {
	DiscoverGroupConversations(context.Context, Config) (GroupDiscovery, error)
}
