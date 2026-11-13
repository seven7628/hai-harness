package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noteCfg(now time.Time) Config {
	return Config{Injection: InjectionFull, Now: func() time.Time { return now }}
}

// 盘点结论要出现在注入里，且**只用事实**说话（数字 + 去哪看全文）。
func TestDreamNoteAppearsInInjection(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")

	cfg := noteCfg(now)
	if err := cfg.SaveDreamNote(d, DreamNote{
		At: now.Add(-2 * time.Hour), Topics: 12, DeadAnchors: 6, NotIndexed: 2,
	}); err != nil {
		t.Fatal(err)
	}

	inj, err := cfg.BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Dream == "" {
		t.Fatalf("注入里没有带盘点结论：\n%s", inj.Text)
	}
	for _, want := range []string{"6 条代码锚点失效", "2 条记忆没进索引", "2 小时前"} {
		if !strings.Contains(inj.Text, want) {
			t.Fatalf("结论缺少 %q：\n%s", want, inj.Text)
		}
	}
	// 只说事实，不说建议。
	for _, bad := range []string{"建议", "应该", "请清理"} {
		if strings.Contains(inj.Dream, bad) {
			t.Fatalf("结论里混入了建议 %q：%s", bad, inj.Dream)
		}
	}
}

// 两周前的"6 条失效"比不说更糟：用户会以为它说的是现在。
func TestDreamNoteExpires(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")

	cfg := noteCfg(now)
	if err := cfg.SaveDreamNote(d, DreamNote{At: now.Add(-20 * 24 * time.Hour), DeadAnchors: 6}); err != nil {
		t.Fatal(err)
	}
	inj, err := cfg.BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Dream != "" || strings.Contains(inj.Text, "代码锚点失效") {
		t.Fatalf("过期结论不该出现：%q", inj.Dream)
	}
}

// 没问题的盘点不该占一行——每会话固定开销，噪声也是成本。
func TestDreamNoteCleanSaysNothing(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")

	cfg := noteCfg(now)
	if err := cfg.SaveDreamNote(d, DreamNote{At: now.Add(-time.Hour), Topics: 3}); err != nil {
		t.Fatal(err)
	}
	inj, err := cfg.BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Dream != "" {
		t.Fatalf("干净盘点不该说话：%q", inj.Dream)
	}
}

// 摘要文件损坏/缺字段 → 当作没有，不要报错也不要注入半句。
func TestDreamNoteCorruptIsIgnored(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")
	if err := os.MkdirAll(filepath.Join(d.Path, stateDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dreamNotePath(d), []byte("{ 半行"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := noteCfg(time.Unix(1700000000, 0).UTC())
	if _, ok := cfg.LoadDreamNote(d); ok {
		t.Fatal("损坏的摘要不该被读出来")
	}
	inj, err := cfg.BuildInjection(d)
	if err != nil || inj.Dream != "" {
		t.Fatalf("损坏摘要不应影响注入：err=%v dream=%q", err, inj.Dream)
	}
}

// 真正跑一遍只读盘点并落盘：这是"自动 Dream"能被验证的最小闭环。
func TestRunDreamAndSave(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")
	if err := os.WriteFile(filepath.Join(d.Path, "a.md"),
		[]byte("---\ntype: project\n---\n见 `nope/missing.go`\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := noteCfg(now)
	// 会话转录要扫真实事件目录（5.4GB）：测试里换成空目录，否则这一条要 8 秒。
	cfg.EventsRoot = t.TempDir()
	n, err := cfg.RunDreamAndSave(d)
	if err != nil {
		t.Fatal(err)
	}
	if n.Topics != 1 {
		t.Fatalf("Topics = %d，期望 1", n.Topics)
	}
	// 没绑定仓库时锚点整段跳过：**跳过不等于没问题**，摘要要把这件事记下来。
	if !n.Unbound {
		t.Fatalf("未绑定仓库时应标 Unbound，得到 %+v", n)
	}
	back, ok := cfg.LoadDreamNote(d)
	if !ok || back.Topics != n.Topics {
		t.Fatalf("落盘的摘要读不回来：%+v", back)
	}
}

// DreamDue：没跑过 → 该跑；12 小时内 → 不跑；过了就再跑。
func TestDreamDue(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := memDir(t)
	cfg := noteCfg(now)

	if !cfg.DreamDue(d) {
		t.Fatal("从未盘点过，应当判定该跑")
	}
	_ = cfg.SaveDreamNote(d, DreamNote{At: now.Add(-time.Hour)})
	if cfg.DreamDue(d) {
		t.Fatal("1 小时前刚跑过，不该再跑")
	}
	_ = cfg.SaveDreamNote(d, DreamNote{At: now.Add(-13 * time.Hour)})
	if !cfg.DreamDue(d) {
		t.Fatal("13 小时前跑过，应当再跑")
	}
}

// 零值 Config 必须能用：RunDreamAndSave / DreamDue 会先取 Now()，而它默认是 nil。
// 这条是实测踩出来的——真实调用点传的正是 Config{}，第一版直接 panic。
func TestDreamNoteZeroConfigDoesNotPanic(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")

	var cfg Config // 全部字段零值
	cfg.EventsRoot = t.TempDir()

	if !cfg.DreamDue(d) {
		t.Fatal("零值 Config 应判定该跑")
	}
	if _, err := cfg.RunDreamAndSave(d); err != nil {
		t.Fatal(err)
	}
	if cfg.DreamDue(d) {
		t.Fatal("刚跑完不该再跑")
	}
}

// "每天最多一次"落地为：今天没归档过 + 配了回归命令 → 提示升级成待办；
// 否则只报告现状，不催。
func TestDreamTaskAppearsOnlyWhenActionable(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := memDir(t)
	writeIndex(t, d, "- [a.md](a.md) — 甲\n")
	cfg := noteCfg(now)
	if err := cfg.SaveDreamNote(d, DreamNote{At: now.Add(-time.Hour), DeadAnchors: 8}); err != nil {
		t.Fatal(err)
	}

	// 没配回归命令：只报告，不催归档（因为归档后没法验证）。
	t.Setenv(EnvDreamEvalCommand, "")
	inj, _ := cfg.BuildInjection(d)
	if inj.Dream == "" || inj.DreamTask != "" {
		t.Fatalf("没配回归命令时不该出现待办：dream=%q task=%q", inj.Dream, inj.DreamTask)
	}

	// 配了：出现待办，且指向工具与命令。
	t.Setenv(EnvDreamEvalCommand, "python3 eval.py")
	inj, _ = cfg.BuildInjection(d)
	if inj.DreamTask == "" {
		t.Fatal("配了回归命令后应出现待办")
	}
	for _, want := range []string{"memory_dream", "python3 eval.py", "最多 2 条"} {
		if !contains(inj.DreamTask, want) {
			t.Fatalf("待办缺少 %q：%s", want, inj.DreamTask)
		}
	}

	// 今天已经归档过：不再催。
	if err := cfg.MarkAutoDream(d, 1); err != nil {
		t.Fatal(err)
	}
	inj, _ = cfg.BuildInjection(d)
	if inj.DreamTask != "" {
		t.Fatalf("今天已归档过，不该再催：%q", inj.DreamTask)
	}
}
