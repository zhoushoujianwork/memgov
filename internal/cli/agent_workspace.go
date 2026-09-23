package cli

import (
	"context"
	"errors"
	"slices"

	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/agentworkspace"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func workspaceError(err error) error {
	if err == nil {
		return nil
	}
	for _, item := range []struct {
		target error
		code   string
	}{{agentworkspace.ErrNotFound, "not_found"}, {agentworkspace.ErrInvalid, "denied"}, {agentworkspace.ErrTooLarge, "invalid_input"}, {agentworkspace.ErrConflict, "conflict"}, {agentworkspace.ErrBusy, "unavailable"}} {
		if errors.Is(err, item.target) {
			return core.Fail(item.code, "%s", err)
		}
	}
	return err
}

// boundWorkspace resolves identity from persisted execution state, never from
// workspace selectors or sender identifiers supplied by an Agent.
func boundWorkspace(ctx context.Context, q core.Queryer, taskID, attemptID string) (agentworkspace.Ref, error) {
	t, err := core.ReadRuntimeTask(ctx, q, taskID)
	if err != nil {
		return agentworkspace.Ref{}, err
	}
	if t.Status != "running" {
		return agentworkspace.Ref{}, core.Fail("conflict", "task is no longer running")
	}
	c, err := core.ReadRuntime(ctx, q, t.RuntimeID)
	if err != nil {
		return agentworkspace.Ref{}, err
	}
	if c.Status != "running" {
		return agentworkspace.Ref{}, core.Fail("denied", "runtime is not running")
	}
	if err = core.RuntimeAttemptPolicyCurrent(ctx, q, attemptID, t.ID, t.Version); err != nil {
		return agentworkspace.Ref{}, err
	}
	current, err := core.RuntimeTaskOutputCurrent(ctx, q, t.ID)
	if err != nil {
		return agentworkspace.Ref{}, err
	}
	if !current {
		return agentworkspace.Ref{}, core.Fail("denied", "task evidence is no longer current")
	}
	policy, err := core.ResolveRuntimeTaskAgent(ctx, q, c, t)
	if err != nil {
		return agentworkspace.Ref{}, err
	}
	var inputDigest string
	if err = q.QueryRowContext(ctx, "SELECT input_digest FROM runtime_attempts WHERE id=? AND task_id=?", attemptID, t.ID).Scan(&inputDigest); err != nil {
		return agentworkspace.Ref{}, err
	}
	if inputDigest != core.Digest(map[string]any{"title": t.Title, "instructions": t.Instructions, "version": t.Version, "agent_policy": policy}) {
		return agentworkspace.Ref{}, core.Fail("conflict", "Agent policy changed since this attempt started")
	}
	route, err := core.ReadRoute(ctx, q, t.RouteID)
	if err != nil {
		return agentworkspace.Ref{}, err
	}
	if c.ApplicationMode == "group_mention" {
		if route.ConversationType != "group" || !slices.Contains(c.RouteIDs, route.ID) {
			return agentworkspace.Ref{}, core.Fail("denied", "group route is no longer in the runtime audience")
		}
		return agentworkspace.GroupRef(c.ChannelID, route.ConversationID)
	}
	if c.ApplicationMode != "direct" && c.ApplicationMode != "proactive" {
		return agentworkspace.Ref{}, core.Fail("denied", "workspace requires a verified Owner or group runtime")
	}
	if c.ApplicationMode == "direct" {
		ok, e := core.RuntimeDirectTurnCurrent(ctx, q, t.ID)
		if e != nil {
			return agentworkspace.Ref{}, e
		}
		if !ok {
			return agentworkspace.Ref{}, core.Fail("conflict", "private session was cleared")
		}
	}
	channel, err := core.ReadChannel(ctx, q, c.ChannelID)
	if err != nil {
		return agentworkspace.Ref{}, err
	}
	var verified int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM identity_aliases WHERE tenant=? AND id_type=? AND id_value=? AND principal_id=? AND verified=1`, channel.Tenant, c.OwnerIDType, c.OwnerIDValue, c.OwnerPrincipalID).Scan(&verified); err != nil {
		return agentworkspace.Ref{}, err
	}
	if verified != 1 {
		return agentworkspace.Ref{}, core.Fail("denied", "Owner identity is no longer verified")
	}
	return agentworkspace.OwnerRef(c.OwnerPrincipalID)
}

func (a *app) agentWorkspaceCommand() *cobra.Command {
	root := &cobra.Command{Use: "workspace", Short: "Read and maintain durable Agent knowledge files"}
	var id, task, attempt, file, query string
	var limit int
	root.PersistentFlags().StringVar(&id, "workspace-id", "", "Existing Agent workspace ID (local operator only)")
	root.PersistentFlags().StringVar(&task, "task", "", "Bind access to a running task")
	root.PersistentFlags().StringVar(&attempt, "attempt", "", "Current task attempt")
	resolve := func(ctx context.Context, q core.Queryer) (string, error) {
		if task != "" || attempt != "" {
			if task == "" || attempt == "" || id != "" {
				return "", core.Fail("invalid_input", "use task and attempt together, without workspace-id")
			}
			ref, err := boundWorkspace(ctx, q, task, attempt)
			return ref.ID, err
		}
		return id, nil
	}
	for _, op := range []string{"list", "read", "search", "history"} {
		op := op
		cmd := a.simple(op, "Read scoped Workspace files", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
			var workspaceID string
			var err error
			var store *core.Store
			if task != "" || attempt != "" {
				s, e := core.Open(ctx, a.dbPath(), false)
				if e != nil {
					return nil, e
				}
				defer s.Close()
				store = s
				workspaceID, err = resolve(ctx, s.DB)
			} else {
				workspaceID = id
			}
			if err != nil {
				return nil, err
			}
			var out any
			switch op {
			case "list":
				if workspaceID == "" {
					out, err = agentworkspace.List(a.home)
				} else {
					out, err = agentworkspace.Files(a.home, workspaceID)
				}
			case "read":
				out, err = agentworkspace.Read(a.home, workspaceID, file)
			case "search":
				out, err = agentworkspace.Search(a.home, workspaceID, query, limit)
			case "history":
				out, err = agentworkspace.History(a.home, workspaceID, file)
			}
			// A revocation during a potentially long search must also fence its
			// output; discard data if the attempt no longer has access.
			if err == nil && store != nil {
				if current, checkErr := resolve(ctx, store.DB); checkErr != nil {
					return nil, checkErr
				} else if current != workspaceID {
					return nil, core.Fail("conflict", "workspace binding changed")
				}
			}
			return out, workspaceError(err)
		})
		if op == "read" || op == "history" {
			cmd.Flags().StringVar(&file, "path", "", "Relative Markdown file path")
		}
		if op == "search" {
			cmd.Flags().StringVar(&query, "query", "", "Literal case-insensitive query")
			cmd.Flags().IntVar(&limit, "limit", 30, "Maximum matching files (1..100)")
		}
		root.AddCommand(cmd)
	}
	root.AddCommand(a.simple("write", "Write a file using its expected digest", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		if a.input == "" {
			a.input = "-"
		}
		raw, err := a.payload()
		if err != nil {
			return nil, err
		}
		var in struct {
			Path           string  `json:"path"`
			Content        string  `json:"content"`
			ExpectedDigest *string `json:"expected_digest"`
		}
		if err = decode(raw, &in); err != nil {
			return nil, err
		}
		if in.ExpectedDigest == nil {
			return nil, core.Fail("invalid_input", "expected_digest is required; empty means create-only")
		}
		s, err := core.Open(ctx, a.dbPath(), false)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		workspaceID, err := resolve(ctx, s.DB)
		if err != nil {
			return nil, err
		}
		if workspaceID == "" {
			return nil, core.Fail("invalid_input", "select a workspace or bind a task")
		}
		actor := a.actor
		if task != "" {
			actor = "runtime:" + task + ":" + attempt
		}
		metadata := map[string]any{"workspace_id": workspaceID, "path": in.Path, "expected_digest": *in.ExpectedDigest, "digest": core.Hash([]byte(in.Content)), "task_id": task, "attempt_id": attempt}
		result, err := s.Mutate(ctx, core.Request{ID: a.requestID, Command: "agent.workspace.write", Scope: workspaceID, Actor: actor, Key: a.key, Input: metadata}, func(tx *core.Tx) (any, error) {
			check := func() error {
				current, e := resolve(ctx, tx.Conn)
				if e != nil {
					return e
				}
				if current != workspaceID {
					return core.Fail("conflict", "workspace binding changed")
				}
				return nil
			}
			doc, e := agentworkspace.WriteChecked(a.home, workspaceID, in.Path, in.Content, *in.ExpectedDigest, actor, a.requestID, check)
			if e != nil {
				return nil, workspaceError(e)
			}
			_, e = tx.Audit(ctx, "agent.workspace.write", core.JSON(metadata), []core.Change{{ObjectType: "agent_workspace_file", ObjectID: workspaceID + ":" + in.Path}})
			// Only metadata is retained in SQLite, even when idempotency is requested.
			return doc.FileInfo, e
		})
		if err != nil {
			return nil, err
		}
		// Mutate may return a cached response without entering the callback.
		// Re-check after waiting for its database lock, including that path.
		if task != "" {
			current, checkErr := resolve(ctx, s.DB)
			if checkErr != nil {
				return nil, checkErr
			}
			if current != workspaceID {
				return nil, core.Fail("conflict", "workspace binding changed")
			}
		}
		return result, nil
	}))
	return root
}
