package core

// P3b-01a：群唤醒 hidden 块（header + 快照）的 allowlist 回归。

import "testing"

func TestMessageValidateHubGroupWake(t *testing.T) {
	hidden := Content{
		Type:    ContentTypeHubGroupWake,
		Content: "{\"group_id\":\"grp_x\",\"agent_id\":\"ag_x\",\"round_id\":\"rnd_x\",\"trigger_seq\":7}\n\n群快照",
	}
	msg := NewUserMessage(hidden, Content{Type: ContentTypeText, Content: "@你 看一下"})
	if err := msg.Validate(); err != nil {
		t.Fatalf("hub_group_wake block should validate: %v", err)
	}
	hiddenOnly := NewUserMessage(hidden)
	if err := hiddenOnly.Validate(); err != nil {
		t.Fatalf("hidden-only group wake should validate: %v", err)
	}
	// allowlist 只加这一项，不能连带放开拼写相近的未知类型。
	bad := NewUserMessage(Content{Type: "hub_group_wakes", Content: "x"})
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown content type must still be rejected")
	}
}
