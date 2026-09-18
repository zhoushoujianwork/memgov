// Package dws adapts the personal dws CLI. Every call uses argv arrays through
// exec.CommandContext; message text is data and never reaches a shell.
//
// This path does not promise end-to-end loss-free collection: dws receiving a
// cloud event and memgov committing to SQLite are separate processes, and no
// verified upstream ACK control or arbitrary event replay exists. Overlapping
// backfill and an explicit gap record are how that is handled honestly.
package dws

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"github.com/zhoushoujianwork/memgov/internal/processtree"
)

// ParseVersion identifies this normalization. It is stored with every event so a
// later parser change is distinguishable from a platform change.
const ParseVersion = "dws/1"

// Runner executes dws. Tests substitute it; production uses argv exec.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Streamer starts a long-running dws command and exposes both streams, because
// readiness is reported on stderr while events arrive on stdout.
type Streamer func(ctx context.Context, args ...string) (stdout, stderr io.ReadCloser, wait func() error, err error)

type Adapter struct {
	Run    Runner
	Stream Streamer
	// Timeout bounds a single history call. A slow platform must fail visibly
	// rather than hold a receive loop forever.
	Timeout      time.Duration
	directMu     sync.RWMutex
	robotDirect  map[string]bool
	ownerSenders map[string]string
}

// Resolve the authenticated owner's IM sender ID through DWS's stable-ID
// sender filter. Display names never establish which side of a chat is ours.
func (a *Adapter) ownerSender(ctx context.Context, cfg channel.Config, historyArgs []string) (string, error) {
	key := cfg.Identity.Profile + "|" + cfg.Identity.ExpectedUserID
	a.directMu.RLock()
	id := a.ownerSenders[key]
	a.directMu.RUnlock()
	if id != "" {
		return id, nil
	}
	if cfg.Identity.ExpectedUserID == "" {
		return "", core.Fail("denied", "direct history requires a verified owner user ID")
	}
	args := append(append([]string{}, historyArgs...), "--sender", cfg.Identity.ExpectedUserID)
	raw, err := a.run(ctx, withProfile(cfg, args)...)
	if err != nil {
		return "", err
	}
	var page struct {
		ResolvedFilters struct {
			Senders []struct {
				Status   string `json:"status"`
				Selected struct {
					UserID string `json:"userId"`
					OpenID string `json:"openDingTalkId"`
				} `json:"selected"`
			} `json:"senders"`
		} `json:"resolvedFilters"`
	}
	if json.Unmarshal(raw, &page) != nil || len(page.ResolvedFilters.Senders) != 1 {
		return "", core.Fail("unavailable", "DWS did not resolve the owner's stable IM sender identity")
	}
	resolved := page.ResolvedFilters.Senders[0]
	if resolved.Status != "resolved" || resolved.Selected.UserID != cfg.Identity.ExpectedUserID || resolved.Selected.OpenID == "" {
		return "", core.Fail("denied", "DWS owner sender identity was ambiguous or mismatched")
	}
	a.directMu.Lock()
	if a.ownerSenders == nil {
		a.ownerSenders = map[string]string{}
	}
	a.ownerSenders[key] = resolved.Selected.OpenID
	a.directMu.Unlock()
	return resolved.Selected.OpenID, nil
}

func New() *Adapter { return &Adapter{Run: execRun, Stream: execStream, Timeout: 30 * time.Second} }

type directSearchPage struct {
	Messages []struct {
		ConversationID string `json:"conversationId"`
		Sender         string `json:"sender"`
	} `json:"messages"`
	Complete    bool   `json:"complete"`
	HasMore     bool   `json:"hasMore"`
	Partial     bool   `json:"partial"`
	FailedCount int    `json:"failedCount"`
	StopReason  string `json:"stopReason"`
}

func (a *Adapter) directSearch(ctx context.Context, cfg channel.Config, start, end time.Time, pageLimit int, robots bool) (directSearchPage, error) {
	args := []string{"chat", "+search-msg", "--conversation-type", "single", "--start", start.Format(time.RFC3339), "--end", end.Format(time.RFC3339), "--order", "asc", "--limit", "100", "--page-all", "--page-limit", strconv.Itoa(pageLimit), "--no-enrich"}
	if robots {
		args = append(args, "--only-robot")
	}
	raw, err := a.run(ctx, withProfile(cfg, args)...)
	var out directSearchPage
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, core.Fail("unavailable", "dws direct discovery page was unreadable")
	}
	return out, nil
}

