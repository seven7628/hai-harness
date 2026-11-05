package agenthub

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func validProfile() *AgentProfile {
	return &AgentProfile{
		ID:                "ag_7f3k2m7q",
		Name:              "Researcher",
		Avatar:            Avatar{Kind: AvatarKindEmoji, Value: "🟢", BG: "#2f6f5e"},
		Accent:            "#2f6f5e",
		Provider:          "deepseek",
		Model:             "deepseek-v4-flash",
		CompressThreshold: 0.8,
		Memory:            MemoryPrefs{Enabled: true, MaxIndexLines: 200},
		Tools:             ToolPolicy{Preset: ToolPresetAll},
		Policy:            RunPolicy{MaxAutoTurns: 6},
		Version:           1,
	}
}

func TestProfileNormalizeDefaults(t *testing.T) {
	p := &AgentProfile{Name: "  Researcher  ", Provider: "deepseek", Model: "m"}
	if err := p.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Name != "Researcher" {
		t.Fatalf("name = %q", p.Name)
	}
	if p.Avatar.Kind != AvatarKindEmoji || p.Avatar.Value == "" {
		t.Fatalf("avatar defaults = %#v", p.Avatar)
	}
	if _, ok := normalizeHexColor(p.Accent); !ok {
		t.Fatalf("accent not derived: %q", p.Accent)
	}
	if p.Avatar.BG != p.Accent {
		t.Fatalf("avatar.bg = %q, want derived accent %q", p.Avatar.BG, p.Accent)
	}
	if p.CompressThreshold != DefaultCompressThreshold {
		t.Fatalf("compress threshold = %v", p.CompressThreshold)
	}
	if p.Memory.MaxIndexLines != DefaultMemoryIndexLines {
		t.Fatalf("memory max lines = %d", p.Memory.MaxIndexLines)
	}
	if p.Tools.Preset != ToolPresetAll {
		t.Fatalf("tools preset = %q", p.Tools.Preset)
	}
	if p.Policy.MaxAutoTurns != DefaultMaxAutoTurns {
		t.Fatalf("max auto turns = %d", p.Policy.MaxAutoTurns)
	}
	if p.Version != 1 {
		t.Fatalf("version = %d", p.Version)
	}
}

func TestProfileAccentDefaults(t *testing.T) {
	p := &AgentProfile{Name: "Researcher", Provider: "deepseek", Model: "m", Avatar: Avatar{BG: "#2F6F5E"}}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.Accent != "#2f6f5e" || p.Avatar.BG != "#2f6f5e" {
		t.Fatalf("accent from avatar.bg = %q / %q", p.Accent, p.Avatar.BG)
	}

	a := &AgentProfile{Name: "Alpha", Provider: "p", Model: "m"}
	b := &AgentProfile{Name: "Beta", Provider: "p", Model: "m"}
	a2 := &AgentProfile{Name: "Alpha", Provider: "p", Model: "m"}
	if err := a.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := b.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := a2.Normalize(); err != nil {
		t.Fatal(err)
	}
	if a.Accent != a2.Accent {
		t.Fatalf("accent not stable: %q vs %q", a.Accent, a2.Accent)
	}
	if a.Accent == b.Accent {
		t.Fatalf("different names produced same accent %q", a.Accent)
	}
}

func TestProfileCompressThresholdClamped(t *testing.T) {
	for _, tc := range []struct {
		in, want float64
	}{
		{0.2, 0.5},
		{0.5, 0.5},
		{0.8, 0.8},
		{0.95, 0.95},
		{0.99, 0.95},
	} {
		p := validProfile()
		p.CompressThreshold = tc.in
		if err := p.Normalize(); err != nil {
			t.Fatalf("in %v: %v", tc.in, err)
		}
		if p.CompressThreshold != tc.want {
			t.Fatalf("threshold %v -> %v, want %v", tc.in, p.CompressThreshold, tc.want)
		}
	}
	p := validProfile()
	p.CompressThreshold = math.NaN()
	if err := p.Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NaN threshold err = %v, want ErrInvalid", err)
	}
}

