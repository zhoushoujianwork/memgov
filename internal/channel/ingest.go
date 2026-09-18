package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// ProbeResult records what a capability probe actually established. It is stored
// on the channel so later runs refuse work the probe did not confirm.
type ProbeResult struct {
	Channel      string            `json:"channel"`
	Adapter      string            `json:"adapter"`
	Capabilities core.Capabilities `json:"capabilities"`
	Note         string            `json:"note"`
}

// Probe asks the adapter what it can actually do and stores the answer. It is
// the only place a capability becomes verified, and it contacts the platform.
func (c Collector) Probe(ctx context.Context, req core.Request, channelValue string) (ProbeResult, error) {
	out := ProbeResult{Adapter: c.Adapter.Name()}
	stored, err := core.ReadChannel(ctx, c.Store.DB, channelValue)
	if err != nil {
		return out, err
	}
	out.Channel = stored.Name
	caps, err := c.Adapter.ProbeCapabilities(ctx, ConfigFor(stored))
	if err != nil {
		return out, err
	}
	scoped := req
	scoped.Command = "channel.probe"
	// A configuration change deliberately clears capabilities and requires a
	// fresh platform check. Include the configuration version so an earlier
	// successful probe cannot be replayed from the idempotency cache.
	scoped.Key = "probe|" + stored.ID + "|config:" + fmt.Sprint(stored.ConfigVersion) + "|" + core.Hash([]byte(core.JSON(caps)))
	if _, err = c.Store.Mutate(ctx, scoped, func(tx *core.Tx) (any, error) {
		return tx.SetChannelCapabilities(ctx, stored.ID, caps, c.Adapter.Name())
	}); err != nil {
		return out, err
	}
	out.Capabilities = caps
	out.Note = "only verified capabilities are usable; an unverified capability is treated as absent"
	return out, nil
}

// IngestResult reports an offline replay. Rejected lines are counted and kept
// rather than skipped, so a malformed export is visible.
type IngestResult struct {
	Channel    string   `json:"channel"`
	Lines      int      `json:"lines"`
	Applied    int      `json:"applied"`
	Duplicates int      `json:"duplicates"`
	Rejected   int      `json:"rejected"`
	Reasons    []string `json:"reasons,omitempty"`
	Note       string   `json:"note"`
}

// Ingest replays normalized events from a local NDJSON file. It never contacts a
// platform, and an imported event may not claim an unverified online identity,
// which the intake path enforces.
func (c Collector) Ingest(ctx context.Context, req core.Request, channelValue string, r io.Reader) (IngestResult, error) {
	out := IngestResult{Reasons: []string{}}
	stored, err := core.ReadChannel(ctx, c.Store.DB, channelValue)
	if err != nil {
		return out, err
	}
	out.Channel = stored.Name
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		out.Lines++
		var e core.NormalizedEvent
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			out.Rejected++
			out.Reasons = append(out.Reasons, "line is not a normalized event: "+err.Error())
			continue
		}
		// An import is labelled as such so it can never be mistaken for a live
		// platform observation.
		e.Origin = "import"
		result, err := c.intake(ctx, req, stored.Name, e)
		if err != nil {
			out.Rejected++
			out.Reasons = append(out.Reasons, core.ErrorCode(err)+": "+err.Error())
			continue
		}
		if result.Duplicate {
			out.Duplicates++
			continue
		}
		out.Applied++
	}
	if err := scanner.Err(); err != nil {
		return out, core.Fail("invalid_input", "could not read the event file: %v", err)
	}
	out.Note = "imported events are recorded with origin=import and do not advance coverage"
	return out, nil
}