// ListDirectConversations discovers conversations from messages inside the
// explicit enable/retention range. A separate robot-only ledger prevents bot
// conversations from entering colleague proactive processing.
func (a *Adapter) ListDirectConversations(ctx context.Context, cfg channel.Config, start, end time.Time, pageLimit, maxItems int) ([]channel.DirectConversation, bool, string, error) {
	if pageLimit <= 0 {
		pageLimit = 5
	}
	if maxItems <= 0 {
		maxItems = 200
	}
	all, err := a.directSearch(ctx, cfg, start, end, pageLimit, false)
	if err != nil {
		return nil, false, "call_failed", err
	}
	robots, robotErr := a.directSearch(ctx, cfg, start, end, 40, true)
	if robotErr != nil {
		return nil, false, "robot_classification_failed", robotErr
	}
	if !robots.Complete || robots.HasMore || robots.Partial || robots.FailedCount > 0 {
		return nil, false, "robot_classification_incomplete", core.Fail("unavailable", "DWS robot direct-message classification was incomplete")
	}
	robotSet := map[string]bool{}
	for _, m := range robots.Messages {
		if m.ConversationID != "" {
			robotSet[m.ConversationID] = true
		}
	}
	a.directMu.Lock()
	a.robotDirect = robotSet
	a.directMu.Unlock()
	seen := map[string]bool{}
	out := []channel.DirectConversation{}
	for _, m := range all.Messages {
		if m.ConversationID == "" || seen[m.ConversationID] || robotSet[m.ConversationID] {
			continue
		}
		seen[m.ConversationID] = true
		out = append(out, channel.DirectConversation{ID: m.ConversationID, DisplayName: m.Sender})
		if len(out) >= maxItems {
			return out, false, "item_cap_reached", nil
		}
	}
	complete := all.Complete && !all.HasMore && !all.Partial && all.FailedCount == 0 && robots.Complete && !robots.HasMore && !robots.Partial && robots.FailedCount == 0
	reason := ""
	if !complete {
		reason = all.StopReason
		if reason == "" {
			reason = "incomplete_direct_discovery"
		}
	}
	return out, complete, reason, nil
}

func (a *Adapter) Name() string { return "dws" }

func execRun(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "dws", args...)
	return processtree.Output(ctx, cmd)
}

func execStream(ctx context.Context, args ...string) (io.ReadCloser, io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, "dws", args...)
	cmd.WaitDelay = 5 * time.Second
	// A graceful stop is required: dws owns subscription state, so the receiver
	// is asked to terminate instead of being killed outright.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return stdout, stderr, cmd.Wait, nil
}

var requestSlots = make(chan struct{}, 2)

func (a *Adapter) run(ctx context.Context, args ...string) (json.RawMessage, error) {
	timeout := a.Timeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case requestSlots <- struct{}{}:
		defer func() { <-requestSlots }()
	case <-ctx.Done():
		return nil, core.Fail("unavailable", "dws request queue timed out")
	}
	b, err := a.Run(ctx, append(args, "--format", "json")...)
	if ctx.Err() != nil {
		return nil, core.Fail("unavailable", "dws call timed out: %v", strings.Join(args, " "))
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		if err != nil {
			return nil, core.Fail("unavailable", "dws call failed: %v", err)
		}
		return nil, core.Fail("unavailable", "dws returned an unrecognized envelope")
	}
	if raw, ok := obj["error"]; ok && string(raw) != "null" {
		var detail struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &detail) != nil {
			return nil, core.Fail("unavailable", "dws returned an unreadable error")
		}
		switch detail.Code {
		case "auth_required", "permission_denied":
			return nil, core.Fail("denied", "dws refused the call: %s", detail.Code)
		default:
			return nil, core.Fail("unavailable", "dws error %s: %s", detail.Code, detail.Message)
		}
	}
	if err != nil {
		return nil, core.Fail("unavailable", "dws call failed: %v", err)
	}
	if raw, ok := obj["success"]; ok {
		var success bool
		if json.Unmarshal(raw, &success) != nil || !success {
			return nil, core.Fail("unavailable", "dws reported failure")
		}
		result, ok := obj["result"]
		if ok && string(result) != "null" {
			return result, nil
		}
		// Newer dws shortcuts keep their projected fields beside `success`
		// instead of nesting them below `result`. Preserve those fields so
		// callers can support both envelope generations.
		delete(obj, "success")
		return json.Marshal(obj)
	}
	// Current dws releases wrap some composite shortcuts in the common
	// {ok,result,identity,tool} envelope. Unwrap it before transport-specific
	// validation so an acknowledged bot send is not mislabeled unknown.
	if raw, exists := obj["ok"]; exists {
		var ok bool
		if json.Unmarshal(raw, &ok) != nil || !ok {
			return nil, core.Fail("unavailable", "dws reported failure")
		}
		if result, present := obj["result"]; present && string(result) != "null" {
			return result, nil
		}
		delete(obj, "ok")
		return json.Marshal(obj)
	}
	return b, nil
}