func TestProfileValidation(t *testing.T) {
	longName := strings.Repeat("界", 33)
	tests := []struct {
		name   string
		mutate func(*AgentProfile)
	}{
		{"empty name", func(p *AgentProfile) { p.Name = "   " }},
		{"long name", func(p *AgentProfile) { p.Name = longName }},
		{"reserved all", func(p *AgentProfile) { p.Name = "ALL" }},
		{"at in name", func(p *AgentProfile) { p.Name = "a@b" }},
		{"empty provider", func(p *AgentProfile) { p.Provider = " " }},
		{"bad provider", func(p *AgentProfile) { p.Provider = "bad provider" }},
		{"empty model", func(p *AgentProfile) { p.Model = " " }},
		{"bad avatar kind", func(p *AgentProfile) { p.Avatar.Kind = "gif" }},
		{"image without path", func(p *AgentProfile) { p.Avatar = Avatar{Kind: AvatarKindImage} }},
		{"bad accent", func(p *AgentProfile) { p.Accent = "#12345" }},
		{"bad avatar bg", func(p *AgentProfile) { p.Avatar.BG = "green" }},
		{"allow with all", func(p *AgentProfile) { p.Tools.Allow = []string{"bash"} }},
		{"disable reenabled", func(p *AgentProfile) {
			p.Tools = ToolPolicy{Preset: ToolPresetCustom, Disable: []string{"bash"}, Allow: []string{"bash"}}
		}},
		{"custom without allow", func(p *AgentProfile) {
			p.Tools = ToolPolicy{Preset: ToolPresetCustom}
		}},
		{"mcp disable reenabled", func(p *AgentProfile) {
			p.Tools = ToolPolicy{Preset: ToolPresetCustom, Allow: []string{"x"},
				MCP: MCPPolicy{DisableTools: []string{"s.t"}, AllowTools: []string{"s.t"}}}
		}},
		{"zero width name", func(p *AgentProfile) { p.Name = "Researcher\u200b" }},
		{"workspace without roots", func(p *AgentProfile) {
			p.Policy.CanCreateWorkspace = true
			p.Policy.WorkspaceRoots = nil
		}},
		{"negative memory lines", func(p *AgentProfile) { p.Memory.MaxIndexLines = -1 }},
		{"negative budget", func(p *AgentProfile) { p.Policy.DailyBudgetUSD = -1 }},
		{"bad id", func(p *AgentProfile) { p.ID = "bad" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := validProfile()
			tc.mutate(p)
			if err := p.Normalize(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Normalize err = %v, want ErrInvalid", err)
			}
		})
	}

	p := fullProfile()
	if err := p.Normalize(); err != nil {
		t.Fatalf("fullProfile Normalize: %v", err)
	}
}

func TestProfileValidateToleratesZeroDefaults(t *testing.T) {
	p := &AgentProfile{Name: "R", Provider: "deepseek", Model: "m"}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate(zero defaults): %v", err)
	}
	p.Accent = "#12345"
	if err := p.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("explicit bad accent err = %v", err)
	}
	p.Accent = ""
	p.Avatar.Kind = "gif"
	if err := p.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("explicit bad avatar kind err = %v", err)
	}
	p.Avatar.Kind = ""
	p.CompressThreshold = 0.2
	if err := p.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("explicit bad threshold err = %v", err)
	}
}

func TestProfileNormalizeDoesNotMutateOnFailure(t *testing.T) {
	p := validProfile()
	p.Name = "   "
	p.Accent = "#12345"
	before := *p
	before.Tools.Disable = append([]string(nil), p.Tools.Disable...)
	if err := p.Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Normalize err = %v", err)
	}
	if p.Name != before.Name || p.Accent != before.Accent || p.Version != before.Version {
		t.Fatalf("Normalize mutated on failure:\n got %#v\nwant %#v", p, before)
	}
}

func TestProfileNameBoundary32(t *testing.T) {
	p := validProfile()
	p.Name = strings.Repeat("界", 32)
	if err := p.Normalize(); err != nil {
		t.Fatalf("32-rune name: %v", err)
	}
}

