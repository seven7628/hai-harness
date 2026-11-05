package agenthub

import (
	"errors"
	"reflect"
	"testing"
)

func mentionProfile(id AgentID, name string) *AgentProfile {
	p := validProfile()
	p.ID = id
	p.Name = name
	return p
}

func mentionParser(t *testing.T, profiles ...*AgentProfile) *MentionParser {
	t.Helper()
	p, err := NewMentionParser(profiles)
	if err != nil {
		t.Fatalf("NewMentionParser: %v", err)
	}
	return p
}

func agentIDs(ids ...AgentID) []AgentID { return ids }

func TestMentionBasicAndLongest(t *testing.T) {
	alice := mentionProfile(testAgentID('a'), "Alice")
	bob := mentionProfile(testAgentID('b'), "Bob")
	account := mentionProfile(testAgentID('c'), "Account")
	accountManager := mentionProfile(testAgentID('d'), "Account Manager")
	writer := mentionProfile(testAgentID('e'), "Writer")
	writerLead := mentionProfile(testAgentID('f'), "Writer Lead")
	p := mentionParser(t, alice, bob, account, accountManager, writer, writerLead)

	for _, tc := range []struct {
		text string
		want []AgentID
	}{
		{"@Writer please", agentIDs(writer.ID)},
		{"@writer please", agentIDs(writer.ID)},
		{"@Account Manager, take it", agentIDs(accountManager.ID)},
		{"@Writer Lead and @Bob", agentIDs(writerLead.ID, bob.ID)},
		{"@Account, ping", agentIDs(account.ID)},
	} {
		got := p.Parse(tc.text)
		if !reflect.DeepEqual(got.Mentions, tc.want) {
			t.Fatalf("Parse(%q).Mentions = %v, want %v", tc.text, got.Mentions, tc.want)
		}
	}
}

func TestMentionBoundaries(t *testing.T) {
	writer := mentionProfile(testAgentID('e'), "Writer")
	p := mentionParser(t, writer)
	for _, text := range []string{
		"foo@Writer", // 前面是字母
		"x@Writer",   // 前面是字母
		`\@Writer`,   // 前面是反斜杠
		"/@Writer",   // 前面是斜杠
		"@Writer-",   // 后面是连字符
		"@WriterX",   // 后面是字母
		"@@Writer",   // 双 @
	} {
		if got := p.Parse(text); len(got.Mentions) != 0 {
			t.Fatalf("Parse(%q) matched %v, want none", text, got.Mentions)
		}
	}
	if got := p.Parse("@Writer, hi"); len(got.Mentions) != 1 {
		t.Fatalf("punctuation boundary = %v", got.Mentions)
	}
}

func TestMentionSkipsCodeURLAndEmail(t *testing.T) {
	alice := mentionProfile(testAgentID('a'), "Alice")
	bob := mentionProfile(testAgentID('b'), "Bob")
	writer := mentionProfile(testAgentID('e'), "Writer")
	p := mentionParser(t, alice, bob, writer)

	cases := []struct {
		text string
		want []AgentID
	}{
		{"`@Writer` @Bob", agentIDs(bob.ID)},
		{"```\n@Writer\n@Alice\n```\n@Bob", agentIDs(bob.ID)},
		{"https://example.com/@Writer @Bob", agentIDs(bob.ID)},
		{"(https://example.com/@Writer) @Bob", agentIDs(bob.ID)},
		{"mail a@Writer.com @Bob", agentIDs(bob.ID)},
		{"@Writer and @Alice", agentIDs(writer.ID, alice.ID)},
	}
	for _, tc := range cases {
		got := p.Parse(tc.text)
		if !reflect.DeepEqual(got.Mentions, tc.want) {
			t.Fatalf("Parse(%q).Mentions = %v, want %v", tc.text, got.Mentions, tc.want)
		}
	}
}

