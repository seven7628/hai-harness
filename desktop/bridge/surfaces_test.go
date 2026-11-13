// surfaces_test.go —— `~/.go-code/surfaces/` 持久层（PR-13 A）。
//
// 全部从**生产入口**进（00-conventions §4.7）：`HOME` 指到临时目录 → `appDataDir()`
// → `newSurfaceStore("")`，与 main.go 装配时同一条构造路径。不另建「等价目录」来测。
//
// 断言的重点不是「文件写出来了」，而是四条不变式：
//  1. 失败**一个字节都不改** index（半装状态比装不上更糟）；
//  2. 卸载**不删目录**，只挪到 `.trash/<id>-<ts>`（给手滑一次后悔的机会）；
//  3. `index.json.bak` 是**变更前**的内容（写成变更后 = 回滚点指向坏状态）；
//  4. 并发两个 install 都成功且 index 仍是合法 JSON（进程内 mutex + flock）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 夹具 ----

// testSurfaceStore 用临时 HOME 构造生产路径上的 store（appDataDirOverride 清零，
// 否则会盖掉 HOME —— 那等于测了另一条路径，§4.7 的 F53 形态）。
func testSurfaceStore(t *testing.T) (*surfaceStore, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows 的 os.UserHomeDir 读它
	prev := appDataDirOverride
	appDataDirOverride = ""
	t.Cleanup(func() { appDataDirOverride = prev })

	st := newSurfaceStore("")
	root := st.Dir()
	if want := filepath.Join(home, ".go-code", "surfaces"); root != want {
		t.Fatalf("store 根目录 = %q，want %q（必须走 appDataDir 的生产解析）", root, want)
	}
	return st, root
}

// surfaceFixture 造一个最小合法插件目录（surface.json + panel.js），返回绝对路径。
// mutate 可改 manifest；noPanel = 不写 panel.js（「缺 panel.js」用例）。
type fixtureOpt func(dir string, m map[string]any)

func surfaceFixture(t *testing.T, id, version, panel string, opts ...fixtureOpt) string {
	t.Helper()
	dir := t.TempDir()
	m := map[string]any{
		"id":      id,
		"name":    map[string]any{"zh": "成本看板", "en": "Cost board"},
		"version": version,
		"api":     1,
		"kind":    "plugin:" + id,
		"entry":   "panel.js",
		"icon":    "icon.svg",
	}
	for _, o := range opts {
		o(dir, m)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "surface.json"), raw, 0o600); err != nil {
		t.Fatalf("写 surface.json: %v", err)
	}
	if panel != "" {
		if err := os.WriteFile(filepath.Join(dir, "panel.js"), []byte(panel), 0o600); err != nil {
			t.Fatalf("写 panel.js: %v", err)
		}
	}
	return dir
}

func withManifest(fn func(m map[string]any)) fixtureOpt {
	return func(_ string, m map[string]any) { fn(m) }
}

func withSymlink(t *testing.T, name, target string) fixtureOpt {
	t.Helper()
	return func(dir string, _ map[string]any) {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatalf("建软链 %s: %v", name, err)
		}
	}
}

func withFile(t *testing.T, name, content string) fixtureOpt {
	t.Helper()
	return func(dir string, _ map[string]any) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
}

func readBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return b
}

func notExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("%s 不该存在（err=%v）", p, err)
	}
}

func wantMode(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Fatalf("%s 权限 = %o，want %o（目录 0700 / 文件 0600）", p, got, want)
	}
}

// globOne 返回唯一匹配；没有或多于一个都判失败（多说明留下了垃圾目录）。
func globOne(t *testing.T, pattern string) string {
	t.Helper()
	got, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	if len(got) != 1 {
		t.Fatalf("glob %s = %v，want 恰好 1 个", pattern, got)
	}
	return got[0]
}

func noStagingLeftovers(t *testing.T, root string) {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(root, ".install-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("留下了暂存目录：%v", got)
	}
}

// readIndex 解出 index.json 的原始 JSON（顺序与字段都按磁盘上那份算）。
func readIndex(t *testing.T, root string) (raw []byte, idx struct {
	Version int              `json:"version"`
	Items   []map[string]any `json:"items"`
}) {
	t.Helper()
	raw = readBytes(t, filepath.Join(root, "index.json"))
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("index.json 不是合法 JSON: %v\n%s", err, raw)
	}
	return raw, idx
}