func withProfile(cfg channel.Config, args []string) []string {
	if cfg.Identity.Profile != "" {
		return append(args, "--profile", cfg.Identity.Profile)
	}
	return args
}

// ProbeCapabilities confirms the logged-in identity matches the configured one
// and records only what was actually observed. A mismatch stops collection
// instead of quietly reading another account's messages.
func (a *Adapter) ProbeCapabilities(ctx context.Context, cfg channel.Config) (core.Capabilities, error) {
	caps := core.Capabilities{Verified: map[string]bool{}, Unverified: []string{}, Tool: "dws"}
	raw, err := a.run(ctx, "profile", "list")
	if err != nil {
		return caps, err
	}
	var profiles struct {
		Current  string `json:"currentProfile"`
		Profiles []struct {
			Profile string `json:"profile"`
			CorpID  string `json:"corpId"`
			UserID  string `json:"userId"`
		} `json:"profiles"`
	}
	if err = json.Unmarshal(raw, &profiles); err != nil {
		return caps, core.Fail("unavailable", "dws profile list was unreadable")
	}
	want := cfg.Identity.Profile
	if want == "" {
		want = profiles.Current
	}
	var found bool
	for _, p := range profiles.Profiles {
		if p.Profile != want {
			continue
		}
		found = true
		if p.CorpID != "" && p.CorpID != cfg.Identity.ExpectedCorpID {
			return caps, core.Fail("denied", "identity_mismatch: dws profile %s belongs to corp %s but the channel expects %s", want, p.CorpID, cfg.Identity.ExpectedCorpID)
		}
		if p.UserID != "" && cfg.Identity.ExpectedUserID != "" && p.UserID != cfg.Identity.ExpectedUserID {
			return caps, core.Fail("denied", "identity_mismatch: dws profile %s is user %s but the channel expects %s", want, p.UserID, cfg.Identity.ExpectedUserID)
		}
	}
	if !found {
		return caps, core.Fail("denied", "identity_mismatch: dws has no logged-in profile %q", want)
	}
	// History and live receive are the two capabilities this probe establishes.
	caps.Verified["history"] = true
	caps.Verified["receive"] = true
	if cfg.Identity.DeliveryRobotCode != "" {
		bots, botErr := a.run(ctx, withProfile(cfg, []string{"chat", "bot", "search", "--page", "1", "--size", "100"})...)
		if botErr == nil && jsonContainsRobotCode(bots, cfg.Identity.DeliveryRobotCode) {
			caps.Verified["send"] = true
		} else if cfg.Identity.ExpectedUserID != "" {
			// +bot-search only lists robots created by the current user. An
			// enterprise application robot can still be valid, so exercise the
			// exact send path in dry-run mode before accepting its explicit code.
			preview, previewErr := a.run(ctx, withProfile(cfg, []string{"chat", "+messages-send", "--as", "bot",
				"--robot-code", cfg.Identity.DeliveryRobotCode, "--users", cfg.Identity.ExpectedUserID,
				"--markdown", "memgov delivery capability probe", "--title", "memgov setup", "--dry-run"})...)
			var dry struct {
				DryRun      bool `json:"dry_run"`
				Executed    bool `json:"executed"`
				ActionCount int  `json:"actionCount"`
			}
			if previewErr == nil && json.Unmarshal(preview, &dry) == nil && dry.DryRun && !dry.Executed && dry.ActionCount > 0 {
				caps.Verified["send"] = true
			} else {
				caps.Unverified = append(caps.Unverified, "send")
			}
		} else {
			caps.Unverified = append(caps.Unverified, "send")
		}
	} else {
		caps.Unverified = append(caps.Unverified, "send")
	}
	caps.Unverified = append(caps.Unverified, "group_history", "recall_events")
	return caps, nil
}