func TestMentionUnknownAndLimit(t *testing.T) {
	alice := mentionProfile(testAgentID('a'), "Alice")
	bob := mentionProfile(testAgentID('b'), "Bob")
	carol := mentionProfile(testAgentID('c'), "Carol")
	dave := mentionProfile(testAgentID('d'), "Dave")
	p := mentionParser(t, alice, bob, carol, dave)

	got := p.Parse("@Nobody, hi")
	if len(got.Mentions) != 0 || len(got.Unknown) != 1 || got.Unknown[0] != "Nobody" {
		t.Fatalf("unknown = %#v", got)
	}
	got = p.Parse("@Alice @Bob @Carol @Dave")
	if len(got.Mentions) != MaxMentionsPerMessage || got.Dropped != 1 {
		t.Fatalf("limit = %#v", got)
	}
	got = p.Parse("@Alice @Alice @Bob")
	if len(got.Mentions) != 2 {
		t.Fatalf("dedupe = %#v", got)
	}
}

func TestMentionAllAndHuman(t *testing.T) {
	alice := mentionProfile(testAgentID('a'), "Alice")
	p := mentionParser(t, alice)
	for _, text := range []string{"@all", "@ALL", "@所有人"} {
		got := p.Parse(text)
		if !got.MentionAll || len(got.Mentions) != 0 {
			t.Fatalf("Parse(%q) = %#v", text, got)
		}
	}
	if got := p.Parse("@all @Alice"); !got.MentionAll || !reflect.DeepEqual(got.Mentions, agentIDs(alice.ID)) {
		t.Fatalf("@all + member = %#v", got)
	}
	for _, text := range []string{"@你", "@user"} {
		got := p.Parse(text)
		if !got.HumanMention || len(got.Mentions) != 0 {
			t.Fatalf("Parse(%q) = %#v", text, got)
		}
	}
}

func TestMentionSelfIgnored(t *testing.T) {
	alice := mentionProfile(testAgentID('a'), "Alice")
	bob := mentionProfile(testAgentID('b'), "Bob")
	p := mentionParser(t, alice, bob)
	got := p.ParseFrom("@Alice @Bob", alice.ID)
	if !reflect.DeepEqual(got.Mentions, agentIDs(bob.ID)) || got.Dropped != 0 {
		t.Fatalf("self mention = %#v", got)
	}
}

func TestMentionDuplicateNamesRejected(t *testing.T) {
	if _, err := NewMentionParser([]*AgentProfile{
		mentionProfile(testAgentID('a'), "Writer"),
		mentionProfile(testAgentID('b'), "writer"),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate err = %v, want ErrInvalid", err)
	}
	for _, name := range []string{"all", "所有人", "你", "user"} {
		if _, err := NewMentionParser([]*AgentProfile{mentionProfile(testAgentID('a'), name)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("reserved %q err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestMentionAllPermissions(t *testing.T) {
	if !CanMentionAll(true, RoleMember) {
		t.Fatal("human should be able to @all")
	}
	if !CanMentionAll(false, RoleOwner) {
		t.Fatal("owner should be able to @all")
	}
	if CanMentionAll(false, RoleMember) || CanMentionAll(false, RoleObserver) {
		t.Fatal("member/observer must not @all")
	}
	res := MentionResult{MentionAll: true}
	if err := res.ValidateMentionAll(false, RoleMember); !errors.Is(err, ErrMentionAllForbidden) {
		t.Fatalf("member @all err = %v, want ErrMentionAllForbidden", err)
	}
	if err := res.ValidateMentionAll(false, RoleOwner); err != nil {
		t.Fatalf("owner @all: %v", err)
	}
	if err := res.ValidateMentionAll(true, RoleObserver); err != nil {
		t.Fatalf("human @all: %v", err)
	}
	if err := (MentionResult{}).ValidateMentionAll(false, RoleMember); err != nil {
		t.Fatalf("no @all: %v", err)
	}
}

func TestMentionVisibility(t *testing.T) {
	owner := Message{Text: "secret", Visibility: VisibilityOwner}
	if owner.VisibleTo(false) || !owner.VisibleTo(true) {
		t.Fatal("owner-only visibility wrong")
	}
	if owner.ContextText(false) != OwnerOnlyPlaceholder || owner.ContextText(true) != "secret" {
		t.Fatalf("owner-only context text = %q / %q", owner.ContextText(false), owner.ContextText(true))
	}
	group := Message{Text: "hi"}
	if group.Visibility.EffectiveVisibility() != VisibilityGroup || !group.VisibleTo(false) {
		t.Fatal("empty visibility should default group")
	}
	if err := (Visibility("team")).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid visibility err = %v", err)
	}
}