// ---- 安装：index 内容 / 顺序 / hash / 权限 / 文件真的拷过去 ----

func TestSurfaceInstallWritesIndexHashAndPerms(t *testing.T) {
	st, root := testSurfaceStore(t)
	panel := "export default function Panel() { return null }\n"
	src := surfaceFixture(t, "cost-board", "0.1.0", panel)

	start := time.Now()
	rec := st.Install(src, true)
	elapsed := time.Since(start)
	if rec.Status != surfaceApplied {
		t.Fatalf("status = %q，want %q（reason=%q）", rec.Status, surfaceApplied, rec.Reason)
	}
	// P14 的一半（安装/卸载磁盘路径，预算 150ms）；另一半是渲染层 loader，PR-14 才有。
	t.Logf("P14 install 耗时 = %v（预算 150ms）", elapsed)
	if elapsed > 150*time.Millisecond {
		t.Fatalf("P14 超预算：install = %v > 150ms", elapsed)
	}
	if rec.ID != "cost-board" || rec.Version != "0.1.0" {
		t.Fatalf("回执 id/version = %q/%q", rec.ID, rec.Version)
	}
	wantHash := sha256Hex(panel)
	if rec.Hash != wantHash {
		t.Fatalf("回执 hash = %q，want sha256(panel.js) = %q", rec.Hash, wantHash)
	}

	raw, idx := readIndex(t, root)
	if idx.Version != 1 {
		t.Fatalf("index version = %d，want 1（%s）", idx.Version, raw)
	}
	if len(idx.Items) != 1 {
		t.Fatalf("items = %d，want 1：%s", len(idx.Items), raw)
	}
	it := idx.Items[0]
	if it["id"] != "cost-board" || it["version"] != "0.1.0" || it["hash"] != wantHash {
		t.Fatalf("item = %v", it)
	}
	if it["enabled"] != true {
		t.Fatalf("enabled = %v，want true（enable 缺省）", it["enabled"])
	}
	if got, _ := it["source"].(string); got != src {
		t.Fatalf("source = %q，want 源目录绝对路径 %q", got, src)
	}
	if ts, _ := it["installedAt"].(string); ts == "" {
		t.Fatalf("installedAt 为空：%v", it)
	}

	// 文件真的拷过去了（不是只写了一行 index）。
	gotPanel := string(readBytes(t, filepath.Join(root, "cost-board", "panel.js")))
	if gotPanel != panel {
		t.Fatalf("拷过去的 panel.js = %q，want %q", gotPanel, panel)
	}
	if _, err := os.Stat(filepath.Join(root, "cost-board", "surface.json")); err != nil {
		t.Fatalf("manifest 没拷过去: %v", err)
	}

	// 权限：目录 0700 / 文件 0600（index、manifest、panel.js、audit 都一样）。
	wantMode(t, root, 0o700)
	wantMode(t, filepath.Join(root, "index.json"), 0o600)
	wantMode(t, filepath.Join(root, "cost-board"), 0o700)
	wantMode(t, filepath.Join(root, "cost-board", "surface.json"), 0o600)
	wantMode(t, filepath.Join(root, "cost-board", "panel.js"), 0o600)
	wantMode(t, filepath.Join(root, "audit.jsonl"), 0o600)

	noStagingLeftovers(t, root)
}

// 顺序：index 是有序列表，升级不改变位置，新装的追加在末尾。
func TestSurfaceInstallKeepsIndexOrder(t *testing.T) {
	st, root := testSurfaceStore(t)
	a := surfaceFixture(t, "alpha", "1.0.0", "export default () => 'a'\n")
	b := surfaceFixture(t, "bravo", "1.0.0", "export default () => 'b'\n")
	if r := st.Install(a, true); r.Status != surfaceApplied {
		t.Fatalf("alpha: %+v", r)
	}
	if r := st.Install(b, true); r.Status != surfaceApplied {
		t.Fatalf("bravo: %+v", r)
	}
	// 升级 alpha（换版本 + 换内容）后位置必须仍是第 0 条。
	upgraded := surfaceFixture(t, "alpha", "1.1.0", "export default () => 'a2'\n")
	if r := st.Install(upgraded, true); r.Status != surfaceApplied {
		t.Fatalf("upgrade: %+v", r)
	}

	_, idx := readIndex(t, root)
	if len(idx.Items) != 2 {
		t.Fatalf("items = %d，want 2（升级不许产生第二条）", len(idx.Items))
	}
	if idx.Items[0]["id"] != "alpha" || idx.Items[1]["id"] != "bravo" {
		t.Fatalf("顺序 = [%v %v]，want [alpha bravo]", idx.Items[0]["id"], idx.Items[1]["id"])
	}
	if idx.Items[0]["version"] != "1.1.0" {
		t.Fatalf("升级后 version = %v，want 1.1.0", idx.Items[0]["version"])
	}
}

