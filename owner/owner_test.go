package owner

import "testing"

// TestFromSessionID Hub 会话 id 是归属真相源：DM→agent、群→group、其它→workspace。
func TestFromSessionID(t *testing.T) {
	cases := []struct{ name, sid, ws, want string }{
		{"Hub DM", "hub.abc.dm", "/home/u/.go-code/hub/homes/abc", "agent:abc"},
		{"Hub 群聊", "hub.abc.g.g1", "/home/u/.go-code/hub/homes/abc", "group:g1"},
		{"群 id 带点", "hub.abc.g.team.eu", "", "group:team.eu"},
		{"项目会话", "s-123", "/proj/x", "workspace:/proj/x"},
		{"cron 派生会话（同工作区）", "cron-42", "/proj/x", "workspace:/proj/x"},
		{"非 hub 前缀但像 hub", "hubby", "/proj/x", "workspace:/proj/x"},
		// hub 前缀但形状不对：显式「无法归属」（空），不回落成 workspace ——
		// 那会把智能体 home 当成一个项目归属（谁都不该拥有的 workspace）。
		{"hub 但缺 .dm 后缀", "hub.abc", "/proj/x", ""},
		{"hub 群但空 group", "hub.abc.g.", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromSessionID(tc.sid, tc.ws); got != tc.want {
				t.Fatalf("FromSessionID(%q,%q) = %q, want %q", tc.sid, tc.ws, got, tc.want)
			}
		})
	}
}

// TestKindAndValid 合法归属只有三维；legacy 只读不可写。
func TestKindAndValid(t *testing.T) {
	cases := []struct {
		in, kind string
		valid    bool
	}{
		{"workspace:/p", "workspace", true},
		{"agent:a", "agent", true},
		{"group:g", "group", true},
		{Legacy, "legacy", false},
		{"workspace:", "", false},
		{"agent:", "", false},
		{"global", "", false},
		{"", "", false},
		{"Agent:a", "", false},
	}
	for _, tc := range cases {
		if got := Kind(tc.in); got != tc.kind {
			t.Fatalf("Kind(%q) = %q, want %q", tc.in, got, tc.kind)
		}
		if got := Valid(tc.in); got != tc.valid {
			t.Fatalf("Valid(%q) = %v, want %v", tc.in, got, tc.valid)
		}
	}
}

// TestNoImplicitInheritance 产品决策：同一 agent 的 DM 与群聊不共享归属，
// 群与 agent 之间也没有包含关系（任何隐式继承都会把隔离决策悄悄推翻）。
func TestNoImplicitInheritance(t *testing.T) {
	dm := FromSessionID("hub.a.dm", "")
	group := FromSessionID("hub.a.g.g1", "")
	if dm == group {
		t.Fatalf("同一 agent 的 DM 与群必须不同归属，得到 %q / %q", dm, group)
	}
	if Kind(dm) != "agent" || Kind(group) != "group" {
		t.Fatalf("归属维度不对: %q / %q", Kind(dm), Kind(group))
	}
	if ID(dm) != "a" || ID(group) != "g1" {
		t.Fatalf("ID 派生不对: %q / %q", ID(dm), ID(group))
	}
}

// TestWorkspaceOwnerOnlyForPaths 空路径不产生归属（避免 workspace: 这种「谁都拥有」的值）。
func TestWorkspaceOwnerOnlyForPaths(t *testing.T) {
	if Workspace("") != "" || Workspace("   ") != "" {
		t.Fatal("空路径必须返回空归属")
	}
	if Agent("") != "" || Group("  ") != "" {
		t.Fatal("空 id 必须返回空归属")
	}
	if Workspace("/a") != "workspace:/a" {
		t.Fatalf("Workspace(/a) = %q", Workspace("/a"))
	}
}