func jsonContainsRobotCode(raw []byte, want string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				if (k == "robotCode" || k == "robot_code") && item == want {
					return true
				}
				if walk(item) {
					return true
				}
			}
		case []any:
			for _, item := range x {
				if walk(item) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

type historyPage struct {
	Messages []struct {
		ID         string `json:"messageId"`
		Text       string `json:"text"`
		Time       string `json:"time"`
		CreateTime string `json:"createTime"`
		SenderID   string `json:"senderId"`
		SenderName string `json:"senderName"`
		SenderType string `json:"senderIdType"`
		QuotedID   string `json:"quotedMessageId"`
	} `json:"messages"`
	Complete        bool                 `json:"complete"`
	HasMore         bool                 `json:"hasMore"`
	NextPage        *historyContinuation `json:"nextPage"`
	StopReason      string               `json:"stopReason"`
	FailedCount     int                  `json:"failedCount"`
	Partial         bool                 `json:"partial"`
	PaginationKnown *bool                `json:"paginationKnown"`
}

// ReadWindow backfills one half-open range. Reaching a page or item cap is
// reported as partial with its stop reason, never as "no more messages".
func (a *Adapter) ReadWindow(ctx context.Context, cfg channel.Config, w channel.Window) (channel.WindowResult, error) {
	out := channel.WindowResult{Events: []core.NormalizedEvent{}}
	if w.ConversationID == "" {
		return out, core.Fail("invalid_input", "history window requires a conversation")
	}
	if !w.End.After(w.Start) {
		return out, core.Fail("invalid_input", "history window must be a non-empty [start,end) range")
	}
	pageLimit, maxItems := w.PageLimit, w.MaxItems
	if pageLimit <= 0 {
		pageLimit = 5
	}
	if maxItems <= 0 {
		maxItems = 200
	}
	requestStart, err := historyRequestStart(w)
	if err != nil {
		return out, err
	}
	targetFlag := "--group"
	if w.ConversationType == "direct" {
		targetFlag = "--conversation-id"
	}
	args := []string{"chat", "+chat-messages", targetFlag, w.ConversationID,
		"--start", requestStart.UTC().Format(time.RFC3339Nano), "--end", w.End.UTC().Format(time.RFC3339Nano),
		"--order", "asc", "--page-all", "--page-limit", strconv.Itoa(pageLimit), "--max-items", strconv.Itoa(maxItems)}
	raw, err := a.run(ctx, withProfile(cfg, args)...)
	if err != nil {
		out.StopReason = "call_failed"
		return out, err
	}
	var page historyPage
	if err = json.Unmarshal(raw, &page); err != nil {
		out.StopReason = "unreadable_page"
		return out, core.Fail("unavailable", "dws history page was unreadable")
	}
	ownerID := ""
	if w.ConversationType == "direct" {
		ownerID, err = a.ownerSender(ctx, cfg, args)
		if err != nil {
			out.StopReason = "owner_identity_unverified"
			return out, err
		}
	}
	out.Pages = 1
	out.StopReason = page.StopReason
	sourceComplete := out.StopReason == "source_complete" || (page.Complete && out.StopReason == "range_end")
	if sourceComplete {
		out.StopReason = ""
	}
	var lastObserved time.Time
	for _, m := range page.Messages {
		if m.ID == "" {
			// A page entry without an identity cannot be stored as a message;
			// the window is partial rather than silently short.
			out.Complete = false
			out.StopReason = "message_without_id"
			return out, nil
		}
		sentAt := m.Time
		if sentAt == "" {
			sentAt = m.CreateTime
		}
		stamp, naiveTime, stampErr := parseHistoryTime(sentAt)
		if stampErr != nil {
			out.StopReason = "invalid_message_time"
			return out, nil
		}
		if stamp.Before(requestStart.Truncate(time.Second)) || stamp.After(w.End) {
			out.StopReason = "message_outside_window"
			return out, nil
		}
		if !lastObserved.IsZero() && stamp.Before(lastObserved) {
			out.StopReason = "unordered_history_page"
			return out, nil
		}
		if stamp.Equal(w.End) {
			continue // DWS can include the exclusive end boundary.
		}
		lastObserved = stamp
		if stamp.Before(w.Start) {
			continue // DWS accepts second-precision ranges; retain the exact enable boundary.
		}
		idType := m.SenderType
		if idType == "" {
			idType = "union_id"
			if w.ConversationType == "direct" {
				idType = "open_id"
			}
		}
		if m.SenderID == "" {
			idType = "unknown"
		}
		e := core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dws", ParseVersion: historyParseVersion,
			SourceTimeRaw: sentAt,
			Origin:        "history", ProviderMessageID: m.ID, ConversationID: w.ConversationID,
			ConversationType: w.ConversationType,
			Sender:           core.Sender{IDType: idType, IDValue: m.SenderID, DisplayName: m.SenderName, SelfAuthor: ownerID != "" && m.SenderID == ownerID},
			Body:             m.Text, SentAt: stamp.Format(time.RFC3339Nano), EventAt: stamp.Format(time.RFC3339Nano)}
		if naiveTime {
			e.HistoryPreviousSentAt = stamp.Add(8 * time.Hour).Format(time.RFC3339Nano)
		}
		if m.QuotedID != "" {
			e.Relations = []core.Relation{{Kind: "quote", ProviderMessageID: m.QuotedID, Confidence: "provider"}}
		}
		out.Events = append(out.Events, e)
	}
	if page.Partial || page.FailedCount > 0 || (page.PaginationKnown != nil && !*page.PaginationKnown) {
		out.StopReason = "unverified_pagination"
		return out, nil
	}
	// Completeness is only claimed when dws said the range was fully read and
	// nothing remains. A cap or a stop reason keeps the window partial.
	out.Truncated = page.HasMore || len(page.Messages) >= maxItems
	out.Complete = (page.Complete || sourceComplete) && !page.HasMore && out.StopReason == ""
	if !out.Complete && out.StopReason == "" {
		if out.Truncated {
			out.StopReason = "page_or_item_cap_reached"
		} else {
			out.StopReason = "incomplete_without_reason"
		}
	}
	if !out.Complete {
		var reason string
		out.NextCursor, reason = historyContinuationCursor(w, requestStart, lastObserved, page.NextPage)
		if reason != "" {
			out.StopReason = reason
		}
	}
	return out, nil
}

