package knowledge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func schema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func field(kind string) map[string]any { return map[string]any{"type": kind} }

var sourceTools = map[string][]mcpTool{
	"dokki": {
		{"list_dokki_workspaces", "List workspaces readable by the configured Dokki key.", schema(map[string]any{"limit": field("integer")})},
		{"search_dokki", "Search Dokki narrowly; read relevant resources and cite actual source_url.", schema(map[string]any{"query": field("string"), "mode": field("string"), "workspace_ids": map[string]any{"type": "array", "items": field("string")}, "limit": field("integer")}, "query")},
		{"read_dokki_resource", "Read a Dokki resource by its search result ID.", schema(map[string]any{"resource_id": field("string")}, "resource_id")},
	},
	"confluence": {
		{"list_confluence_tools", "List currently advertised and approved Confluence read tools and their schemas.", schema(map[string]any{})},
		{"search_confluence", "Search Confluence pages through read-only REST; read relevant pages and cite actual source_url.", schema(map[string]any{"query": field("string"), "query_mode": field("string"), "limit": field("integer"), "start": field("integer")}, "query")},
		{"read_confluence_page", "Read a Confluence page by numeric ID through read-only REST, including its body and source URL.", schema(map[string]any{"id": field("string")}, "id")},
		{"query_confluence", "Call an approved advanced Confluence read tool with its advertised arguments; use search_confluence for page search.", schema(map[string]any{"tool": field("string"), "arguments": field("object")}, "tool", "arguments")},
		{"read_confluence_resource", "Read an export resource URI returned by Confluence.", schema(map[string]any{"uri": field("string")}, "uri")},
	},
}

func toolSource(name string) string {
	for source, tools := range sourceTools {
		for _, tool := range tools {
			if tool.Name == name {
				return source
			}
		}
	}
	return ""
}

func decodeArguments(raw json.RawMessage, dest any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return fmt.Errorf("invalid tool arguments")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid tool arguments")
	}
	return nil
}

func callTool(ctx context.Context, c *Client, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "list_dokki_workspaces":
		var a struct {
			Limit int `json:"limit"`
		}
		if err := decodeArguments(raw, &a); err != nil {
			return nil, err
		}
		if a.Limit == 0 {
			a.Limit = 50
		}
		return c.ListDokkiWorkspaces(ctx, a.Limit)
	case "search_dokki":
		var a struct {
			Query        string   `json:"query"`
			Mode         string   `json:"mode"`
			WorkspaceIDs []string `json:"workspace_ids"`
			Limit        int      `json:"limit"`
		}
		if err := decodeArguments(raw, &a); err != nil {
			return nil, err
		}
		if a.Mode == "" {
			a.Mode = "hybrid"
		}
		if a.Limit == 0 {
			a.Limit = 10
		}
		return c.SearchDokki(ctx, a.Query, a.Mode, a.WorkspaceIDs, a.Limit)
	case "read_dokki_resource":
		var a struct {
			ResourceID string `json:"resource_id"`
		}
		if err := decodeArguments(raw, &a); err != nil {
			return nil, err
		}
		return c.ReadDokkiResource(ctx, a.ResourceID)
	case "list_confluence_tools":
		var a struct{}
		if err := decodeArguments(raw, &a); err != nil {
			return nil, err
		}
		return c.ListConfluenceTools(ctx)
	case "search_confluence":
		var a struct {
			Query string `json:"query"`
			Mode  string `json:"query_mode"`
			Limit int    `json:"limit"`
			Start int    `json:"start"`
		}
		if err := decodeArguments(raw, &a); err != nil {
			return nil, err
		}
		if a.Mode == "" {
			a.Mode = "keyword"
		}
		if a.Limit == 0 {
			a.Limit = 10
		}
		return c.SearchConfluence(ctx, a.Query, a.Mode, a.Limit, a.Start)
	case "read_confluence_page":
		var a struct {
			ID string `json:"id"`
		}
		if err := decodeArguments(raw, &a); err != nil {
			return nil, err
		}
		return c.ReadConfluencePage(ctx, a.ID)
	case "query_confluence":
		var a struct {
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := decodeArguments(raw, &a); err != nil {
			return nil, err
		}
		return c.QueryConfluence(ctx, a.Tool, a.Arguments)
	case "read_confluence_resource":
		var a struct {
			URI string `json:"uri"`
		}
		if err := decodeArguments(raw, &a); err != nil {
			return nil, err
		}
		return c.ReadConfluenceResource(ctx, a.URI)
	}
	return nil, fmt.Errorf("unknown knowledge tool")
}

// ServeMCP exposes only selected read tools over newline-delimited MCP stdio.
// It has no memory, filesystem, messaging, or write tool surface.
func ServeMCP(ctx context.Context, home string, sources []string, in io.Reader, out io.Writer) error {
	selected := map[string]bool{}
	for _, source := range sources {
		if sourceTools[source] == nil || selected[source] {
			return fmt.Errorf("invalid or duplicate knowledge source")
		}
		selected[source] = true
	}
	if len(selected) == 0 {
		return fmt.Errorf("at least one knowledge source is required")
	}
	reader := bufio.NewScanner(in)
	reader.Buffer(make([]byte, 4096), 1<<20)
	writer := json.NewEncoder(out)
	for reader.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(reader.Bytes(), &request) != nil || request.JSONRPC != "2.0" {
			continue
		}
		if len(request.ID) == 0 || string(request.ID) == "null" {
			continue
		}
		var result any
		var rpcError any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "memgov-knowledge", "version": "1.0.0"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			tools := []mcpTool{}
			for _, source := range []string{"dokki", "confluence"} {
				if selected[source] {
					tools = append(tools, sourceTools[source]...)
				}
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(request.Params, &params) != nil || !selected[toolSource(params.Name)] {
				rpcError = map[string]any{"code": -32602, "message": "tool is not enabled"}
				break
			}
			credentials, err := Load(home)
			if err != nil {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "knowledge credentials unavailable"}}}
				break
			}
			value, err := callTool(ctx, NewClient(credentials), params.Name, params.Arguments)
			if err != nil {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}
				break
			}
			raw, _ := json.Marshal(value)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(raw)}}}
			if structured, ok := object(value); ok {
				result.(map[string]any)["structuredContent"] = structured
			}
		default:
			rpcError = map[string]any{"code": -32601, "message": "method not found"}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		if rpcError != nil {
			response["error"] = rpcError
		} else {
			response["result"] = result
		}
		if err := writer.Encode(response); err != nil {
			return err
		}
	}
	if err := reader.Err(); err != nil {
		return fmt.Errorf("knowledge MCP input failed")
	}
	return nil
}

func ToolNames(sources []string) []string {
	out := []string{}
	for _, source := range sources {
		for _, tool := range sourceTools[strings.TrimSpace(source)] {
			out = append(out, tool.Name)
		}
	}
	return out
}
