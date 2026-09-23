package core

import (
	"context"
	"encoding/json"
	"strings"
)

// The old declaration remains in the sealed pre-upgrade archive. The current
// snapshot must not revive removed knowledge settings on the next config plan.
func stripLegacyAppliedMemoryConfig(ctx context.Context, q Queryer) error {
	// Preserve manual drift: rebase only snapshots matching the old runtime.
	runtimeRows, err := q.QueryContext(ctx, "SELECT id FROM runtime_configs")
	if err != nil {
		return err
	}
	var runtimeIDs []string
	for runtimeRows.Next() {
		var id string
		if err = runtimeRows.Scan(&id); err != nil {
			runtimeRows.Close()
			return err
		}
		runtimeIDs = append(runtimeIDs, id)
	}
	err = runtimeRows.Err()
	runtimeRows.Close()
	if err != nil {
		return err
	}
	rebases := map[string][2]string{}
	for _, id := range runtimeIDs {
		r, err := ReadRuntime(ctx, q, id)
		if err != nil {
			return err
		}
		old := RuntimeManagedSnapshot(r)
		old["memory_scope"] = r.MemoryScope
		if r.ReviewTimeoutSeconds != 120 {
			old["review_timeout_seconds"] = r.ReviewTimeoutSeconds
		}
		oldDigest := Digest(old)
		caps := []string{}
		for _, c := range r.AgentCapabilities {
			if c != "memory_read" {
				caps = append(caps, c)
			}
		}
		r.AgentCapabilities = caps
		r.MemoryScope = ""
		if _, err = q.ExecContext(ctx, "UPDATE runtime_configs SET memory_scope='',agent_capabilities=? WHERE id=?", JSON(caps), id); err != nil {
			return err
		}
		rebases["runtime\x00"+id] = [2]string{oldDigest, Digest(RuntimeManagedSnapshot(r))}
	}
	channels, err := ChannelList(ctx, q)
	if err != nil {
		return err
	}
	for _, channel := range channels {
		for _, route := range channel.Routes {
			snapshot := RouteManagedSnapshot(route)
			next := Digest(snapshot)
			snapshot["memory_policy"] = route.MemoryPolicy
			rebases["route\x00"+route.ID] = [2]string{Digest(snapshot), next}
		}
	}
	if _, err = q.ExecContext(ctx, "UPDATE channel_routes SET memory_policy=''"); err != nil {
		return err
	}
	if err = removeLegacyKnowledgeCaches(ctx, q); err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, "SELECT id,schema_version,declaration,objects FROM applied_configs")
	if err != nil {
		return err
	}
	type item struct {
		id           string
		version      int
		raw, objects string
	}
	var items []item
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.version, &i.raw, &i.objects); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, i := range items {
		var declaration map[string]any
		var objects []ManagedConfigObject
		if err = json.Unmarshal([]byte(i.raw), &declaration); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(i.objects), &objects); err != nil {
			return err
		}
		var declaredChannels []ChannelInput
		if rawChannels, ok := declaration["channels"]; ok {
			if err = json.Unmarshal([]byte(JSON(rawChannels)), &declaredChannels); err != nil {
				return err
			}
		}
		for n, o := range objects {
			if r, ok := rebases[o.ObjectType+"\x00"+o.ObjectID]; ok && o.Snapshot == r[0] {
				objects[n].Snapshot = r[1]
			}
			if o.ObjectType == "channel" {
				for _, channel := range channels {
					if channel.ID != o.ObjectID {
						continue
					}
					for _, input := range declaredChannels {
						if input.Name == o.Name && o.Snapshot == Digest(channelManagedSnapshot(channel, input, true)) {
							objects[n].Snapshot = ChannelManagedSnapshot(channel, input)
						}
					}
				}
			}
		}
		stripLegacyDeclaration(declaration)
		digest := Digest(map[string]any{"schema_version": i.version, "declaration": declaration, "objects": objects})
		if _, err = q.ExecContext(ctx, "UPDATE applied_configs SET declaration=?,objects=?,digest=? WHERE id=?", JSON(declaration), JSON(objects), digest, i.id); err != nil {
			return err
		}
	}
	return nil
}

// Retain operational idempotency keys: discarding delivery or message keys can
// turn a retry into a duplicate side effect. Retired knowledge caches are only
// historical payloads and their commands no longer exist.
func removeLegacyKnowledgeCaches(ctx context.Context, q Queryer) error {
	rows, err := q.QueryContext(ctx, "SELECT rowid,command FROM idempotency")
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var command string
		if err = rows.Scan(&id, &command); err != nil {
			rows.Close()
			return err
		}
		command = strings.TrimPrefix(strings.ReplaceAll(command, " ", "."), "memgov.")
		root, _, _ := strings.Cut(command, ".")
		if contains([]string{"source", "candidate", "memory", "review", "audience", "recall", "search", "reindex", "hotword", "job", "migrate", "migration", "purge", "plan", "consolidate", "conflict", "relation"}, root) || strings.HasPrefix(command, "runtime.review.") || strings.HasPrefix(command, "runtime.memory.") || command == "context.recall" {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = q.ExecContext(ctx, "DELETE FROM idempotency WHERE rowid=?", id); err != nil {
			return err
		}
	}
	return nil
}

func stripLegacyDeclaration(declaration map[string]any) {
	if agents, ok := declaration["agents"].(map[string]any); ok {
		for _, value := range agents {
			agent, _ := value.(map[string]any)
			delete(agent, "home")
			delete(agent, "memory_scope")
			if caps, ok := agent["capabilities"].([]any); ok {
				clean := []any{}
				for _, capability := range caps {
					if capability != "memory_read" {
						clean = append(clean, capability)
					}
				}
				agent["capabilities"] = clean
			}
		}
	}
	cleanGroup := func(value any) {
		if group, ok := value.(map[string]any); ok {
			delete(group, "shared_memory_workspaces")
			delete(group, "excluded_memory_categories")
		}
	}
	if apps, ok := declaration["applications"].(map[string]any); ok {
		if proactive, ok := apps["proactive"].(map[string]any); ok {
			delete(proactive, "review_timeout_seconds")
		}
		cleanGroup(apps["group_mention"])
		if bots, ok := apps["bots"].(map[string]any); ok {
			for _, value := range bots {
				if bot, ok := value.(map[string]any); ok {
					cleanGroup(bot["group_mention"])
				}
			}
		}
	}
	if channels, ok := declaration["channels"].([]any); ok {
		for _, value := range channels {
			if channel, ok := value.(map[string]any); ok {
				if route, ok := channel["route"].(map[string]any); ok {
					delete(route, "memory_policy")
				}
			}
		}
	}
}