func normalizeTime(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	// Epoch milliseconds appear in some dws payloads.
	if ms, err := strconv.ParseInt(value, 10, 64); err == nil && ms > 0 {
		return time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	return ""
}

// eventEnvelope is the dws NDJSON event line. The business payload arrives as a
// JSON string, so it is parsed separately and never trusted as instructions.
type eventEnvelope struct {
	Type              string `json:"type"`
	EventType         string `json:"event_type"`
	EventID           string `json:"event_id"`
	EventCorpID       string `json:"event_corp_id"`
	EventUnifiedAppID string `json:"event_unified_app_id"`
	EventBornTime     any    `json:"event_born_time"`
	Timestamp         any    `json:"timestamp"`
	EventScope        string `json:"event_scope"`
	Data              string `json:"data"`
	ReceivedAtUnixMs  int64  `json:"received_at_unix_ms"`
	SubscribeID       string `json:"subscribe_id"`
	Seq               int64  `json:"seq"`
	SourceID          string `json:"source_id"`
	MessageID         string `json:"message_id"`
	ConversationID    string `json:"conversation_id"`
	Sender            string `json:"sender"`
	SenderOpenID      string `json:"sender_open_dingtalk_id"`
	SenderStaffID     string `json:"sender_staff_id"`
	Content           string `json:"content"`
	CreateTime        string `json:"create_time"`
}

type messagePayload struct {
	MsgID            string                   `json:"msgId"`
	ConversationID   string                   `json:"conversationId"`
	ConversationType string                   `json:"conversationType"`
	SenderID         string                   `json:"senderId"`
	SenderStaffID    string                   `json:"senderStaffId"`
	SenderNick       string                   `json:"senderNick"`
	MsgType          string                   `json:"msgtype"`
	CreateAt         any                      `json:"createAt"`
	Text             struct{ Content string } `json:"text"`
	Content          struct{ Content string } `json:"content"`
	OriginalMsgID    string                   `json:"originalMsgId"`
	RecalledMsgID    string                   `json:"recalledMsgId"`
	IsInAtList       bool                     `json:"isInAtList"`
}

// maxEventBytes bounds one NDJSON line. An oversized line is isolated rather
// than parsed, so a hostile or broken producer cannot exhaust memory.
const maxEventBytes = 1 << 20

// ParseEvent normalizes one dws NDJSON line. A parse failure is returned as an
// error so the caller isolates it instead of storing an empty message.
func ParseEvent(cfg channel.Config, line []byte) (core.NormalizedEvent, error) {
	var env eventEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return core.NormalizedEvent{}, core.Fail("invalid_input", "event line is not JSON")
	}
	kind := env.EventType
	if kind == "" {
		kind = env.Type
	}
	if kind == "" {
		return core.NormalizedEvent{}, core.Fail("invalid_input", "event line has no type")
	}
	if env.EventCorpID != "" && env.EventCorpID != cfg.Tenant {
		return core.NormalizedEvent{}, core.Fail("denied", "event belongs to corp %s, not %s", env.EventCorpID, cfg.Tenant)
	}
	var payload messagePayload
	if env.Data != "" {
		if err := json.Unmarshal([]byte(env.Data), &payload); err != nil {
			return core.NormalizedEvent{}, core.Fail("invalid_input", "event data is not JSON")
		}
	} else {
		// +listen-im projects common message fields at the top level. Recorded
		// replays and older dws releases still use the JSON-encoded data field.
		payload.MsgID = env.MessageID
		payload.ConversationID = env.ConversationID
		payload.SenderID = env.SenderOpenID
		payload.SenderStaffID = env.SenderStaffID
		payload.SenderNick = env.Sender
		payload.Content.Content = env.Content
		payload.CreateAt = env.CreateTime
	}
	e := core.NormalizedEvent{Adapter: "dws", ParseVersion: ParseVersion, Origin: "stream",
		ProviderEventID: env.EventID, ConversationID: payload.ConversationID,
		EventAt: eventTime(firstNonNil(env.EventBornTime, env.Timestamp), env.ReceivedAtUnixMs), Mentioned: payload.IsInAtList,
		Ordering: strconv.FormatInt(env.Seq, 10)}
	switch {
	case strings.Contains(kind, "message_recall"):
		e.Kind = core.EventRecall
		e.ProviderMessageID = firstNonEmpty(payload.RecalledMsgID, payload.MsgID, payload.OriginalMsgID)
		e.RecalledAt = e.EventAt
	case strings.Contains(kind, "message_receive"):
		e.Kind = core.EventMessage
		e.ProviderMessageID = payload.MsgID
		e.Body = firstNonEmpty(payload.Text.Content, payload.Content.Content)
		e.Format = "text"
		e.SentAt = eventTime(payload.CreateAt, 0)
		e.SourceTimeRaw = core.JSON(payload.CreateAt)
		if e.SentAt == "" {
			e.SentAt = e.EventAt
		}
		idType := "staff_id"
		value := payload.SenderStaffID
		if value == "" {
			idType, value = "union_id", payload.SenderID
		}
		if value == "" {
			idType = "unknown"
		}
		e.Sender = core.Sender{IDType: idType, IDValue: value, DisplayName: payload.SenderNick}
		if payload.OriginalMsgID != "" {
			e.Relations = []core.Relation{{Kind: "quote", ProviderMessageID: payload.OriginalMsgID, Confidence: "provider"}}
		}
	default:
		// Reactions, read receipts and unknown versions are isolated rather than
		// coerced into a message shape.
		return core.NormalizedEvent{}, core.Fail("invalid_input", "unsupported dws event type %q", kind)
	}
	if e.ProviderMessageID == "" || e.ConversationID == "" {
		return core.NormalizedEvent{}, core.Fail("invalid_input", "event lacks a message or conversation identity")
	}
	e.Payload = string(line)
	return e, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
func eventTime(value any, fallbackMs int64) string {
	switch v := value.(type) {
	case string:
		if t := normalizeTime(v); t != "" {
			return t
		}
	case float64:
		if v > 0 {
			return time.UnixMilli(int64(v)).UTC().Format(time.RFC3339)
		}
	case json.Number:
		if ms, err := v.Int64(); err == nil && ms > 0 {
			return time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
	}
	if fallbackMs > 0 {
		return time.UnixMilli(fallbackMs).UTC().Format(time.RFC3339)
	}
	return ""
}

// readyMarker is the line dws prints on stderr when the event bus is live.
// Readiness is never inferred from a timer.
const readyMarker = "[event] ready"

// RunReceiver consumes the dws event stream. Both streams are read
// concurrently, and readiness is reported only after dws printed its own ready
// marker, so a caller never claims to be listening before it is.
func (a *Adapter) RunReceiver(ctx context.Context, cfg channel.Config, opts channel.ReceiverOptions) error {
	if opts.Handle == nil {
		return core.Fail("invalid_input", "receiver requires a handler")
	}
	var callbackMu sync.Mutex
	safe := channel.ReceiverOptions{
		Ready: func(detail map[string]any) {
			callbackMu.Lock()
			defer callbackMu.Unlock()
			if opts.Ready != nil {
				opts.Ready(detail)
			}
		},
		Handle: func(ctx context.Context, event core.NormalizedEvent) error {
			callbackMu.Lock()
			defer callbackMu.Unlock()
			return opts.Handle(ctx, event)
		},
		Reject: func(ctx context.Context, rejected channel.RejectedEvent) error {
			callbackMu.Lock()
			defer callbackMu.Unlock()
			if opts.Reject == nil {
				return nil
			}
			return opts.Reject(ctx, rejected)
		},
	}
	if cfg.CollectAllDirect && cfg.CollectGroups {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var readyMu sync.Mutex
		readyKinds := map[string]bool{}
		merged := safe
		merged.Ready = func(detail map[string]any) {
			kind, _ := detail["kind"].(string)
			readyMu.Lock()
			readyKinds[kind] = true
			both := readyKinds["all-group"] && readyKinds["all-direct"]
			readyMu.Unlock()
			if both && safe.Ready != nil {
				safe.Ready(map[string]any{"marker": readyMarker, "group": true, "direct": true})
			}
		}
		errs := make(chan error, 2)
		go func() { errs <- a.runReceiverKind(ctx, cfg, merged, "all-group") }()
		go func() { errs <- a.runReceiverKind(ctx, cfg, merged, "all-direct") }()
		err := <-errs
		cancel()
		<-errs
		return err
	}
	if cfg.CollectAllDirect {
		return a.runReceiverKind(ctx, cfg, safe, "all-direct")
	}
	if len(cfg.DirectConversations) == 0 {
		return a.runReceiverKind(ctx, cfg, safe, "all-group")
	}
	if len(cfg.Conversations) == len(cfg.DirectConversations) {
		return a.runReceiverKind(ctx, cfg, safe, "all-direct")
	}
	// The installed DWS consume contract rejects a mixed user/group EventKey
	// command. Two independent consumers contend for the same personal stream.
	// Fail before opening a subscription rather than retrying a false promise.
	return core.Fail("invalid_input", "DWS cannot subscribe to group and direct IM together; collect groups in the source and owner private callbacks through the bound application bot")
}

func (a *Adapter) runReceiverKind(ctx context.Context, cfg channel.Config, opts channel.ReceiverOptions, kind string) error {
	// dws exposes one enterprise-wide group subscription. Route matching below
	// keeps only configured groups and applies the local ignore blacklist.
	args := []string{"event", "+listen-im", "--kind", kind, "--events", "message", "--format", "json"}
	wrapped := opts
	wrapped.Ready = func(detail map[string]any) {
		detail["kind"] = kind
		if opts.Ready != nil {
			opts.Ready(detail)
		}
	}
	return a.runReceiverArgs(ctx, cfg, wrapped, kind, args)
}

func (a *Adapter) runReceiverArgs(ctx context.Context, cfg channel.Config, opts channel.ReceiverOptions, kind string, args []string) error {
	stdout, stderr, wait, err := a.Stream(ctx, withProfile(cfg, args)...)
	if err != nil {
		return core.Fail("unavailable", "could not start the dws receiver: %v", err)
	}
	defer stdout.Close()
	defer stderr.Close()
	var once sync.Once
	ready := func(detail map[string]any) {
		once.Do(func() {
			if opts.Ready != nil {
				opts.Ready(detail)
			}
		})
	}
	var wg sync.WaitGroup
	// stderr carries readiness and diagnostics. It must be drained or dws will
	// block on a full pipe, which would look like a silent stall.
	wg.Go(func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, readyMarker) {
				ready(parseReady(line))
			}
		}
		// A stderr read failure is diagnostic only; the stdout loop decides the
		// outcome, so it is not turned into a receive error here.
		_ = scanner.Err()
	})
	handleErr := func() error {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			copied := append([]byte(nil), line...)
			e, err := ParseEvent(cfg, copied)
			if err != nil {
				if err := reject(ctx, opts, core.ErrorCode(err), err.Error(), string(copied)); err != nil {
					return err
				}
				continue
			}
			if kind == "all-direct" {
				e.ConversationType = "direct"
				a.directMu.RLock()
				robot := a.robotDirect[e.ConversationID]
				a.directMu.RUnlock()
				if robot {
					continue
				}
			}
			if !containsConversation(cfg.Conversations, e.ConversationID) {
				if kind == "all-direct" && cfg.CollectAllDirect {
					// Unknown direct conversations wait for the bounded DWS search
					// to classify them as human rather than robot. Its next backfill
					// captures the message without opening a reply loop.
					continue
				} else {
					// Direct subscriptions identify the real IM conversation, while the
					// delivery route addresses the owner by userId. Bind only a message
					// whose verified sender is that owner; never route another person's
					// private chat into the unattended owner lane.
					if (kind != "all-direct" && kind != "combined") || len(cfg.DirectConversations) != 1 || e.Sender.IDValue != cfg.Identity.ExpectedUserID {
						continue
					}
					e.ConversationID = cfg.DirectConversations[0]
				}
			}
			// A handler failure means the event was not persisted. The loop stops
			// so the caller can recover by watermark rather than losing the event.
			if err := opts.Handle(ctx, e); err != nil {
				return err
			}
		}
		err := scanner.Err()
		switch {
		case err == nil, errors.Is(err, io.ErrClosedPipe):
			return nil
		case errors.Is(err, bufio.ErrTooLong):
			// An oversized line is isolated rather than parsed. The stream cannot
			// be resynchronized after it, so receiving stops with a clear reason.
			if err := reject(ctx, opts, "oversized_event", "event line exceeded the size limit", ""); err != nil {
				return err
			}
			return core.Fail("unavailable", "dws sent an event larger than the %d byte limit; the stream cannot be resynchronized", maxEventBytes)
		default:
			return core.Fail("unavailable", "dws event stream failed: %v", err)
		}
	}()
	wg.Wait()
	waitErr := wait()
	if handleErr != nil {
		return handleErr
	}
	if ctx.Err() != nil {
		return nil
	}
	if waitErr != nil {
		return core.Fail("unavailable", "dws receiver exited: %v", waitErr)
	}
	return nil
}

