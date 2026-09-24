package dingtalkapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhoushoujianwork/memgov/internal/channel"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

const (
	messageFileDownloadPath = "/v1.0/robot/messageFiles/download"
	maxPictureBytes         = 5 << 20
	maxPictures             = 4
)

// pictureCodes reads capabilities only from the authenticated callback in memory.
// Neither codes nor the temporary download URL are copied into NormalizedEvent.
func pictureCodes(raw []byte) []string {
	var cb botCallback
	if json.Unmarshal(raw, &cb) != nil {
		return nil
	}
	var parts []map[string]json.RawMessage
	switch cb.MsgType {
	case "picture":
		var part map[string]json.RawMessage
		if json.Unmarshal(cb.Content, &part) == nil {
			parts = append(parts, part)
		}
	case "richText":
		var content struct {
			RichText []map[string]json.RawMessage `json:"richText"`
		}
		if json.Unmarshal(cb.Content, &content) == nil {
			parts = content.RichText
		}
	}
	var codes []string
	for _, part := range parts {
		if cb.MsgType == "richText" {
			var kind string
			_ = json.Unmarshal(part["type"], &kind)
			if kind != "picture" {
				continue
			}
		}
		var code string
		_ = json.Unmarshal(part["downloadCode"], &code)
		if code == "" {
			_ = json.Unmarshal(part["pictureDownloadCode"], &code)
		}
		codes = append(codes, code)
	}
	return codes
}

func trustedMediaURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return false
	}
	for _, domain := range []string{"aliyuncs.com", "alicdn.com", "dingtalk.com", "dingtalkcdn.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func (a *Adapter) downloadPicture(ctx context.Context, cfg channel.Config, code string) ([]byte, string, error) {
	if code == "" || len(code) > 4096 {
		return nil, "", core.Fail("invalid_input", "picture download code is invalid")
	}
	raw, err := a.post(ctx, cfg, messageFileDownloadPath, map[string]string{"downloadCode": code, "robotCode": cfg.Identity.RobotCode})
	if err != nil {
		return nil, "", err
	}
	var reply struct {
		DownloadURL string `json:"downloadUrl"`
	}
	if json.Unmarshal(raw, &reply) != nil || !trustedMediaURL(reply.DownloadURL) {
		return nil, "", core.Fail("unavailable", "DingTalk returned an invalid picture download URL")
	}
	client := *a.client()
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if !trustedMediaURL(req.URL.String()) {
			return core.Fail("denied", "picture download redirected outside DingTalk media hosts")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reply.DownloadURL, nil)
	if err != nil {
		return nil, "", core.Fail("unavailable", "could not create picture download request")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", core.Fail("unavailable", "DingTalk picture download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxPictureBytes {
		return nil, "", core.Fail("unavailable", "DingTalk picture download was rejected or too large")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPictureBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxPictureBytes {
		return nil, "", core.Fail("unavailable", "DingTalk picture was unreadable or too large")
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return data, mime, nil
	default:
		return nil, "", core.Fail("invalid_input", "DingTalk picture has an unsupported media type")
	}
}

// storePicture writes content-addressed media with owner-only permissions. The
// database receives only the digest; task execution resolves it under MediaRoot.
func (a *Adapter) storePicture(data []byte) (string, error) {
	root := a.MediaRoot
	if root == "" || !filepath.IsAbs(root) {
		return "", core.Fail("unavailable", "DingTalk picture storage is not configured")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", core.Fail("unavailable", "could not create picture storage")
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	path := filepath.Join(root, id)
	if _, err := os.Stat(path); err == nil {
		return id, nil
	}
	f, err := os.CreateTemp(root, ".picture-*")
	if err != nil {
		return "", core.Fail("unavailable", "could not stage picture")
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return "", core.Fail("unavailable", "could not save picture")
	}
	return id, nil
}

func (a *Adapter) attachPictures(ctx context.Context, cfg channel.Config, raw []byte, event *core.NormalizedEvent) {
	if a.MediaRoot == "" || event.ConversationType != "group" || !event.Mentioned {
		return
	}
	bound := false
	for _, conversation := range cfg.Conversations {
		if conversation == event.ConversationID {
			bound = true
			break
		}
	}
	if !bound {
		return
	}
	codes := pictureCodes(raw)
	if len(codes) > maxPictures {
		return
	}
	imageCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	picture := 0
	for i := range event.Attachments {
		attachment := &event.Attachments[i]
		if attachment.MediaType != "image/*" {
			continue
		}
		if picture >= len(codes) {
			return
		}
		code := codes[picture]
		picture++
		data, mime, err := a.downloadPicture(imageCtx, cfg, code)
		if err != nil {
			continue
		}
		id, err := a.storePicture(data)
		if err != nil {
			continue
		}
		old := attachment.Name
		attachment.Name = strings.Replace(old, "（内容未读取）", "", 1)
		attachment.MediaType = mime
		attachment.ResourceID = id
		event.Body = strings.Replace(event.Body, "["+old+"]", "["+attachment.Name+"]", 1)
	}
}
