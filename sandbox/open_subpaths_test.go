package sandbox

// WithOpenSubpaths：敏感 deny 之后重开子树（Agent Hub 智能体 home 在 ~/.go-code 下）。
// SBPL 后规则胜，所以顺序错了等于没开 —— 这里用策略文本的顺序钉住。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openSubpathsPolicy(t *testing.T, hub string, deny []string) (policy string) {
	t.Helper()
	if !Available() {
		t.Skip("seatbelt not available")
	}
	sb, err := NewSeatbelt(hub, WithSensitivePaths(deny), WithOpenSubpaths(hub), WithNoProbe())
	if err != nil {
		t.Fatalf("NewSeatbelt: %v", err)
	}
	return sb.Policy
}

func TestOpenSubpathsReopensAfterSensitiveDeny(t *testing.T) {
	// 模拟真实布局：智能体 home 在 $HOME/.go-code/hub/agents/<id>，而 $HOME/.go-code
	// 是**默认**敏感路径（含 settings/密钥）—— 默认 deny 不应剔除显式的重开。
	home := t.TempDir()
	t.Setenv("HOME", home)
	hub := filepath.Join(home, ".go-code", "hub", "agents", "ag_x")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := openSubpathsPolicy(t, hub, nil)

	baseReal := resolveRealPath(filepath.Join(home, ".go-code"))
	hubReal := resolveRealPath(hub)
	denyLine := `(deny file-write* (subpath "` + baseReal + `"))`
	allowLine := `(allow file-write* (subpath "` + hubReal + `"))`
	di := strings.Index(policy, denyLine)
	ai := strings.LastIndex(policy, allowLine)
	if di < 0 {
		t.Fatalf("policy missing deny for %s", baseReal)
	}
	if ai < 0 {
		t.Fatalf("policy missing re-open allow for %s", hubReal)
	}
	if ai < di {
		t.Fatal("re-open allow must come AFTER the sensitive deny (SBPL: last matching rule wins)")
	}
}

func TestOpenSubpathsExplicitDenyWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hub := filepath.Join(home, ".go-code", "hub", "agents", "ag_x")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 用户显式把 .go-code 列为敏感路径 → 重开必须被剔除（显式 deny 意图优先）。
	policy := openSubpathsPolicy(t, hub, []string{filepath.Join(home, ".go-code")})
	hubReal := resolveRealPath(hub)
	denyLine := `(deny file-write* (subpath "` + resolveRealPath(filepath.Join(home, ".go-code")) + `"))`
	di := strings.Index(policy, denyLine)
	if di < 0 {
		t.Fatalf("policy missing explicit deny %s", denyLine)
	}
	// 只看 deny 之后：deny 之前的 workspace allow 是正常放行（workspace == hub），不算重开。
	if strings.Contains(policy[di:], `(allow file-write* (subpath "`+hubReal+`"))`) {
		t.Fatal("explicit sensitive deny must win over the open subpath")
	}
}

// tools.bash_roots：额外根只读 —— 有 file-read* 放行，没有 file-write* 放行。
func TestReadOnlyPathsAllowReadNotWrite(t *testing.T) {
	if !Available() {
		t.Skip("seatbelt not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	hub := filepath.Join(home, ".go-code", "hub", "agents", "ag_x")
	ro := filepath.Join(home, "hai-workspaces")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSeatbelt(hub, WithOpenSubpaths(hub), WithReadOnlyPaths(ro), WithNoProbe())
	if err != nil {
		t.Fatalf("NewSeatbelt: %v", err)
	}
	roReal := resolveRealPath(ro)
	if !strings.Contains(sb.Policy, `(allow file-read* (subpath "`+roReal+`"))`) {
		t.Fatalf("read-only root %s missing read allow", roReal)
	}
	if strings.Contains(sb.Policy, `(allow file-write* (subpath "`+roReal+`"))`) {
		t.Fatalf("read-only root %s must not get write access", roReal)
	}
}