func containsConversation(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func reject(ctx context.Context, opts channel.ReceiverOptions, reason, detail, payload string) error {
	if opts.Reject == nil {
		return nil
	}
	return opts.Reject(ctx, channel.RejectedEvent{Reason: reason, Detail: detail, Payload: payload})
}

// parseReady extracts the counters dws reports with its ready marker so the
// caller can record which subscription actually became live.
func parseReady(line string) map[string]any {
	detail := map[string]any{"marker": readyMarker, "line": line}
	for _, field := range strings.Fields(line) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(value); err == nil {
			detail[key] = n
			continue
		}
		detail[key] = value
	}
	return detail
}

func (a *Adapter) Send(ctx context.Context, cfg channel.Config, req channel.SendRequest) (channel.SendResult, error) {
	if req.Transport == "user_group" || req.Transport == "user_dm" {
		return a.sendOwnerMessage(ctx, cfg, req)
	}
	if req.Transport != "bot_dm" {
		return channel.SendResult{State: "blocked"}, channel.Unsupported("dws adapter", "delivery transport "+req.Transport)
	}
	if cfg.Identity.DeliveryRobotCode == "" || req.ConversationID == "" {
		return channel.SendResult{State: "blocked"}, core.Fail("invalid_input", "bot delivery requires a verified robot code and owner user ID")
	}
	args := []string{"chat", "+messages-send", "--as", "bot", "--robot-code", cfg.Identity.DeliveryRobotCode, "--users", req.ConversationID, "--markdown", req.Content, "--title", "memgov AI 值守", "--ai-tag", "true", "--yes"}
	raw, err := a.run(ctx, withProfile(cfg, args)...)
	if err != nil {
		return channel.SendResult{State: "unknown"}, err
	}
	var ledger struct {
		Succeeded int  `json:"succeededCount"`
		Failed    int  `json:"failedCount"`
		Success   bool `json:"success"`
		Result    struct {
			ProcessQueryKey string   `json:"processQueryKey"`
			InvalidStaffIDs []string `json:"invalidStaffIdList"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &ledger) != nil {
		return channel.SendResult{State: "unknown", Detail: "platform result did not match the delivery ledger"}, nil
	}
	if ledger.Failed > 0 {
		return channel.SendResult{State: "failed", Receipt: string(raw), Detail: "platform reported a failed target"}, nil
	}
	if ledger.Success && ledger.Result.ProcessQueryKey != "" && len(ledger.Result.InvalidStaffIDs) == 0 {
		return channel.SendResult{State: "accepted", Receipt: string(raw)}, nil
	}
	if ledger.Succeeded < 1 {
		return channel.SendResult{State: "unknown", Receipt: string(raw), Detail: "platform did not confirm a successful target"}, nil
	}
	return channel.SendResult{State: "accepted", Receipt: string(raw)}, nil
}

func (a *Adapter) AddReaction(ctx context.Context, cfg channel.Config, req channel.ReactionRequest) error {
	return a.reaction(ctx, cfg, req, true)
}

func (a *Adapter) RemoveReaction(ctx context.Context, cfg channel.Config, req channel.ReactionRequest) error {
	return a.reaction(ctx, cfg, req, false)
}

func (a *Adapter) reaction(ctx context.Context, cfg channel.Config, req channel.ReactionRequest, add bool) error {
	if req.ConversationID == "" || req.MessageID == "" || strings.TrimSpace(req.Emoji) == "" {
		return core.Fail("invalid_input", "reaction requires conversation, message and emoji")
	}
	action := "add-emoji"
	if !add {
		action = "remove-emoji"
	}
	_, err := a.run(ctx, withProfile(cfg, []string{"chat", "message", action,
		"--conversation-id", req.ConversationID, "--message-id", req.MessageID, "--emoji", req.Emoji})...)
	return err
}

var _ channel.Reactor = (*Adapter)(nil)

var _ channel.Adapter = (*Adapter)(nil)
