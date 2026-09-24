package core

import (
	"context"
	"testing"
)

func TestRuntimeMessagesLoadCurrentPictureReference(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c, _ := fixtureChannel(t, s, ChannelDingTalkApp, "cid:group1")
	e := sampleEvent("picture-request", "请看[图片 1]", Sender{IDType: "user_id", IDValue: "alice"})
	e.Adapter = "dingtalk_app"
	e.Attachments = []Attachment{{Name: "图片 1", MediaType: "image/png", ResourceID: "content-digest"}}
	stored := intake(t, s, c.ID, e, "picture-request")
	messages, err := runtimeMessages(ctx, s.DB, []string{stored.MessageID})
	if err != nil || len(messages) != 1 || len(messages[0].Attachments) != 1 || messages[0].Attachments[0].ResourceID != "content-digest" {
		t.Fatalf("current picture reference not loaded: %+v %v", messages, err)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE messages SET availability='recalled' WHERE id=?", stored.MessageID); err != nil {
		t.Fatal(err)
	}
	messages, err = runtimeMessages(ctx, s.DB, []string{stored.MessageID})
	if err != nil || len(messages) != 1 || len(messages[0].Attachments) != 0 {
		t.Fatalf("recalled picture was supplied to runtime: %+v %v", messages, err)
	}
}