// ---- 幂等：同 hash 第二次 = already_installed，一个字节都不改 ----

func TestSurfaceInstallSameHashIsAlreadyInstalled(t *testing.T) {
	st, root := testSurfaceStore(t)
	panel := "export default () => null\n"
	src := surfaceFixture(t, "cost-board", "0.1.0", panel)
	if r := st.Install(src, true); r.Status != surfaceApplied {
		t.Fatalf("首次安装: %+v", r)
	}
	before := readBytes(t, filepath.Join(root, "index.json"))

	rec := st.Install(src, true)
	if rec.Status != surfaceAlreadyInstalled {
		t.Fatalf("第二次 status = %q，want %q（reason=%q）", rec.Status, surfaceAlreadyInstalled, rec.Reason)
	}
	if rec.ID != "cost-board" || rec.Hash != sha256Hex(panel) {
		t.Fatalf("already_installed 回执 = %+v（必须仍能拿到 id/hash）", rec)
	}
	after := readBytes(t, filepath.Join(root, "index.json"))
	if string(before) != string(after) {
		t.Fatalf("already_installed 改了 index：\nbefore=%s\nafter=%s", before, after)
	}
	noStagingLeftovers(t, root)
}

// ---- 校验失败五连 / 七连：index 一个字节没变、不留半个目录 ----

func TestSurfaceInstallRejectsBadManifests(t *testing.T) {
	cases := []struct {
		name   string
		fix    func(t *testing.T) string
		reason string // reason 里必须出现的子串（可读、能指向问题）
	}{
		{
			name: "缺 id",
			fix: func(t *testing.T) string {
				return surfaceFixture(t, "x", "1.0.0", "a\n", withManifest(func(m map[string]any) { delete(m, "id") }))
			},
			reason: "id",
		},
		{
			name: "id 非法（大写+空格）",
			fix: func(t *testing.T) string {
				return surfaceFixture(t, "Cost Board", "1.0.0", "a\n")
			},
			reason: "id",
		},
		{
			name: "entry 逃逸 ../x.js",
			fix: func(t *testing.T) string {
				return surfaceFixture(t, "escape-dotdot", "1.0.0", "a\n",
					withManifest(func(m map[string]any) { m["entry"] = "../x.js" }))
			},
			reason: "entry",
		},
		{
			name: "entry 绝对路径",
			fix: func(t *testing.T) string {
				return surfaceFixture(t, "escape-abs", "1.0.0", "a\n",
					withManifest(func(m map[string]any) { m["entry"] = "/etc/passwd" }))
			},
			reason: "entry",
		},
		{
			name: "缺 panel.js",
			fix: func(t *testing.T) string {
				return surfaceFixture(t, "no-panel", "1.0.0", "") // panel 为空 = 不写
			},
			reason: "panel.js",
		},
		{
			name: "panel.js 超 256 KiB",
			fix: func(t *testing.T) string {
				return surfaceFixture(t, "too-big", "1.0.0", strings.Repeat("x", 256<<10+1))
			},
			reason: "256",
		},
		{
			// 校验能过（入口是常规文件、体积合规），死在**拷贝**这一步：
			// 这条是「先拷到 .install-* 再 rename」的唯一可观测点 —— 没有暂存目录
			// 就会在 <id> 下留半个插件。
			name: "目录里混了符号链接（死在拷贝步）",
			fix: func(t *testing.T) string {
				return surfaceFixture(t, "link-in-dir", "1.0.0", "a\n",
					withSymlink(t, "extra-link", "/etc/passwd"))
			},
			reason: "符号链接",
		},
		{
			name: "整目录超 1 MiB",
			fix: func(t *testing.T) string {
				return surfaceFixture(t, "dir-too-big", "1.0.0", "a\n",
					withFile(t, "big.bin", strings.Repeat("y", 1<<20+1)))
			},
			reason: "1 MiB",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, root := testSurfaceStore(t)
			// 先装一个合法的：让「index 一个字节没变」有真内容可比（空文件不存在是弱断言）。
			okSrc := surfaceFixture(t, "baseline", "1.0.0", "export default () => null\n")
			if r := st.Install(okSrc, true); r.Status != surfaceApplied {
				t.Fatalf("基线安装失败: %+v", r)
			}
			before := readBytes(t, filepath.Join(root, "index.json"))
			entriesBefore, _ := os.ReadDir(root)

			bad := c.fix(t)
			rec := st.Install(bad, true)
			if rec.Status != surfaceRejected {
				t.Fatalf("status = %q，want %q（reason=%q）", rec.Status, surfaceRejected, rec.Reason)
			}
			if !strings.Contains(rec.Reason, c.reason) {
				t.Fatalf("reason = %q，必须包含 %q（可读、指向具体问题）", rec.Reason, c.reason)
			}

			after := readBytes(t, filepath.Join(root, "index.json"))
			if string(before) != string(after) {
				t.Fatalf("校验失败却改了 index：\nbefore=%s\nafter=%s", before, after)
			}
			// 没有半个目录：根目录条目集合不变（只可能多出 audit.jsonl）。
			entriesAfter, _ := os.ReadDir(root)
			for _, e := range entriesAfter {
				found := false
				for _, b := range entriesBefore {
					if b.Name() == e.Name() {
						found = true
						break
					}
				}
				if !found && e.Name() != "audit.jsonl" {
					t.Fatalf("校验失败后多出了 %q（不许留半个目录/暂存目录）", e.Name())
				}
			}
			noStagingLeftovers(t, root)
		})
	}
}

