package console

import (
	"errors"
	"net/http"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/agentworkspace"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

// The local owner's console can inspect workspaces, but cannot modify them.
// The package validates workspace IDs and file paths before touching the disk.
func workspaceQuery(home string, r *http.Request) (result any, err error) {
	defer func() {
		switch {
		case errors.Is(err, agentworkspace.ErrNotFound):
			err = core.Fail("not_found", "workspace or file not found")
		case errors.Is(err, agentworkspace.ErrInvalid):
			err = core.Fail("invalid_input", "invalid workspace or file path")
		case errors.Is(err, agentworkspace.ErrTooLarge):
			err = core.Fail("invalid_input", "workspace query exceeds its size limit")
		case errors.Is(err, agentworkspace.ErrConflict):
			err = core.Fail("conflict", "workspace changed; refresh and try again")
		}
	}()
	if r.URL.Path == "/api/v1/agent-workspaces" {
		return agentworkspace.List(home)
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/agent-workspaces/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		return nil, core.Fail("not_found", "workspace query not found")
	}
	id := parts[0]
	switch parts[1] {
	case "files":
		query := strings.TrimSpace(r.URL.Query().Get("query"))
		if len([]rune(query)) > 500 {
			return nil, core.Fail("invalid_input", "search exceeds 500 characters")
		}
		if query != "" {
			return agentworkspace.Search(home, id, query, 100)
		}
		return agentworkspace.Files(home, id)
	case "file":
		return agentworkspace.Read(home, id, r.URL.Query().Get("path"))
	case "history":
		return agentworkspace.History(home, id, r.URL.Query().Get("path"))
	default:
		return nil, core.Fail("not_found", "workspace query not found")
	}
}
