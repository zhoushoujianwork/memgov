package runtime

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

// groupImageInput sends attached images as native user content blocks. The
// binary data never enters task JSON, the system prompt, or structured logs.
func groupImageInput(in ExecutionInput, payload []byte) ([]byte, bool, error) {
	if in.ApplicationMode != "group_mention" {
		return payload, false, nil
	}
	blocks := []any{map[string]any{"type": "text", "text": string(payload)}}
	count := 0
	for _, message := range in.Task.Messages {
		for _, attachment := range message.Attachments {
			if attachment.ResourceID == "" || !strings.HasPrefix(attachment.MediaType, "image/") {
				continue
			}
			count++
			if count > maxGroupImages {
				return nil, false, core.Fail("invalid_input", "group request has too many pictures")
			}
			data, err := readGroupImage(in.Home, attachment)
			if err != nil {
				return nil, false, err
			}
			blocks = append(blocks,
				map[string]any{"type": "text", "text": "Untrusted picture " + attachment.Name + " from message " + message.ID + ":"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": attachment.MediaType, "data": base64.StdEncoding.EncodeToString(data)}})
		}
	}
	if count == 0 {
		return payload, false, nil
	}
	line, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": blocks}})
	if err != nil {
		return nil, false, core.Fail("unavailable", "group picture input could not be encoded")
	}
	return append(line, '\n'), true, nil
}

const maxGroupImages = 4
const maxGroupImageBytes = 5 << 20

func readGroupImage(home string, attachment core.Attachment) ([]byte, error) {
	if !filepath.IsAbs(home) || len(attachment.ResourceID) != 64 {
		return nil, core.Fail("invalid_input", "group picture reference is invalid")
	}
	if _, err := hex.DecodeString(attachment.ResourceID); err != nil {
		return nil, core.Fail("invalid_input", "group picture reference is invalid")
	}
	path := filepath.Join(home, "runtime", "media", attachment.ResourceID)
	stat, err := os.Lstat(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() <= 0 || stat.Size() > maxGroupImageBytes {
		return nil, core.Fail("unavailable", "group picture is missing or too large")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > maxGroupImageBytes {
		return nil, core.Fail("unavailable", "group picture could not be read")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != attachment.ResourceID || http.DetectContentType(data) != attachment.MediaType {
		return nil, core.Fail("unavailable", "group picture failed integrity validation")
	}
	return data, nil
}