// ---- 卸载：进 .trash，不直接删 ----

func TestSurfaceRemoveMovesToTrash(t *testing.T) {
	st, root := testSurfaceStore(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")
	if r := st.Install(src, true); r.Status != surfaceApplied {
		t.Fatalf("安装: %+v", r)
	}

	start := time.Now()
	rec := st.Remove("cost-board")
	elapsed := time.Since(start)
	if rec.Status != surfaceApplied {
		t.Fatalf("卸载 status = %q，want applied（reason=%q）", rec.Status, rec.Reason)
	}
	// P14（安装/卸载预算 150ms 的一半；loader 那一半在 PR-14）：磁盘操作不该接近预算。
	t.Logf("P14 remove 耗时 = %v（预算 150ms）", elapsed)
	if elapsed > 150*time.Millisecond {
		t.Fatalf("P14 超预算：remove = %v > 150ms", elapsed)
	}

	_, idx := readIndex(t, root)
	if len(idx.Items) != 0 {
		t.Fatalf("卸载后 index 还有 %d 条：%v", len(idx.Items), idx.Items)
	}
	notExist(t, filepath.Join(root, "cost-board"))

	trash := globOne(t, filepath.Join(root, ".trash", "cost-board-*"))
	if _, err := os.Stat(filepath.Join(trash, "surface.json")); err != nil {
		t.Fatalf(".trash 里没有 manifest（目录被删了？）: %v", err)
	}
	if _, err := os.Stat(filepath.Join(trash, "panel.js")); err != nil {
		t.Fatalf(".trash 里没有 panel.js: %v", err)
	}
	wantMode(t, filepath.Join(root, ".trash"), 0o700)

	// 幂等：再来一次是 cleaned（记录已没了），不是 error。
	if again := st.Remove("cost-board"); again.Status != surfaceCleaned {
		t.Fatalf("重复卸载 status = %q，want cleaned", again.Status)
	}
}

// 目标不存在但 index 有记录：清记录 + cleaned，而不是留一条幽灵。
func TestSurfaceRemoveRecordWithoutDirCleansUp(t *testing.T) {
	st, root := testSurfaceStore(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")
	if r := st.Install(src, true); r.Status != surfaceApplied {
		t.Fatalf("安装: %+v", r)
	}
	if err := os.RemoveAll(filepath.Join(root, "cost-board")); err != nil {
		t.Fatalf("模拟外部删除: %v", err)
	}
	rec := st.Remove("cost-board")
	if rec.Status != surfaceCleaned {
		t.Fatalf("status = %q，want cleaned", rec.Status)
	}
	_, idx := readIndex(t, root)
	if len(idx.Items) != 0 {
		t.Fatalf("index 还留着幽灵记录：%v", idx.Items)
	}
}

// ---- index 损坏：当空启动、原文件原样备份、下一次 install 后 index 合法 ----

func TestSurfaceIndexCorruptIsQuarantined(t *testing.T) {
	st, root := testSurfaceStore(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")
	if r := st.Install(src, true); r.Status != surfaceApplied {
		t.Fatalf("安装: %+v", r)
	}

	corrupt := []byte(`{"version":1,"items":[{"id":`) // 半截 JSON
	if err := os.WriteFile(filepath.Join(root, "index.json"), corrupt, 0o600); err != nil {
		t.Fatalf("写坏 index: %v", err)
	}

	list := st.List(false)
	if list.Status != surfaceApplied || len(list.Items) != 0 {
		t.Fatalf("损坏 index 应当按空列表启动，得到 %+v", list)
	}

	backup := globOne(t, filepath.Join(root, "index.json.corrupt-*"))
	if got := readBytes(t, backup); string(got) != string(corrupt) {
		t.Fatalf("备份内容被改写：%q vs %q", got, corrupt)
	}

	// 下一次 install 必须写出合法 index（而且是完整的两条之外的新一份）。
	ok2 := surfaceFixture(t, "alpha", "1.0.0", "export default () => 'a'\n")
	if r := st.Install(ok2, true); r.Status != surfaceApplied {
		t.Fatalf("损坏后安装: %+v", r)
	}
	_, idx := readIndex(t, root)
	if len(idx.Items) != 1 || idx.Items[0]["id"] != "alpha" {
		t.Fatalf("损坏后 index = %v，want 只有 alpha", idx.Items)
	}
	if got := readBytes(t, filepath.Join(root, "index.json")); !json.Valid(got) {
		t.Fatalf("index 仍不合法：%s", got)
	}
}

// ---- .bak 是变更前的内容（逐字节）----

func TestSurfaceIndexBakIsPreviousContent(t *testing.T) {
	st, root := testSurfaceStore(t)
	a := surfaceFixture(t, "alpha", "1.0.0", "export default () => 'a'\n")
	b := surfaceFixture(t, "bravo", "1.0.0", "export default () => 'b'\n")
	if r := st.Install(a, true); r.Status != surfaceApplied {
		t.Fatalf("alpha: %+v", r)
	}
	before := readBytes(t, filepath.Join(root, "index.json"))
	if r := st.Install(b, true); r.Status != surfaceApplied {
		t.Fatalf("bravo: %+v", r)
	}

	after := readBytes(t, filepath.Join(root, "index.json"))
	if string(before) == string(after) {
		t.Fatal("两次安装后 index 内容相同 —— .bak 断言会失去意义")
	}
	bak := readBytes(t, filepath.Join(root, "index.json.bak"))
	if string(bak) != string(before) {
		t.Fatalf("index.json.bak 不是变更前内容：\nwant=%s\ngot =%s", before, bak)
	}
}

// ---- 并发：两个 install 都成功、两条都在、JSON 合法、无暂存残留 ----

func TestSurfaceInstallConcurrent(t *testing.T) {
	st, root := testSurfaceStore(t)
	ids := []string{"alpha", "bravo"}
	srcs := make([]string, len(ids))
	for i, id := range ids {
		srcs[i] = surfaceFixture(t, id, "1.0.0", "export default () => '"+id+"'\n")
	}

	var wg sync.WaitGroup
	recs := make([]surfaceReceipt, len(ids))
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = st.Install(srcs[i], true)
		}(i)
	}
	wg.Wait()

	for i, rec := range recs {
		if rec.Status != surfaceApplied {
			t.Fatalf("%s 并发安装 = %q（reason=%q）", ids[i], rec.Status, rec.Reason)
		}
	}
	raw, idx := readIndex(t, root)
	if len(idx.Items) != 2 {
		t.Fatalf("items = %d，want 2：%s", len(idx.Items), raw)
	}
	seen := map[string]bool{}
	for _, it := range idx.Items {
		seen[it["id"].(string)] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("index 缺 %s：%s", id, raw)
		}
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(root, id, "panel.js")); err != nil {
			t.Fatalf("%s 的 panel.js 没落盘: %v", id, err)
		}
	}
	noStagingLeftovers(t, root)
}

