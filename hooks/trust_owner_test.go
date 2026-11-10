package hooks

// owner 隔离回归（BR-21 产品决策）：信任库的每一条确认都属于某个 owner
// （workspace:<path> / agent:<id> / group:<id>），互不继承；旧的全局条目迁移到 legacy，
// 且**只对 workspace 维**兼容生效。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/owner"
)

func tempTrustPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "hooks-state.json")
}

// TestTrustIsOwnerScoped 是本条的核心负例：A 的确认不得让 B 免确认。
func TestTrustIsOwnerScoped(t *testing.T) {
	ts, err := LoadTrust(tempTrustPath(t))
	if err != nil {
		t.Fatal(err)
	}
	agentA, agentB := owner.Agent("a"), owner.Agent("b")
	groupG, groupG2 := owner.Group("g1"), owner.Group("g2")
	ws := owner.Workspace("/proj/x")

	if err := ts.Trust(agentA, "echo hi", "project", "agent a"); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if got := ts.Check(agentA, "echo hi"); got != TrustTrusted {
		t.Fatalf("同 owner 必须 trusted，得到 %q", got)
	}
	for _, other := range []string{agentB, groupG, groupG2, ws} {
		if got := ts.Check(other, "echo hi"); got != TrustUntrusted {
			t.Fatalf("%s 不得继承 %s 的确认（得到 %q）", other, agentA, got)
		}
	}

	// 同一 agent 的 DM 与群聊按不同 owner 处理（产品决策：不共享）：
	// agent:a 的确认不影响 group:g1，反之亦然。
	if err := ts.Trust(groupG, "echo grp", "project", "group g1"); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if got := ts.Check(groupG, "echo grp"); got != TrustTrusted {
		t.Fatalf("群聊自己的确认必须生效，得到 %q", got)
	}
	if got := ts.Check(agentA, "echo grp"); got != TrustUntrusted {
		t.Fatalf("群聊确认不得让该群的 agent DM 免确认，得到 %q", got)
	}
	if got := ts.Check(groupG2, "echo grp"); got != TrustUntrusted {
		t.Fatalf("别的群不得继承，得到 %q", got)
	}
}

// TestTrustRequiresValidOwner 写路径拒绝 legacy / 空 / 未知前缀：不允许「不知道属于谁」
// 的确认落盘（那是把刚拆掉的全局语义装回去）。
func TestTrustRequiresValidOwner(t *testing.T) {
	ts, _ := LoadTrust(tempTrustPath(t))
	for _, bad := range []string{"", owner.Legacy, "global", "workspace:", "agent:", "Agent:a"} {
		if err := ts.Trust(bad, "echo hi", "user", "x"); err == nil {
			t.Fatalf("owner %q 必须被拒绝", bad)
		}
	}
	// 空 owner 的查询一律未信任（派发时缺归属 → 不执行，而不是猜一个）。
	if got := ts.Check("", "echo hi"); got != TrustUntrusted {
		t.Fatalf("空 owner 必须未信任，得到 %q", got)
	}
}

