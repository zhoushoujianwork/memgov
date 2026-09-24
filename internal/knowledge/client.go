package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	dokkiAPI       = "https://dokki.one/api/v1"
	dokkiWeb       = "https://dokki.one"
	confluenceAPI  = "https://mcp-dock.patsnap.info/mcp/confluence"
	confluenceREST = "https://confluence.zhihuiya.com/rest/api"
	maxResponse    = 16 << 20
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)
var exportURI = regexp.MustCompile(`^confluence://page-export-pdf/[0-9]{1,40}$`)
var numericPageID = regexp.MustCompile(`^[0-9]{1,40}$`)

var confluenceReadTools = map[string]bool{
	"confluence_content_get": true, "confluence_content_list": true,
	"confluence_content_tree": true, "confluence_comment_list": true, "confluence_label_list": true,
	"confluence_attachment_list": true, "confluence_attachment_download": true,
	"confluence_property_get": true, "confluence_restriction_get": true,
	"confluence_space_get": true, "confluence_personal_space_get": true,
	"confluence_user_get": true, "confluence_group_get": true, "confluence_system_get": true,
	"confluence_export": true, "confluence_body_transform": true,
	"confluence_space_permission_get": true, "confluence_template_get": true, "confluence_audit_get": true,
}

type Client struct {
	Credentials       Credentials
	HTTP              *http.Client
	DokkiURL          string
	ConfluenceURL     string
	ConfluenceRESTURL string
}

func NewClient(c Credentials) *Client {
	return &Client{Credentials: c, HTTP: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, DokkiURL: dokkiAPI, ConfluenceURL: confluenceAPI, ConfluenceRESTURL: confluenceREST}
}

func (c *Client) secrets() []string {
	return []string{c.Credentials.Dokki.APIKey, c.Credentials.Confluence.DockToken, c.Credentials.Confluence.PAT}
}

func scrub(value any, secrets []string, depth int) any {
	if depth > 100 {
		return "[response too deep]"
	}
	switch v := value.(type) {
	case string:
		for _, secret := range secrets {
			if secret != "" {
				v = strings.ReplaceAll(v, secret, "[redacted]")
				v = strings.ReplaceAll(v, url.QueryEscape(secret), "[redacted]")
			}
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = scrub(item, secrets, depth+1)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[scrub(k, secrets, depth+1).(string)] = scrub(item, secrets, depth+1)
		}
		return out
	default:
		return value
	}
}

func (c *Client) request(ctx context.Context, method, endpoint string, body any, headers map[string]string) (any, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("invalid request")
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return nil, fmt.Errorf("invalid request")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("knowledge source connection failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("knowledge source returned HTTP %d", res.StatusCode)
	}
	if res.ContentLength > maxResponse {
		return nil, fmt.Errorf("knowledge source response exceeds 16 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("knowledge source response failed")
	}
	if len(raw) > maxResponse {
		return nil, fmt.Errorf("knowledge source response exceeds 16 MiB")
	}
	if strings.Contains(res.Header.Get("content-type"), "text/event-stream") {
		return parseSSE(raw)
	}
	var result any
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("knowledge source returned invalid JSON")
	}
	return result, nil
}