// ---- audit：每个动作一行、JSON 合法；写失败不影响主流程 ----

func TestSurfaceAuditLines(t *testing.T) {
	st, root := testSurfaceStore(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")
	if r := st.Install(src, true); r.Status != surfaceApplied {
		t.Fatalf("安装: %+v", r)
	}
	if r := st.Remove("cost-board"); r.Status != surfaceApplied {
		t.Fatalf("卸载: %+v", r)
	}

	raw := strings.TrimSpace(string(readBytes(t, filepath.Join(root, "audit.jsonl"))))
	lines := strings.Split(raw, "\n")
	if len(lines) != 2 {
		t.Fatalf("audit 行数 = %d，want 2：\n%s", len(lines), raw)
	}
	var actions []string
	for _, ln := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("audit 行不是合法 JSON: %q (%v)", ln, err)
		}
		if rec["id"] != "cost-board" {
			t.Fatalf("audit id = %v", rec["id"])
		}
		if rec["ok"] != true {
			t.Fatalf("audit ok = %v", rec["ok"])
		}
		if ts, _ := rec["ts"].(string); ts == "" {
			t.Fatalf("audit 缺 ts：%v", rec)
		}
		actions = append(actions, rec["action"].(string))
	}
	if actions[0] != "install" || actions[1] != "remove" {
		t.Fatalf("audit actions = %v，want [install remove]", actions)
	}
}