// TestTrustLegacyMigration 旧格式（v1，全局平铺）迁移规则：
//   - 条目落到 legacy owner，不套到 agent/group；
//   - 只有 workspace 维经兼容路径生效（升级后项目会话照常工作）；
//   - 文件在下次写入前保持原样（不因读一下就改用户文件），写入后是 v2 且保留旧条目。
func TestTrustLegacyMigration(t *testing.T) {
	path := tempTrustPath(t)
	v1 := map[string]any{
		"entries": map[string]any{
			"echo hi":  map[string]any{"hash": hashCommand("echo hi"), "scope": "project", "reason": "旧确认"},
			"echo old": map[string]any{"hash": "sha256:deadbeefdeadbeef", "scope": "project"},
		},
		"managed": []string{},
	}
	raw, _ := json.MarshalIndent(v1, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	ts, err := LoadTrust(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ts.Migrated() {
		t.Fatal("读到 v1 必须标记为已迁移")
	}
	// 项目工作区：兼容生效（旧确认本来就是为了项目级 hook 建立的）。
	if got := ts.Check(owner.Workspace("/proj/x"), "echo hi"); got != TrustTrusted {
		t.Fatalf("workspace 维应兼容旧全局确认，得到 %q", got)
	}
	// agent / group：**不**继承旧全局确认（必须重新确认）。
	for _, own := range []string{owner.Agent("a"), owner.Group("g")} {
		if got := ts.Check(own, "echo hi"); got != TrustUntrusted {
			t.Fatalf("%s 不得套用旧全局确认，得到 %q", own, got)
		}
	}
	// 内容哈希不符 → modified（旧条目仍参与判定，只是提示要重新确认）。
	if got := ts.Check(owner.Workspace("/proj/x"), "echo old"); got != TrustModified {
		t.Fatalf("哈希不符应为 modified，得到 %q", got)
	}
	// 迁移只读：还没写过，文件仍是 v1 原样。
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("加载不得改写文件（只在下次确认时落 v2）")
	}
	// 面板提示用：legacy-only 判定
	if !ts.LegacyOnly(owner.Workspace("/proj/x"), "echo hi") {
		t.Fatal("legacy 兼容生效的命令应被标记 legacyOnly")
	}
	if ts.LegacyOnly(owner.Agent("a"), "echo hi") {
		t.Fatal("agent 维不参与 legacy 兼容")
	}

	// 写一条新确认 → 落 v2，且旧条目（legacy）仍在文件里，不丢历史。
	if err := ts.Trust(owner.Agent("a"), "echo hi", "project", "agent a"); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	raw2, _ := os.ReadFile(path)
	var f trustFileV2
	if err := json.Unmarshal(raw2, &f); err != nil {
		t.Fatalf("落盘必须是合法 v2: %v\n%s", err, raw2)
	}
	if f.Version != TrustStoreOwnerVersion {
		t.Fatalf("version = %d, want %d", f.Version, TrustStoreOwnerVersion)
	}
	if _, ok := f.Entries[owner.Legacy]["echo hi"]; !ok {
		t.Fatal("v2 必须保留 legacy 条目（迁移不删历史）")
	}
	if _, ok := f.Entries[owner.Agent("a")]["echo hi"]; !ok {
		t.Fatal("v2 必须有 agent:a 的确认")
	}

	// 重新加载：agent 确认生效，workspace 仍走 legacy 兼容，group 仍不生效。
	ts2, err := LoadTrust(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ts2.Check(owner.Agent("a"), "echo hi"); got != TrustTrusted {
		t.Fatalf("重载后 agent 确认应生效，得到 %q", got)
	}
	if got := ts2.Check(owner.Workspace("/proj/y"), "echo hi"); got != TrustTrusted {
		t.Fatalf("重载后 workspace 维仍应兼容 legacy，得到 %q", got)
	}
	if got := ts2.Check(owner.Group("g"), "echo hi"); got != TrustUntrusted {
		t.Fatalf("重载后 group 仍不得继承，得到 %q", got)
	}
}

// TestTrustCorruptFileIsEmpty 坏文件：空库（全部未信任），不影响启动。
func TestTrustCorruptFileIsEmpty(t *testing.T) {
	path := tempTrustPath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts, err := LoadTrust(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ts.Check(owner.Agent("a"), "echo hi"); got != TrustUntrusted {
		t.Fatalf("坏文件应保守判未信任，得到 %q", got)
	}
	if ts.Migrated() {
		t.Fatal("坏文件不算迁移")
	}
}

// TestTrustHashStillGuardsContent 内容哈希语义不变：命令改一个字符 → modified。
func TestTrustHashStillGuardsContent(t *testing.T) {
	ts, _ := LoadTrust(tempTrustPath(t))
	if err := ts.Trust(owner.Group("g"), "echo hi", "project", "x"); err != nil {
		t.Fatal(err)
	}
	if got := ts.Check(owner.Group("g"), "echo hi "); got != TrustUntrusted {
		t.Fatalf("不同命令应是 untrusted，得到 %q", got)
	}
	if err := ts.Trust(owner.Group("g"), "echo hi", "project", "x"); err != nil {
		t.Fatal(err)
	}
	if got := ts.Check(owner.Group("g"), "echo hi"); got != TrustTrusted {
		t.Fatalf("同 owner 同命令应 trusted，得到 %q", got)
	}
}