func TestProfileNameUnique(t *testing.T) {
	existing := []*AgentProfile{
		{ID: "ag_one", Name: "Researcher"},
		{ID: "ag_two", Name: "Writer"},
		{ID: "ag_del", Name: "Deleted", Deleted: true},
	}
	if err := ValidateAgentNameUnique("researcher", existing, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("case-insensitive duplicate err = %v", err)
	}
	if err := ValidateAgentNameUnique("researcher", existing, "ag_one"); err != nil {
		t.Fatalf("except self: %v", err)
	}
	if err := ValidateAgentNameUnique("Deleted", existing, ""); err != nil {
		t.Fatalf("deleted profile should be ignored: %v", err)
	}
	if err := ValidateAgentNameUnique("New", existing, ""); err != nil {
		t.Fatalf("new name: %v", err)
	}
}

func TestProfileIDAndLimit(t *testing.T) {
	if err := ValidateAgentID(AgentID(NewID(PrefixAgent))); err != nil {
		t.Fatalf("generated agent id invalid: %v", err)
	}
	if err := ValidateAgentID("ag_test"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short agent id err = %v", err)
	}
	if err := ValidateAgentID("ag_00000000"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-base32 agent id err = %v", err)
	}
	if err := ValidateGroupID(GroupID(NewID(PrefixGroup))); err != nil {
		t.Fatalf("generated group id invalid: %v", err)
	}
	agents := make([]*AgentProfile, MaxAgents)
	for i := range agents {
		agents[i] = &AgentProfile{ID: AgentID(NewID(PrefixAgent)), Name: "a"}
	}
	if err := ValidateAgentCount(agents[:MaxAgents-1]); err != nil {
		t.Fatalf("limit-1: %v", err)
	}
	if err := ValidateAgentCount(agents); !errors.Is(err, ErrInvalid) {
		t.Fatalf("limit err = %v", err)
	}
	agents[0].Deleted = true
	if err := ValidateAgentCount(agents); err != nil {
		t.Fatalf("deleted should not count: %v", err)
	}
}

func testAgentID(letter byte) AgentID {
	return AgentID("ag_" + strings.Repeat(string(letter), 8))
}

func validGroup() *Group {
	return &Group{
		Name:         "竞品研究",
		OwnerAgentID: testAgentID('a'),
		Members: []Member{
			{AgentID: testAgentID('a'), Role: RoleOwner},
			{AgentID: testAgentID('b'), Role: RoleMember},
			{AgentID: testAgentID('c'), Role: RoleObserver},
		},
	}
}

func TestGroupNormalizeDefaults(t *testing.T) {
	g := validGroup()
	g.Policy.Mode = ""
	g.Members[1].Role = ""
	if err := g.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if g.OwnerAgentID != testAgentID('a') {
		t.Fatalf("owner = %q", g.OwnerAgentID)
	}
	if g.Members[1].Role != RoleMember {
		t.Fatalf("empty role = %q, want member", g.Members[1].Role)
	}
	if g.Members[0].JoinedAt.IsZero() {
		t.Fatal("joined_at not defaulted")
	}
	if g.Policy.Mode != GroupModeAsk || g.Policy.MaxParallelRuns != 3 ||
		g.Policy.MaxHandoffDepth != 3 || g.Policy.MaxTurnsPerHuman != 8 || g.Policy.MaxMentions != 3 {
		t.Fatalf("policy defaults = %#v", g.Policy)
	}
}

