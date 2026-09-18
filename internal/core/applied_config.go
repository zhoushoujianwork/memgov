package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

// ManagedConfigObject binds a declaration name to an internal object. Snapshot
// is a digest of the applied object, used to detect subsequent manual drift.
type ManagedConfigObject struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	ObjectType string `json:"object_type"`
	ObjectID   string `json:"object_id"`
	Snapshot   string `json:"snapshot,omitempty"`
}
type AppliedConfig struct {
	ID            string                `json:"id"`
	Version       int                   `json:"version"`
	SchemaVersion int                   `json:"schema_version"`
	Declaration   json.RawMessage       `json:"declaration"`
	Objects       []ManagedConfigObject `json:"objects"`
	Digest        string                `json:"digest"`
	CreatedAt     string                `json:"created_at"`
}

// ReadAppliedConfig returns version zero when no declaration has been applied.
// Historical snapshots are audit data; callers must not use them to authorize
// operations that current route or runtime policies no longer permit.
func ReadAppliedConfig(ctx context.Context, q Queryer, version int) (AppliedConfig, error) {
	var c AppliedConfig
	query := "SELECT id,version,schema_version,declaration,objects,digest,created_at FROM applied_configs"
	args := []any{}
	if version > 0 {
		query += " WHERE version=?"
		args = append(args, version)
	}
	query += " ORDER BY version DESC LIMIT 1"
	var declaration, objects string
	err := q.QueryRowContext(ctx, query, args...).Scan(&c.ID, &c.Version, &c.SchemaVersion, &declaration, &objects, &c.Digest, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if version > 0 {
			return c, Fail("not_found", "applied configuration version not found")
		}
		c.Objects = []ManagedConfigObject{}
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.Declaration = json.RawMessage(declaration)
	return c, json.Unmarshal([]byte(objects), &c.Objects)
}

func (tx *Tx) CommitAppliedConfig(ctx context.Context, expected, schemaVersion int, declaration json.RawMessage, objects []ManagedConfigObject) (AppliedConfig, error) {
	current, err := ReadAppliedConfig(ctx, tx.Conn, 0)
	if err != nil {
		return current, err
	}
	if expected != current.Version {
		return current, Fail("conflict", "expected applied configuration version %d, current %d", expected, current.Version)
	}
	if schemaVersion < 1 {
		return current, Fail("invalid_input", "configuration schema version must be positive")
	}
	var value map[string]any
	if err = json.Unmarshal(declaration, &value); err != nil || value == nil {
		return current, Fail("invalid_input", "configuration declaration must be a JSON object")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return current, err
	}
	objects = append([]ManagedConfigObject{}, objects...)
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Kind != objects[j].Kind {
			return objects[i].Kind < objects[j].Kind
		}
		return objects[i].Name < objects[j].Name
	})
	seen := map[string]bool{}
	for _, o := range objects {
		key := o.Kind + "\x00" + o.Name
		if o.Kind == "" || o.Name == "" || o.ObjectType == "" || o.ObjectID == "" || seen[key] {
			return current, Fail("invalid_input", "invalid or duplicate managed configuration object")
		}
		seen[key] = true
	}
	digest := Digest(map[string]any{"schema_version": schemaVersion, "declaration": value, "objects": objects})
	if digest == current.Digest {
		return current, nil
	}
	next := AppliedConfig{ID: NewID(), Version: current.Version + 1, SchemaVersion: schemaVersion, Declaration: canonical, Objects: objects, Digest: digest, CreatedAt: Now()}
	_, err = tx.Conn.ExecContext(ctx, "INSERT INTO applied_configs(id,version,schema_version,declaration,objects,digest,created_at) VALUES(?,?,?,?,?,?,?)", next.ID, next.Version, next.SchemaVersion, string(next.Declaration), JSON(next.Objects), next.Digest, next.CreatedAt)
	if err != nil {
		return current, err
	}
	_, err = tx.Audit(ctx, "config.apply", "applied validated configuration", objectChange("applied_config", next.ID))
	return next, err
}
