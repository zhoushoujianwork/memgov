package cli

import (
	"bytes"
	"context"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceStartupFailureReportsCause(t *testing.T) {
	for _, command := range []string{"start", "run"} {
		t.Run(command, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Run(context.Background(), []string{"--home", t.TempDir(), "service", command, "--no-ui"}, strings.NewReader(""), &out, &stderr)
			if code == 0 || !strings.Contains(out.String(), "database not initialized") || strings.Contains(out.String(), "stream_failed") {
				t.Fatalf("startup failure lost cause: %d %s %s", code, &out, &stderr)
			}
		})
	}
}

func TestUnifiedReceiverAndDisabledDeclaration(t *testing.T) {
	home, r := dualOwnerPrivateFixture(t)
	ctx := context.Background()
	s, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "test.shared.group"}, func(tx *core.Tx) (any, error) {
		bot, e := core.ReadChannel(ctx, tx.Conn, r.ChannelID)
		if e != nil {
			return nil, e
		}
		if _, e = tx.AddRoute(ctx, bot.Identity.HistoryChannel, core.RouteInput{ConversationID: "group1", ConversationType: "group", Mode: "collect"}); e != nil {
			return nil, e
		}
		route, e := tx.AddRoute(ctx, bot.ID, core.RouteInput{ConversationID: "group1", ConversationType: "group", Mode: "assistant", Triggers: []string{"mention"}, SendPolicy: "reply_to_trigger"})
		if e != nil {
			return nil, e
		}
		return tx.ConfigureRuntime(ctx, core.RuntimeConfigInput{Name: "group-mention", Channel: bot.ID, RouteIDs: []string{route.ID}, DeliveryRouteID: route.ID, Owner: core.Sender{IDType: "user_id", IDValue: "owner1"}, ApplicationMode: "group_mention"})
	})
	if err != nil {
		t.Fatal(err)
	}

	a := &app{home: home}
	specs, err := a.unifiedSpecs(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("expected private/group agents and one channel receiver: %+v", specs)
	}
	// Automatic route/version updates must not restart the worker.
	epoch := ""
	for _, spec := range specs {
		if spec.Key == "agent:"+r.ID {
			epoch = spec.Epoch
		}
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE runtime_configs SET version=version+1 WHERE id=?", r.ID); err != nil {
		t.Fatal(err)
	}
	specs, err = a.unifiedSpecs(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		if spec.Key == "agent:"+r.ID && spec.Epoch != epoch {
			t.Fatal("route discovery restarted agent")
		}
	}
	_, err = s.Mutate(ctx, core.Request{Scope: "global", Command: "test.service.disabled"}, func(tx *core.Tx) (any, error) {
		return tx.CommitAppliedConfig(ctx, 0, 1, []byte(`{"applications":{"owner_private":{"enabled":false}}}`), []core.ManagedConfigObject{{Kind: "application", Name: "owner_private", ObjectType: "runtime", ObjectID: r.ID}})
	})
	if err != nil {
		t.Fatal(err)
	}
	specs, err = a.unifiedSpecs(ctx, s)
	if err != nil || len(specs) != 0 {
		t.Fatal(specs, err)
	}
}
func TestServiceStatusDoesNotInitializeHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	code, result := invoke(t, home, "", "service", "status")
	if code != 0 || data(t, result)["state"] != "stopped" {
		t.Fatal(result)
	}
}
func TestOwnerPrivateRuntimeBindingDefault(t *testing.T) {
	a := &app{}
	if err := a.loadConfig([]byte("applications:\n  owner_private: {enabled: true}\n")); err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeDualModeConfig(a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Declaration.Applications.OwnerPrivate.Runtime != "owner-private" {
		t.Fatal(normalized)
	}
}
