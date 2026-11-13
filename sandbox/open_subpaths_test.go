package sandbox

// WithOpenSubpaths：敏感 deny 之后重开子树（Agent Hub 智能体 home 在 ~/.go-code 下）。
// SBPL 后规则胜，所以顺序错了等于没开 —— 这里用策略文本的顺序钉住。

import (
	"fmt"
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
	// 模拟真实布局：智能体 home 在 $HOME/.go-code/hub/homes/<id>，而 $HOME/.go-code
	// 是**默认**敏感路径（含 settings/密钥）—— 默认 deny 不应剔除显式的重开。
	home := t.TempDir()
	t.Setenv("HOME", home)
	hub := filepath.Join(home, ".go-code", "hub", "homes", "ag_x")
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
	hub := filepath.Join(home, ".go-code", "hub", "homes", "ag_x")
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
	hub := filepath.Join(home, ".go-code", "hub", "homes", "ag_x")
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
	// 只发 read-allow 不够：根落在 home/workspace 里时父级 write-allow 仍然生效，
	// 必须显式 write-deny。
	if !strings.Contains(sb.Policy, `(deny file-write* (subpath "`+roReal+`"))`) {
		t.Fatalf("read-only root %s must get an explicit write deny", roReal)
	}
}

// 只读根是敏感 deny 的祖先（如 bash_roots=["~"]）：读 allow 必须排在 deny **之前**，
// 否则后置 allow 会把 ~/.go-code（settings/密钥）重新打开。
func TestReadOnlyRootCannotExposeSensitivePaths(t *testing.T) {
	if !Available() {
		t.Skip("seatbelt not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	hub := filepath.Join(home, ".go-code", "hub", "homes", "ag_x")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSeatbelt(hub, WithOpenSubpaths(hub), WithReadOnlyPaths("~"), WithNoProbe())
	if err != nil {
		t.Fatalf("NewSeatbelt: %v", err)
	}
	homeReal := resolveRealPath(home)
	sensitive := resolveRealPath(filepath.Join(home, ".go-code"))
	readAllow := `(allow file-read* (subpath "` + homeReal + `"))`
	denyRead := `(deny file-read* (subpath "` + sensitive + `"))`
	ai := strings.Index(sb.Policy, readAllow)
	di := strings.Index(sb.Policy, denyRead)
	if ai < 0 || di < 0 {
		t.Fatalf("policy missing allow(ai=%d) or deny(di=%d)", ai, di)
	}
	if ai > di {
		t.Fatal("read-only root allow must come BEFORE the sensitive deny (deny wins)")
	}
}

// 只读根的写 deny 必须排在受管放行子树（智能体 home）之前，否则 home 也一起变只读。
func TestReadOnlyWriteDenyBeforeOpenSubpaths(t *testing.T) {
	if !Available() {
		t.Skip("seatbelt not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	hub := filepath.Join(home, ".go-code", "hub", "homes", "ag_x")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSeatbelt(hub, WithOpenSubpaths(hub), WithReadOnlyPaths("~"), WithNoProbe())
	if err != nil {
		t.Fatalf("NewSeatbelt: %v", err)
	}
	homeReal := resolveRealPath(home)
	hubReal := resolveRealPath(hub)
	wi := strings.Index(sb.Policy, `(deny file-write* (subpath "`+homeReal+`"))`)
	oi := strings.LastIndex(sb.Policy, `(allow file-write* (subpath "`+hubReal+`"))`)
	if wi < 0 || oi < 0 {
		t.Fatalf("policy missing write-deny(wi=%d) or hub reopen(oi=%d)", wi, oi)
	}
	if wi > oi {
		t.Fatal("read-only write deny must come BEFORE the managed open subpath allow")
	}
}

// 显式敏感路径若是 OpenSubpaths 的**子目录**，末尾的显式 deny 必须压过后置 allow。
// 这是 P1-01 复审的残留 blocking：OpenSubpaths 会把整棵子树重开（智能体 home 就在
// ~/.go-code 下），而 SBPL 后规则胜 —— 只在前面 deny 一次不够。
func TestExplicitSensitiveChildDeniedAfterOpenSubpaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hub := filepath.Join(home, ".go-code", "hub", "homes", "ag_x")
	secret := filepath.Join(hub, "secrets")
	if !Available() {
		t.Skip("seatbelt not available")
	}
	sb, err := NewSeatbelt(hub, WithOpenSubpaths(hub), WithSensitivePaths([]string{secret}), WithNoProbe())
	if err != nil {
		t.Fatalf("NewSeatbelt: %v", err)
	}
	policy := sb.Policy
	lastDeny := strings.LastIndex(policy, fmt.Sprintf("(deny file-read* (subpath %q))", secret))
	openAllow := strings.LastIndex(policy, fmt.Sprintf("(allow file-read* (subpath %q))", hub))
	if lastDeny < 0 || openAllow < 0 {
		t.Fatalf("missing deny/allow:\n%s", policy)
	}
	if lastDeny < openAllow {
		t.Fatalf("explicit deny must come after open-subpath allow (deny=%d allow=%d):\n%s", lastDeny, openAllow, policy)
	}
	if !strings.Contains(policy, fmt.Sprintf("(deny file-write* (subpath %q))", secret)) {
		t.Fatalf("explicit write deny missing:\n%s", policy)
	}
}
