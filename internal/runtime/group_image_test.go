package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/agent"
	"github.com/zhoushoujianwork/memgov/internal/core"
)

func TestGroupImageInputSuppliesNativeVisionBlock(t *testing.T) {
	home := t.TempDir()
	image, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(image)
	id := hex.EncodeToString(sum[:])
	root := filepath.Join(home, "runtime", "media")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, id), image, 0600); err != nil {
		t.Fatal(err)
	}
	in := ExecutionInput{Home: home, ApplicationMode: "group_mention", Task: core.RuntimeTask{Messages: []core.RuntimeMessage{{ID: "message-a", Body: "请解释图片", Attachments: []core.Attachment{{Name: "图片 1", MediaType: "image/png", ResourceID: id}}}}}}
	line, hasImage, err := groupImageInput(in, []byte(`{"task":"question"}`))
	if err != nil || !hasImage {
		t.Fatalf("image input: %v %v", hasImage, err)
	}
	var envelope struct {
		Message struct {
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err = json.Unmarshal(line, &envelope); err != nil || len(envelope.Message.Content) != 3 || envelope.Message.Content[2].Type != "image" || envelope.Message.Content[2].Source.MediaType != "image/png" || envelope.Message.Content[2].Source.Data != base64.StdEncoding.EncodeToString(image) {
		t.Fatalf("image was not passed as a native block: %v %+v", err, envelope)
	}
	if err = os.WriteFile(filepath.Join(root, id), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = groupImageInput(in, []byte(`{}`)); core.ErrorCode(err) != "unavailable" {
		t.Fatalf("tampered media was accepted: %v", err)
	}
}

func TestGroupExecutionSelectsMultimodalInputForPicture(t *testing.T) {
	home := t.TempDir()
	image, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC")
	sum := sha256.Sum256(image)
	id := hex.EncodeToString(sum[:])
	root := filepath.Join(home, "runtime", "media")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, id), image, 0600); err != nil {
		t.Fatal(err)
	}
	preset, err := agent.Enable(context.Background(), t.TempDir(), "claude", "group-image")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	c := &Claude{Run: func(_ context.Context, _ string, input []byte, args ...string) ([]byte, error) {
		called = true
		values := claudeArgumentValues(args)
		if values["--input-format"] != "stream-json" || !json.Valid(input) {
			t.Fatalf("group image bypassed native input: args=%v input=%q", args, input)
		}
		return claudeResult(t, map[string]any{"result": "图片内容", "summary": "已查看", "artifacts": []string{}, "tool_kinds": []string{}, "pending_actions": []any{}}), nil
	}}
	_, err = c.Execute(context.Background(), ExecutionInput{Home: home, WorkDir: t.TempDir(), Preset: preset, ApplicationMode: "group_mention", Task: core.RuntimeTask{ID: "image-task", Messages: []core.RuntimeMessage{{ID: "image-message", Body: "请看[图片 1]", Attachments: []core.Attachment{{Name: "图片 1", MediaType: "image/png", ResourceID: id}}}}}})
	if err != nil || !called {
		t.Fatalf("group image did not reach the model: %v", err)
	}
}