func parseSSE(raw []byte) (any, error) {
	for _, event := range regexp.MustCompile(`\r?\n\r?\n`).Split(string(raw), -1) {
		parts := []string{}
		for _, line := range strings.Split(event, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.HasPrefix(line, "data:") {
				parts = append(parts, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(parts) == 0 || parts[0] == "[DONE]" {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(strings.Join(parts, "\n")), &value); err != nil {
			return nil, fmt.Errorf("knowledge source returned invalid event")
		}
		return value, nil
	}
	return nil, fmt.Errorf("knowledge source returned no result")
}

func object(value any) (map[string]any, bool)      { v, ok := value.(map[string]any); return v, ok }
func stringAt(v map[string]any, key string) string { s, _ := v[key].(string); return s }

func (c *Client) dokki(ctx context.Context, method, path string, body any) (map[string]any, error) {
	key := c.Credentials.Dokki.APIKey
	if key == "" {
		return nil, fmt.Errorf("Dokki is not configured")
	}
	if !validSecret(key) {
		return nil, fmt.Errorf("Dokki credential is invalid")
	}
	v, err := c.request(ctx, method, c.DokkiURL+path, body, map[string]string{"Authorization": "Bearer " + key, "Accept": "application/json", "Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	v = scrub(v, c.secrets(), 0)
	out, ok := object(v)
	if !ok {
		return nil, fmt.Errorf("Dokki response format is invalid")
	}
	return out, nil
}

func (c *Client) ListDokkiWorkspaces(ctx context.Context, limit int) (any, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("limit must be 1..100")
	}
	v, err := c.dokki(ctx, "GET", fmt.Sprintf("/workspaces?limit=%d", limit), nil)
	if err != nil {
		return nil, err
	}
	rows, ok := v["workspaces"].([]any)
	if !ok || len(rows) > limit {
		return nil, fmt.Errorf("Dokki workspace response is invalid")
	}
	return v, nil
}

func dokkiURL(kind, id string) string {
	routes := map[string]string{"document": "doc", "file": "file", "table": "table", "artifact": "artifact", "folder": "folder"}
	if route := routes[strings.ToLower(kind)]; route != "" {
		return dokkiWeb + "/" + route + "/" + url.PathEscape(id)
	}
	return ""
}

var htmlTags = regexp.MustCompile(`<[^>]*>`)
var htmlActive = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)\s*>`)
var htmlSpace = regexp.MustCompile(`\s+`)

func cleanHighlight(value string) string {
	if len(value) > 32768 {
		value = value[:32768]
	}
	value = html.UnescapeString(value)
	value = htmlActive.ReplaceAllString(value, " ")
	value = htmlTags.ReplaceAllString(value, " ")
	value = htmlSpace.ReplaceAllString(value, " ")
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 1200 {
		runes = runes[:1200]
	}
	return string(runes)
}

func (c *Client) SearchDokki(ctx context.Context, query, mode string, workspaces []string, limit int) (any, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 2000 || limit < 1 || limit > 20 || mode != "keyword" && mode != "semantic" && mode != "hybrid" || len(workspaces) > 50 {
		return nil, fmt.Errorf("invalid Dokki search parameters")
	}
	for _, id := range workspaces {
		if !identifier.MatchString(id) {
			return nil, fmt.Errorf("invalid Dokki workspace ID")
		}
	}
	body := map[string]any{"query": query, "mode": mode, "limit": limit}
	if workspaces != nil {
		body["workspace_ids"] = workspaces
	}
	v, err := c.dokki(ctx, "POST", "/search", body)
	if err != nil {
		return nil, err
	}
	rows, ok := v["results"].([]any)
	if !ok || len(rows) > limit {
		return nil, fmt.Errorf("Dokki search response is invalid")
	}
	for _, item := range rows {
		row, ok := object(item)
		if !ok {
			return nil, fmt.Errorf("Dokki search hit is invalid")
		}
		id := stringAt(row, "resource_id")
		if !identifier.MatchString(id) || stringAt(row, "title") == "" {
			return nil, fmt.Errorf("Dokki search hit is invalid")
		}
		if source := dokkiURL(stringAt(row, "type"), id); source != "" {
			row["source_url"] = source
		}
		if highlight, exists := row["highlight"].(string); exists {
			row["highlight"] = cleanHighlight(highlight)
		}
	}
	return v, nil
}

func (c *Client) ReadDokkiResource(ctx context.Context, id string) (any, error) {
	if !identifier.MatchString(id) {
		return nil, fmt.Errorf("invalid Dokki resource ID")
	}
	path := "/resources/" + url.PathEscape(id)
	detail, err := c.dokki(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	resource, ok := object(detail["resource"])
	if !ok || stringAt(resource, "id") != id {
		return nil, fmt.Errorf("Dokki resource identity mismatch")
	}
	body, err := c.dokki(ctx, "GET", path+"/content", nil)
	if err != nil {
		return nil, err
	}
	contentResource, ok := object(body["resource"])
	if !ok || stringAt(contentResource, "id") != id || body["content"] == nil ||
		stringAt(resource, "type") != "" && stringAt(contentResource, "type") != "" && stringAt(resource, "type") != stringAt(contentResource, "type") {
		return nil, fmt.Errorf("Dokki content identity mismatch")
	}
	detail["content"] = body["content"]
	if source := dokkiURL(stringAt(resource, "type"), id); source != "" {
		detail["source_url"] = source
	}
	return detail, nil
}

func (c *Client) confluence(ctx context.Context, method string, params map[string]any) (any, error) {
	token, pat := c.Credentials.Confluence.DockToken, c.Credentials.Confluence.PAT
	if token == "" || pat == "" {
		return nil, fmt.Errorf("Confluence is not configured")
	}
	if !validSecret(token) || !validSecret(pat) {
		return nil, fmt.Errorf("Confluence credential is invalid")
	}
	v, err := c.request(ctx, "POST", c.ConfluenceURL, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, map[string]string{
		"Authorization": "Bearer " + token, "x-confluence-pat": pat, "Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
	if err != nil {
		return nil, err
	}
	rpc, ok := object(v)
	if !ok || rpc["jsonrpc"] != "2.0" || rpc["id"] != float64(1) {
		return nil, fmt.Errorf("Confluence response identity mismatch")
	}
	if rpc["error"] != nil {
		return nil, fmt.Errorf("Confluence request failed")
	}
	result, exists := rpc["result"]
	if !exists {
		return nil, fmt.Errorf("Confluence response has no result")
	}
	return scrub(result, c.secrets(), 0), nil
}

func (c *Client) ListConfluenceTools(ctx context.Context) (any, error) {
	v, err := c.confluence(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	result, ok := object(v)
	if !ok {
		return nil, fmt.Errorf("Confluence tool list is invalid")
	}
	rows, ok := result["tools"].([]any)
	if !ok {
		return nil, fmt.Errorf("Confluence tool list is invalid")
	}
	filtered := []any{}
	for _, item := range rows {
		tool, ok := object(item)
		if ok && confluenceReadTools[stringAt(tool, "name")] {
			if _, ok := object(tool["inputSchema"]); ok {
				filtered = append(filtered, tool)
			}
		}
	}
	return map[string]any{"tools": filtered}, nil
}

func sourceURL(base, link string) string {
	if link == "" || strings.HasPrefix(link, "//") {
		return ""
	}
	if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return ""
		}
		link = strings.TrimRight(base, "/") + "/" + strings.TrimLeft(link, "/")
	}
	u, err := url.Parse(link)
	if err != nil || u.User != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" || strings.Contains(u.Path, "/rest/") || strings.Contains(u.Path, "/api/") {
		return ""
	}
	return u.String()
}

func withSourceURLs(value any, base string, depth int) any {
	if depth > 100 {
		return "[response too deep]"
	}
	switch v := value.(type) {
	case []any:
		for i, item := range v {
			v[i] = withSourceURLs(item, base, depth+1)
		}
		return v
	case map[string]any:
		links, _ := object(v["_links"])
		if b := stringAt(links, "base"); b != "" {
			base = b
		}
		source := sourceURL(base, stringAt(v, "url"))
		if source == "" {
			source = sourceURL(base, stringAt(links, "webui"))
		}
		if source == "" {
			if nested, ok := object(v["content"]); ok {
				if nestedLinks, ok := object(nested["_links"]); ok {
					source = sourceURL(base, stringAt(nestedLinks, "webui"))
				}
			}
		}
		delete(v, "source_url")
		for k, item := range v {
			v[k] = withSourceURLs(item, base, depth+1)
		}
		if source != "" {
			v["source_url"] = source
		}
		return v
	default:
		return v
	}
}

func (c *Client) QueryConfluence(ctx context.Context, tool string, args map[string]any) (any, error) {
	if !confluenceReadTools[tool] {
		return nil, fmt.Errorf("Confluence tool is not on the read-only allowlist")
	}
	if args == nil {
		args = map[string]any{}
	}
	v, err := c.confluence(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return nil, err
	}
	result, ok := object(v)
	if !ok || result["isError"] == true {
		return nil, fmt.Errorf("Confluence tool call failed")
	}
	if structured, ok := object(result["structuredContent"]); ok {
		return withSourceURLs(structured, "", 0), nil
	}
	items, ok := result["content"].([]any)
	if !ok {
		return nil, fmt.Errorf("Confluence tool returned no content")
	}
	for _, item := range items {
		row, ok := object(item)
		if !ok || stringAt(row, "type") != "text" {
			continue
		}
		var parsed any
		if json.Unmarshal([]byte(stringAt(row, "text")), &parsed) == nil {
			return withSourceURLs(parsed, "", 0), nil
		}
	}
	return map[string]any{"content": items}, nil
}

func (c *Client) SearchConfluence(ctx context.Context, query, mode string, limit, start int) (any, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 2000 || strings.ContainsAny(query, "\r\n\x00") || mode != "keyword" && mode != "cql" || limit < 1 || limit > 50 || start < 0 || start > 10000 {
		return nil, fmt.Errorf("invalid Confluence search parameters")
	}
	cql := query
	if mode == "keyword" {
		cql = "type = page and text ~ " + strconv.Quote(query)
	}
	params := url.Values{"cql": {cql}, "limit": {strconv.Itoa(limit)}, "start": {strconv.Itoa(start)}}
	v, err := c.confluenceREST(ctx, "/content/search?"+params.Encode())
	if err != nil {
		return nil, err
	}
	rows, ok := v["results"].([]any)
	if !ok || len(rows) > limit {
		return nil, fmt.Errorf("Confluence search response is invalid")
	}
	for _, item := range rows {
		row, ok := object(item)
		if !ok || !numericPageID.MatchString(stringAt(row, "id")) || stringAt(row, "title") == "" {
			return nil, fmt.Errorf("Confluence search hit is invalid")
		}
	}
	return map[string]any{"data": map[string]any{"results": rows}, "total": v["totalSize"], "start": start, "limit": limit}, nil
}

func (c *Client) confluenceREST(ctx context.Context, path string) (map[string]any, error) {
	pat := c.Credentials.Confluence.PAT
	if pat == "" {
		return nil, fmt.Errorf("Confluence is not configured")
	}
	if !validSecret(pat) {
		return nil, fmt.Errorf("Confluence PAT is invalid")
	}
	v, err := c.request(ctx, "GET", c.ConfluenceRESTURL+path, nil, map[string]string{"Authorization": "Bearer " + pat, "Accept": "application/json"})
	if err != nil {
		return nil, err
	}
	v = withSourceURLs(scrub(v, c.secrets(), 0), "", 0)
	result, ok := object(v)
	if !ok {
		return nil, fmt.Errorf("Confluence REST response is invalid")
	}
	return result, nil
}

func (c *Client) ReadConfluencePage(ctx context.Context, id string) (any, error) {
	if !numericPageID.MatchString(id) {
		return nil, fmt.Errorf("Confluence page ID must be numeric")
	}
	params := url.Values{"expand": {"body.storage,version,space"}}
	v, err := c.confluenceREST(ctx, "/content/"+url.PathEscape(id)+"?"+params.Encode())
	if err != nil {
		return nil, err
	}
	if stringAt(v, "id") != id || stringAt(v, "type") != "page" {
		return nil, fmt.Errorf("Confluence page identity mismatch")
	}
	body, ok := object(v["body"])
	if !ok {
		return nil, fmt.Errorf("Confluence page body is unavailable")
	}
	storage, ok := object(body["storage"])
	if !ok {
		return nil, fmt.Errorf("Confluence page body is unavailable")
	}
	if _, ok = storage["value"].(string); !ok {
		return nil, fmt.Errorf("Confluence page body is unavailable")
	}
	return v, nil
}

func (c *Client) ReadConfluenceResource(ctx context.Context, uri string) (any, error) {
	if !exportURI.MatchString(uri) {
		return nil, fmt.Errorf("invalid Confluence export URI")
	}
	v, err := c.confluence(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	result, ok := object(v)
	if !ok {
		return nil, fmt.Errorf("Confluence resource response is invalid")
	}
	items, ok := result["contents"].([]any)
	if !ok || len(items) == 0 {
		return nil, fmt.Errorf("Confluence resource response is invalid")
	}
	for _, item := range items {
		row, ok := object(item)
		if !ok || stringAt(row, "uri") != uri || (row["text"] == nil && row["blob"] == nil) {
			return nil, errors.New("Confluence resource identity mismatch")
		}
	}
	return result, nil
}