func TestGroupValidation(t *testing.T) {
	many := make([]Member, 0, MaxGroupMembers+1)
	many = append(many, Member{AgentID: testAgentID('a'), Role: RoleOwner})
	for i := 0; i < MaxGroupMembers; i++ {
		many = append(many, Member{AgentID: testAgentID(byte('b' + i)), Role: RoleMember})
	}
	tests := []struct {
		name   string
		mutate func(*Group)
	}{
		{"empty name", func(g *Group) { g.Name = " " }},
		{"long name", func(g *Group) { g.Name = strings.Repeat("界", 33) }},
		{"no members", func(g *Group) { g.Members = nil }},
		{"too many members", func(g *Group) { g.Members = many }},
		{"duplicate member", func(g *Group) { g.Members = append(g.Members, Member{AgentID: testAgentID('b'), Role: RoleMember}) }},
		{"no owner", func(g *Group) {
			g.Members = []Member{{AgentID: testAgentID('a'), Role: RoleMember}}
			g.OwnerAgentID = ""
		}},
		{"owner role mismatch", func(g *Group) {
			g.Members[0].Role = RoleMember
		}},
		{"invalid role", func(g *Group) { g.Members[1].Role = "admin" }},
		{"owner not member", func(g *Group) { g.OwnerAgentID = testAgentID('d') }},
		{"bad mode", func(g *Group) { g.Policy.Mode = "sometimes" }},
		{"negative parallel", func(g *Group) { g.Policy.MaxParallelRuns = -1 }},
		{"bad member id", func(g *Group) { g.Members[1].AgentID = "bad" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := validGroup()
			tc.mutate(g)
			if err := g.Normalize(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Normalize err = %v, want ErrInvalid", err)
			}
		})
	}

	g := validGroup()
	if err := g.Normalize(); err != nil {
		t.Fatalf("valid group: %v", err)
	}

	// 恰好 12 人通过。
	atLimit := validGroup()
	atLimit.Members = atLimit.Members[:1]
	for i := 0; i < MaxGroupMembers-1; i++ {
		atLimit.Members = append(atLimit.Members, Member{AgentID: testAgentID(byte('b' + i)), Role: RoleMember})
	}
	if err := atLimit.Normalize(); err != nil {
		t.Fatalf("12-member group: %v", err)
	}
}

func TestGroupNormalizeDoesNotMutateOnFailure(t *testing.T) {
	g := validGroup()
	g.Name = " "
	g.Members[1].AgentID = "bad"
	before := *g
	before.Members = append([]Member(nil), g.Members...)
	if err := g.Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Normalize err = %v", err)
	}
	if g.Name != before.Name || g.Members[1].AgentID != before.Members[1].AgentID {
		t.Fatalf("Normalize mutated on failure: %#v", g)
	}
}

func TestGroupRejectsMalformedID(t *testing.T) {
	for _, id := range []AgentID{"ag_bad", "ag_../x", "ag_XYZ12345", "ag_00000000"} {
		g := validGroup()
		g.OwnerAgentID = id
		g.Members[0].AgentID = id
		if err := g.Normalize(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("owner id %q err = %v, want ErrInvalid", id, err)
		}
	}
}

func TestGroupNameAndMemberNamesUnique(t *testing.T) {
	groups := []*Group{{ID: "grp_one", Name: "Research"}, {ID: "grp_del", Name: "Gone", Deleted: true}}
	if err := ValidateGroupNameUnique("research", groups, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate group name err = %v", err)
	}
	if err := ValidateGroupNameUnique("research", groups, "grp_one"); err != nil {
		t.Fatalf("except self: %v", err)
	}
	if err := ValidateGroupNameUnique("Gone", groups, ""); err != nil {
		t.Fatalf("deleted group should be ignored: %v", err)
	}

	g := validGroup()
	profiles := map[AgentID]*AgentProfile{
		testAgentID('a'): {ID: testAgentID('a'), Name: "Researcher"},
		testAgentID('b'): {ID: testAgentID('b'), Name: "researcher"},
		testAgentID('c'): {ID: testAgentID('c'), Name: "Observer"},
	}
	if err := ValidateGroupMemberNames(g, profiles); !errors.Is(err, ErrInvalid) {
		t.Fatalf("member name collision err = %v", err)
	}
	profiles[testAgentID('b')].Name = "Writer"
	if err := ValidateGroupMemberNames(g, profiles); err != nil {
		t.Fatalf("unique member names: %v", err)
	}
	delete(profiles, testAgentID('c'))
	if err := ValidateGroupMemberNames(g, profiles); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing profile err = %v", err)
	}
}
