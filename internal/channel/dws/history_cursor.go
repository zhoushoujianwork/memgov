package dws

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// The installed DWS v1.0.61 (50eb73a0) parses timezone-less message times
// using fixed CST+8 and exposes nextCursor as epoch milliseconds. See upstream
// internal/shortcut/smart/{chat_messages,message_time_range}.go.
const historyParseVersion = "dws-history/3"

var dwsMessageLocation = time.FixedZone("CST", 8*60*60)

type historyContinuation struct {
	Direction  string `json:"direction"`
	NextCursor int64  `json:"nextCursor"`
	Time       string `json:"time"`
}

type historyTimeCursor struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	ConversationID string `json:"conversation_id"`
	StartAt        string `json:"start_at"`
	EndAt          string `json:"end_at"`
	ResumeAt       string `json:"resume_at"`
}

func historyRequestStart(w channel.Window) (time.Time, error) {
	if w.Cursor == "" {
		return w.Start, nil
	}
	var cursor historyTimeCursor
	if len(w.Cursor) > 4096 || json.Unmarshal([]byte(w.Cursor), &cursor) != nil || cursor.SchemaVersion != 2 || cursor.Kind != "dws_history_time" || cursor.ConversationID != w.ConversationID || cursor.StartAt != w.Start.UTC().Format(time.RFC3339Nano) || cursor.EndAt != w.End.UTC().Format(time.RFC3339Nano) {
		return time.Time{}, core.Fail("invalid_input", "history cursor is incompatible or does not match the fixed window; explicitly retry legacy imports")
	}
	resume, err := time.Parse(time.RFC3339Nano, cursor.ResumeAt)
	if err != nil || resume.Before(w.Start) || !resume.Before(w.End) {
		return time.Time{}, core.Fail("invalid_input", "history cursor is outside the fixed window")
	}
	return resume, nil
}

func historyContinuationCursor(w channel.Window, requestStart, lastObserved time.Time, next *historyContinuation) (string, string) {
	if next == nil {
		return "", "missing_time_cursor"
	}
	stamp, err := time.Parse(time.RFC3339Nano, next.Time)
	if err != nil || next.Direction != "newer" || next.NextCursor <= 0 || !stamp.Equal(time.UnixMilli(next.NextCursor)) || lastObserved.IsZero() {
		return "", "invalid_time_cursor"
	}
	if !stamp.After(requestStart) {
		return "", "cursor_not_advancing"
	}
	if !stamp.Before(w.End) || stamp.Before(lastObserved.Truncate(time.Second)) {
		return "", "cursor_outside_window"
	}
	encoded, _ := json.Marshal(historyTimeCursor{SchemaVersion: 2, Kind: "dws_history_time", ConversationID: w.ConversationID, StartAt: w.Start.UTC().Format(time.RFC3339Nano), EndAt: w.End.UTC().Format(time.RFC3339Nano), ResumeAt: stamp.UTC().Format(time.RFC3339Nano)})
	return string(encoded), ""
}

func parseHistoryTime(value string) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC(), false, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, value, dwsMessageLocation); err == nil {
			return t.UTC(), true, nil
		}
	}
	if epoch, err := strconv.ParseInt(value, 10, 64); err == nil && epoch > 0 {
		if epoch < 1_000_000_000_000 {
			return time.Unix(epoch, 0).UTC(), false, nil
		}
		return time.UnixMilli(epoch).UTC(), false, nil
	}
	return time.Time{}, false, core.Fail("invalid_input", "DWS history message has an invalid time")
}
