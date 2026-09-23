package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestGroupExecutionTimeoutNormalization(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, tc := range []struct {
			value string
			want  int
		}{
			{"", 900}, {"1", 1}, {"3600", 3600}, {"86400", 86400},
			{"0", 0}, {"-1", 0}, {"86401", 0}, {"\"3600\"", 0}, {"3.5", 0},
		} {
			t.Run(fmt.Sprintf("bot=%t/value=%s", nested, tc.value), func(t *testing.T) {
				group := "enabled: false"
				if tc.value != "" {
					group += ", execution_timeout_seconds: " + tc.value
				}
				body := "applications:\n  group_mention: {" + group + "}\n"
				if nested {
					body = "applications:\n  bots:\n    app-main:\n      group_mention: {" + group + "}\n"
				}
				a := &app{}
				err := a.loadConfig([]byte(body))
				var normalized DualModeValidation
				if err == nil {
					normalized, err = NormalizeDualModeConfig(a.cfg)
				}
				if tc.want == 0 {
					if err == nil {
						t.Fatalf("invalid execution timeout %q accepted", tc.value)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				g := normalized.Declaration.Applications.GroupMention
				if nested {
					g = normalized.Declaration.Applications.Bots["app-main"].GroupMention
				}
				if g.ExecutionTimeoutSeconds == nil || *g.ExecutionTimeoutSeconds != tc.want {
					t.Fatalf("timeout = %v, want %d", g.ExecutionTimeoutSeconds, tc.want)
				}
				if g.ExecutionConcurrency == nil || *g.ExecutionConcurrency != 4 {
					t.Fatalf("timeout altered default concurrency: %v", g.ExecutionConcurrency)
				}
			})
		}
	}
}

func TestDualApplyGroupExecutionTimeoutReapplyAndLease(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("bot=%t", nested), func(t *testing.T) {
			home, dws, bot := groupApplyFixture(t)
			ctx := context.Background()
			runtimeName := "group-mention"
			if nested {
				runtimeName = botRuntimeName("app-main", "group")
			}
			config := func(timeout string) string {
				group := "enabled: true, source: work_chat, default_agent: group-helper"
				if timeout != "" {
					group += ", execution_timeout_seconds: " + timeout
				}
				application := "  group_mention: {channel: app-main, " + group + "}\n"
				if nested {
					application = "  bots:\n    app-main:\n      group_mention: {" + group + "}\n"
				}
				return configFile(t, "data_sources:\n  work_chat:\n    channel: dws-main\n    groups:\n      member_robot: app-main\nagents:\n  group-helper:\n    preset: claude-default\n    capabilities: [conversation_history_read, local_test]\n    bash: true\n    external_actions: owner_confirmation\napplications:\n"+application)
			}
			apply := func(cfg string, expectedVersion, wantVersion int) {
				t.Helper()
				code, result := invoke(t, home, "", "--config", cfg, "config", "plan")
				if code != 0 {
					t.Fatal(result)
				}
				plan := data(t, result)
				if expectedVersion != 0 && plan["ready"] != true {
					t.Fatalf("group timeout config blocked: %v", plan)
				}
				args := []string{"--config", cfg, "config", "apply-runtime", "--plan-digest", plan["plan_digest"].(string), "--expected-version", strconv.Itoa(expectedVersion)}
				if expectedVersion == 0 {
					args = append(args, "--authorize-expansion", "--reason", "enable group execution for timeout regression")
				}
				code, result = invoke(t, home, "", args...)
				if code != 0 {
					t.Fatal(result)
				}
				if got := data(t, result)["version"]; got != float64(wantVersion) {
					t.Fatalf("applied version = %v, want %d", got, wantVersion)
				}
			}
			readRuntime := func() core.RuntimeConfig {
				t.Helper()
				store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				cfg, err := core.ReadRuntime(ctx, store.DB, runtimeName)
				if err != nil {
					t.Fatal(err)
				}
				return cfg
			}
			apply(config(""), 0, 1)
			initial := readRuntime()
			if initial.ExecutionTimeoutSeconds != 900 {
				t.Fatalf("default execution timeout = %d", initial.ExecutionTimeoutSeconds)
			}
			if code, result := invoke(t, home, "", "runtime", "stop", runtimeName); code != 0 {
				t.Fatal(result)
			}
			custom := config("3600")
			apply(custom, 1, 2)
			updated := readRuntime()
			if updated.ID != initial.ID || updated.ExecutionTimeoutSeconds != 3600 || updated.Concurrency != 4 {
				t.Fatalf("timeout reapply lost runtime identity or concurrency: %+v", updated)
			}
			if updated.AgentPreset != initial.AgentPreset || updated.AgentPreset != "claude-default" || !updated.AgentBash || updated.ExternalActions != "owner_confirmation" || !reflect.DeepEqual(updated.AgentCapabilities, initial.AgentCapabilities) || updated.OwnerPrincipalID != initial.OwnerPrincipalID || updated.ContextChannelID != dws.ID || updated.ChannelID != bot.ID || !reflect.DeepEqual(updated.RouteIDs, initial.RouteIDs) {
				t.Fatalf("timeout reapply changed group identity or Agent policy: %+v", updated)
			}
			apply(custom, 2, 2)
			if got := readRuntime(); got.Version != updated.Version || got.ExecutionTimeoutSeconds != 3600 {
				t.Fatalf("idempotent reapply changed runtime: %+v", got)
			}

			store, err := core.Open(ctx, filepath.Join(home, "state.db"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var message core.IntakeResult
			_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.group.timeout.intake"}, func(tx *core.Tx) (any, error) {
				if _, err := tx.SetRuntimeStatus(ctx, updated.ID, "running", ""); err != nil {
					return nil, err
				}
				var err error
				message, err = tx.Intake(ctx, bot.ID, core.NormalizedEvent{Kind: core.EventMessage, Adapter: "dingtalk_app", ParseVersion: "1", Origin: "stream", ProviderMessageID: "long-running-request", ConversationID: "cid:work", ConversationType: "group", Tenant: bot.Tenant, Sender: core.Sender{IDType: "staff_id", IDValue: "alice"}, Body: "run the longer job", Mentioned: true, SentAt: core.Now(), EventAt: core.Now()})
				return message, err
			})
			if err != nil {
				t.Fatal(err)
			}
			var attempt core.RuntimeAttempt
			_, err = store.Mutate(ctx, core.Request{Scope: "global", Command: "test.group.timeout.claim"}, func(tx *core.Tx) (any, error) {
				if _, err := tx.SyncRuntimeMessages(ctx, updated.ID); err != nil {
					return nil, err
				}
				batch, err := tx.ClaimRuntimeBatch(ctx, updated.ID, time.Now())
				if err != nil {
					return nil, err
				}
				if batch.ID == "" {
					return nil, fmt.Errorf("new group request was not admitted")
				}
				tasks, err := tx.CompleteRuntimeBatch(ctx, batch, core.RuntimeAnalysis{Decisions: []core.RuntimeDecision{{Kind: "task", CanonicalKey: "mention:" + message.MessageID, Title: "long job", Instructions: "run the longer job", MessageIDs: []string{message.MessageID}}}})
				if err != nil {
					return nil, err
				}
				if len(tasks) != 1 {
					return nil, fmt.Errorf("created %d tasks, want 1", len(tasks))
				}
				claimed, execution, err := tx.ClaimRuntimeTask(ctx, updated.ID, "", "fake", updated.AgentPreset, "test-commit", t.TempDir())
				if err == nil && claimed.ID != tasks[0].ID {
					return nil, fmt.Errorf("group request was not claimed")
				}
				attempt = execution
				return claimed, err
			})
			if err != nil {
				t.Fatal(err)
			}
			var deadline, heartbeat, leaseUntil, kind string
			if err := store.DB.QueryRowContext(ctx, "SELECT deadline_at,heartbeat_at,lease_until,kind FROM runtime_work_leases WHERE id=?", attempt.ID).Scan(&deadline, &heartbeat, &leaseUntil, &kind); err != nil {
				t.Fatal(err)
			}
			deadlineAt, err := time.Parse(time.RFC3339Nano, deadline)
			if err != nil {
				t.Fatal(err)
			}
			startedAt, err := time.Parse(time.RFC3339Nano, heartbeat)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "execution" || deadlineAt.Sub(startedAt) != time.Hour || leaseUntil != deadline || attempt.AppliedConfigVersion != 2 {
				t.Fatalf("execution lease did not use applied one-hour timeout: kind=%s deadline=%s heartbeat=%s lease_until=%s policy=%d", kind, deadline, heartbeat, leaseUntil, attempt.AppliedConfigVersion)
			}
		})
	}
}