func TestSurfaceAuditWriteFailureDoesNotBlockInstall(t *testing.T) {
	st, root := testSurfaceStore(t)
	// 让 audit.jsonl 无法追加：同名路径放一个目录。
	if err := os.MkdirAll(filepath.Join(root, "audit.jsonl"), 0o700); err != nil {
		t.Fatalf("造 audit 障碍: %v", err)
	}
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")
	rec := st.Install(src, true)
	if rec.Status != surfaceApplied {
		t.Fatalf("audit 写不了就让 install 失败 = 主次颠倒：%+v", rec)
	}
	_, idx := readIndex(t, root)
	if len(idx.Items) != 1 {
		t.Fatalf("index 没写：%v", idx.Items)
	}
}

// 首次安装就被拒：目录权限仍必须是 0700 / 0600。
//
// 这条守的是 audit 路径：`atomicfile.Append` 建父目录用 0755，如果「校验失败 → 写
// audit」先于 ensureStore 发生，surfaces 根目录就会被种成 0755（之后所有插件文件都
// 住在这个洞里，而权限断言只在成功路径上才发现）。
func TestSurfaceRejectedFirstRunLeavesSafePerms(t *testing.T) {
	st, root := testSurfaceStore(t)
	bad := surfaceFixture(t, "Bad-Id", "1.0.0", "export default () => null\n")
	rec := st.Install(bad, true)
	if rec.Status != surfaceRejected {
		t.Fatalf("status = %q，want rejected（reason=%q）", rec.Status, rec.Reason)
	}
	wantMode(t, root, 0o700)
	wantMode(t, filepath.Join(root, "audit.jsonl"), 0o600)
}

// entry 可以是子目录里的文件：hash 认 manifest.entry，不认「panel.js」这个名字。
//
// 这条钉的是「hash = sha256(manifest.entry)」这个口径（01 §5.4 写的是 sha256(panel.js)）：
// 认死文件名会让 `"entry":"dist/panel.js"` 这种合法 manifest 装不上，或者算出一个
// 不属于任何文件的 hash。
func TestSurfaceInstallAcceptsNestedEntry(t *testing.T) {
	st, root := testSurfaceStore(t)
	panel := "export default () => 'nested'\n"
	src := surfaceFixture(t, "nested-entry", "0.2.0", "", withManifest(func(m map[string]any) {
		m["entry"] = "dist/panel.js"
	}), func(dir string, _ map[string]any) {
		if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o700); err != nil {
			t.Fatalf("建 dist: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "dist", "panel.js"), []byte(panel), 0o600); err != nil {
			t.Fatalf("写 dist/panel.js: %v", err)
		}
	})

	rec := st.Install(src, true)
	if rec.Status != surfaceApplied {
		t.Fatalf("status = %q，want applied（reason=%q）", rec.Status, rec.Reason)
	}
	if want := sha256Hex(panel); rec.Hash != want {
		t.Fatalf("hash = %q，want sha256(dist/panel.js) = %q", rec.Hash, want)
	}
	got := string(readBytes(t, filepath.Join(root, "nested-entry", "dist", "panel.js")))
	if got != panel {
		t.Fatalf("拷贝的入口 = %q，want %q", got, panel)
	}
}
