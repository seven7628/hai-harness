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
	"fmt"
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

// ---- 入口声明 entries（R16 / D-2 · D-3）----
//
// 两条不变式（与 §4.7「生产入口」一起读）：
//  1. **校验先于写盘**：五类非法 entries 一律 `rejected`，index **逐字节**不变、不留半个目录；
//  2. **合法 entries 原样可读**：装完之后从 `List` + `Manifest` 走一遍，渲染层拿到的必须
//     就是 manifest 里写的那三项（id / title / icon / order 一个不丢、顺序不动）。
//
// 为什么单开一个测试而不是塞进 TestSurfaceInstallRejectsBadManifests：那一组测的是
// 「manifest 的既有四字段」（id/version/entry/icon），entries 是**新的一张表**，五条规则
// 各自可独立变红 —— 混进去会让「哪一条坏了」只能靠 reason 猜。
func TestSurfaceEntriesRejectedBeforeWrite(t *testing.T) {
	entry := func(id, title, icon string, order int) map[string]any {
		m := map[string]any{"id": id, "title": title, "order": order}
		if icon != "" {
			m["icon"] = icon
		}
		return m
	}
	// 21 项：前 20 项都合法（每一项单独都能过），第 21 项让「超上限」成为唯一失败原因。
	tooMany := make([]any, 0, 21)
	for i := 0; i < 21; i++ {
		tooMany = append(tooMany, entry(fmt.Sprintf("page-%02d", i), fmt.Sprintf("页 %d", i), "", i*10))
	}

	cases := []struct {
		name   string
		items  []any
		reason string // reason 里必须出现的子串
	}{
		{
			name:   "重复 id",
			items:  []any{entry("overview", "总览", "", 10), entry("overview", "课表", "", 20)},
			reason: "id 重复",
		},
		{
			name:   "21 项（超 20 上限）",
			items:  tooMany,
			reason: "超过 20 项",
		},
		{
			name:   "id 非法（大写 + 空格）",
			items:  []any{entry("Overview", "总览", "", 10)},
			reason: "id 不合法",
		},
		{
			// title 按 rune 判：65 个汉字 = 195 字节。若按字节判，这条会在 22 个字时就红，
			// 而 64 个汉字的标题本该合法（见下一条断言）。
			name:   "title 超 64 字符",
			items:  []any{entry("overview", strings.Repeat("看", 65), "", 10)},
			reason: "title 过长",
		},
		{
			name:   "icon 逃逸 ../x.svg",
			items:  []any{entry("overview", "总览", "../x.svg", 10)},
			reason: "icon",
		},
		{
			// R16b：对象写法里一个非空都没有 = 没有可渲染的标题（与空字符串同类失败，
			// 但 reason 要说清是对象写法，模型才知道该补哪一边）。
			name:   "title 对象两种语言都空",
			items:  []any{map[string]any{"id": "overview", "title": map[string]any{"zh": "", "en": ""}, "order": 10}},
			reason: "title 不能为空",
		},
		{
			// 逐语言各判一次：en 合法、zh 超限 → 必须红（只判「最长那个」也行，但不能不判）。
			name:   "title.zh 超 64 字符（对象写法）",
			items:  []any{map[string]any{"id": "overview", "title": map[string]any{"zh": strings.Repeat("看", 65), "en": "Overview"}, "order": 10}},
			reason: "title.zh 过长",
		},
		{
			name:   "title 对象的值不是字符串",
			items:  []any{map[string]any{"id": "overview", "title": map[string]any{"zh": 42}, "order": 10}},
			reason: "title.zh 必须是字符串",
		},
		{
			name:   "title 是数字",
			items:  []any{map[string]any{"id": "overview", "title": 7, "order": 10}},
			reason: "title 必须是字符串或",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, root := testSurfaceStore(t)
			// 基线：先装一个合法插件，让「index 一字节没变」有真内容可比。
			okSrc := surfaceFixture(t, "baseline", "1.0.0", "export default () => null\n")
			if r := st.Install(okSrc, true); r.Status != surfaceApplied {
				t.Fatalf("基线安装失败: %+v", r)
			}
			before := readBytes(t, filepath.Join(root, "index.json"))
			beforeEntries, _ := os.ReadDir(root)

			bad := surfaceFixture(t, "entry-bad", "1.0.0", "export default () => null\n",
				withManifest(func(m map[string]any) { m["entries"] = c.items }))
			rec := st.Install(bad, true)
			if rec.Status != surfaceRejected {
				t.Fatalf("status = %q，want %q（reason=%q）", rec.Status, surfaceRejected, rec.Reason)
			}
			if !strings.Contains(rec.Reason, c.reason) {
				t.Fatalf("reason = %q，必须包含 %q（可读、指向具体问题）", rec.Reason, c.reason)
			}
			// 五连里每一条都必须先于**任何**写盘动作：index 逐字节不变 + 没有 <id> 目录。
			if after := readBytes(t, filepath.Join(root, "index.json")); string(before) != string(after) {
				t.Fatalf("校验失败却改了 index：\nbefore=%s\nafter=%s", before, after)
			}
			notExist(t, filepath.Join(root, "entry-bad"))
			afterEntries, _ := os.ReadDir(root)
			for _, e := range afterEntries {
				found := false
				for _, b := range beforeEntries {
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

// TestSurfaceEntriesRoundTrip 合法 entries 装完 → 从生产读侧（List + Manifest）原样拿回。
//
// 顺带钉住两条**不该失败**的边界（D-3）：
//
//	· entries 缺省 / 空数组 = 合法（宿主给默认入口，不是「没入口」）；
//	· 单个 entry 的 icon 指向一个**不存在**的文件 = 合法（渲染层用 kind 图标兜底）。
//	  它与「icon 逃逸」的差别是安全 vs 体验：前者读不到只是少个图标，后者是任意文件读取。
func TestSurfaceEntriesRoundTrip(t *testing.T) {
	st, _ := testSurfaceStore(t)
	want := []surfaceEntryDecl{
		{ID: "overview", Title: surfaceEntryTitle{Text: "总览"}, Order: 10},
		{ID: "schedule", Title: surfaceEntryTitle{Text: "课表"}, Icon: "calendar.svg", Order: 20}, // 不存在也合法
		{ID: "resources", Title: surfaceEntryTitle{Text: "课程资源"}, Order: 30},
		// R16b：对象写法与单字符串写法可以**混在同一个 manifest 里**（老插件不用改一个字）。
		{ID: "progress", Title: surfaceEntryTitle{Zh: "进度", En: "Progress"}, Order: 40},
		{ID: "en-only", Title: surfaceEntryTitle{En: "English only"}, Order: 50},
	}
	raw := make([]any, 0, len(want))
	for _, e := range want {
		m := map[string]any{"id": e.ID, "order": e.Order}
		switch {
		case e.Title.Text != "":
			m["title"] = e.Title.Text // 老的字符串写法
		default: // 对象写法：只写声明了的语言
			obj := map[string]any{}
			if e.Title.Zh != "" {
				obj["zh"] = e.Title.Zh
			}
			if e.Title.En != "" {
				obj["en"] = e.Title.En
			}
			m["title"] = obj
		}
		if e.Icon != "" {
			m["icon"] = e.Icon
		}
		raw = append(raw, m)
	}
	src := surfaceFixture(t, "course-manager", "1.0.0", "export default () => null\n",
		withManifest(func(m map[string]any) { m["entries"] = raw }))
	if r := st.Install(src, true); r.Status != surfaceApplied {
		t.Fatalf("合法 entries 装不上: %+v", r)
	}

	list := st.List(false)
	if list.Status != surfaceApplied || len(list.Items) != 1 || list.Items[0].ID != "course-manager" {
		t.Fatalf("List = %+v，want 只有 course-manager", list)
	}
	mf, err := st.Manifest("course-manager")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(mf.Entries) != len(want) {
		t.Fatalf("entries 条数 = %d，want %d（%+v）", len(mf.Entries), len(want), mf.Entries)
	}
	for i, w := range want {
		if got := mf.Entries[i]; got != w {
			t.Fatalf("entries[%d] = %+v，want %+v（顺序与字段都必须原样）", i, got, w)
		}
	}

	// 零入口：缺省（不给 entries 字段）与空数组都合法。
	zero := surfaceFixture(t, "plain-panel", "1.0.0", "export default () => null\n")
	if r := st.Install(zero, true); r.Status != surfaceApplied {
		t.Fatalf("缺省 entries 装不上: %+v", r)
	}
	empty := surfaceFixture(t, "empty-panel", "1.0.0", "export default () => null\n",
		withManifest(func(m map[string]any) { m["entries"] = []any{} }))
	if r := st.Install(empty, true); r.Status != surfaceApplied {
		t.Fatalf("空 entries 数组装不上: %+v", r)
	}
	for _, id := range []string{"plain-panel", "empty-panel"} {
		mf, err := st.Manifest(id)
		if err != nil {
			t.Fatalf("Manifest(%s): %v", id, err)
		}
		if len(mf.Entries) != 0 {
			t.Fatalf("%s 的 entries = %+v，want 空（宿主给默认入口）", id, mf.Entries)
		}
	}
}

// TestSurfaceEntryTitleForms R16b：`entries[].title` 的两种写法。
//
// 三条不许破的约束：
//  1. 单字符串**继续可用**（老 manifest 一个字都不用改）—— 这是「不要为新能力改旧契约」；
//  2. 对象写法缺哪一边都合法（只写 zh 或只写 en），但「两个都空」不合法；
//  3. 写回去形状不变（单字符串仍是字符串）：`Manifest()` 读出来的必须是声明本身，
//     而不是我们内部一律归一化成对象的样子。
func TestSurfaceEntryTitleForms(t *testing.T) {
	big64 := strings.Repeat("看", 64) // 64 个汉字 = 192 字节：按字节判会把这条合法标题判死
	cases := []struct {
		name    string
		raw     string
		want    surfaceEntryTitle
		primary string
	}{
		{"单字符串（老写法）", `"总览"`, surfaceEntryTitle{Text: "总览"}, "总览"},
		{"对象 zh+en", `{"zh":"总览","en":"Overview"}`, surfaceEntryTitle{Zh: "总览", En: "Overview"}, "总览"},
		{"对象只有 zh", `{"zh":"总览"}`, surfaceEntryTitle{Zh: "总览"}, "总览"},
		{"对象只有 en", `{"en":"Overview"}`, surfaceEntryTitle{En: "Overview"}, "Overview"},
		{"64 个汉字（上限本身合法）", `"` + big64 + `"`, surfaceEntryTitle{Text: big64}, big64},
		{"对象 64 个汉字（同上）", `{"zh":"` + big64 + `"}`, surfaceEntryTitle{Zh: big64}, big64},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got surfaceEntryTitle
			if err := json.Unmarshal([]byte(c.raw), &got); err != nil {
				t.Fatalf("合法写法被判非法：%v（输入 %s）", err, c.raw)
			}
			if got != c.want {
				t.Fatalf("解析 = %+v，want %+v", got, c.want)
			}
			if got.Primary() != c.primary {
				t.Fatalf("Primary() = %q，want %q（回执/日志里那句人读标题）", got.Primary(), c.primary)
			}
			if err := got.validate(maxSurfaceEntryTitleChars); err != nil {
				t.Fatalf("validate = %v，want nil", err)
			}
			out, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			wantStringForm := strings.HasPrefix(strings.TrimSpace(c.raw), `"`)
			gotStringForm := strings.HasPrefix(strings.TrimSpace(string(out)), `"`)
			if wantStringForm != gotStringForm {
				t.Fatalf("写回形状变了：输入 %s → 输出 %s（单字符串不许被归一化成对象）", c.raw, out)
			}
		})
	}

	// 非法形状：每条都要有指向具体问题的 reason（不是「形状不对」四个字）。
	bad := []struct {
		name   string
		raw    string
		reason string
	}{
		{"空字符串", `""`, "title 不能为空"},
		{"null", `null`, "title 不能为空"},
		{"空对象", `{}`, "title 不能为空"},
		{"只写了别的语言", `{"ja":"概要"}`, "title 不能为空"},
		{"65 个汉字", `"` + strings.Repeat("看", 65) + `"`, "title 过长"},
		{"对象里 en 超限", `{"zh":"总览","en":"` + strings.Repeat("O", 65) + `"}`, "title.en 过长"},
		{"语言值不是字符串", `{"zh":1}`, "title.zh 必须是字符串"},
	}
	for _, c := range bad {
		t.Run("非法/"+c.name, func(t *testing.T) {
			var got surfaceEntryTitle
			err := json.Unmarshal([]byte(c.raw), &got)
			if err == nil {
				err = got.validate(maxSurfaceEntryTitleChars)
			}
			if err == nil {
				t.Fatalf("%s 被判合法（%+v）", c.raw, got)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("reason = %q，必须包含 %q（模型据此改输入）", err.Error(), c.reason)
			}
		})
	}
}

// TestSurfaceInstallReceiptEntries R16b：安装回执要回答「几个入口 / 入口在哪」。
//
// 三条边界一起钉住：
//   - applied / already_installed 都带 entries（脚本重跑一次，「有几个入口」的答案不该消失）；
//   - 零入口回执是 `entries: 0`，**不是**没有这个字段（omitempty 不能把它吃掉）；
//   - uninstall / rejected 不带（那不是「装上了」的信息，带上会被读成「这个插件有 0 个入口」）。
func TestSurfaceInstallReceiptEntries(t *testing.T) {
	st, _ := testSurfaceStore(t)
	src := surfaceFixture(t, "course-manager", "1.0.0", "export default () => null\n",
		withManifest(func(m map[string]any) {
			m["entries"] = []any{
				map[string]any{"id": "overview", "title": "总览", "order": 10},
				map[string]any{"id": "schedule", "title": map[string]any{"zh": "课表", "en": "Schedule"}, "order": 20},
				map[string]any{"id": "progress", "title": map[string]any{"en": "Progress"}, "order": 30},
			}
		}))

	rec := st.Install(src, true)
	if rec.Status != surfaceApplied {
		t.Fatalf("status = %q，want applied（reason=%q）", rec.Status, rec.Reason)
	}
	if rec.Entries == nil || *rec.Entries != 3 {
		t.Fatalf("回执 entries = %v，want 3", rec.Entries)
	}
	if rec.EntryHint != surfaceEntryHint {
		t.Fatalf("回执 entryHint = %q，want %q", rec.EntryHint, surfaceEntryHint)
	}
	// 逐字钉住提示内容：它是 D-2 第 1 处渲染点（＋ → 面板插件）的对齐点，改文案得同时改这里。
	if surfaceEntryHint != "right panel ＋ → Panel plugins" {
		t.Fatalf("surfaceEntryHint = %q，want \"right panel ＋ → Panel plugins\"", surfaceEntryHint)
	}

	// 幂等重跑：already_installed 也带入口条数。
	again := st.Install(src, true)
	if again.Status != surfaceAlreadyInstalled {
		t.Fatalf("重跑 status = %q，want already_installed", again.Status)
	}
	if again.Entries == nil || *again.Entries != 3 || again.EntryHint != surfaceEntryHint {
		t.Fatalf("重跑回执 = %+v，want entries=3 + entryHint", again)
	}

	// 零入口：字段存在且为 0，序列化里也得看得见。
	zero := st.Install(surfaceFixture(t, "plain-panel", "1.0.0", "export default () => null\n"), true)
	if zero.Status != surfaceApplied || zero.Entries == nil || *zero.Entries != 0 {
		t.Fatalf("零入口回执 = %+v，want entries=0（字段必须在）", zero)
	}
	raw, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("Marshal 回执: %v", err)
	}
	if !strings.Contains(string(raw), `"entries":0`) {
		t.Fatalf("零入口回执 JSON = %s，必须含 \"entries\":0（omitempty 不能把它吃掉）", raw)
	}

	// 卸载 / 拒绝不带 entries：同一个 JSON 字段在别的状态里出现=两种含义。
	rm := st.Remove("plain-panel")
	// 卸载成功的状态词是 `applied`（既有契约，R16b 不动它）：能删掉就是「这次操作生效了」。
	if rm.Status != surfaceApplied {
		t.Fatalf("remove status = %q，want applied", rm.Status)
	}
	rawRm, _ := json.Marshal(rm)
	if strings.Contains(string(rawRm), "entries") || strings.Contains(string(rawRm), "entryHint") {
		t.Fatalf("卸载回执 = %s，不该带 entries/entryHint", rawRm)
	}
	bad := st.Install(surfaceFixture(t, "bad-entries", "1.0.0", "export default () => null\n",
		withManifest(func(m map[string]any) {
			m["entries"] = []any{map[string]any{"id": "overview", "title": "", "order": 10}}
		})), true)
	if bad.Status != surfaceRejected {
		t.Fatalf("非法 entries status = %q，want rejected", bad.Status)
	}
	if bad.Entries != nil || bad.EntryHint != "" {
		t.Fatalf("拒绝回执 = %+v，不该带 entries/entryHint", bad)
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

// ---- 安装期拒绝 D11 下线的四条旧命令（P1e 加固） ----

// TestSurfaceInstallRejectsRetiredCommands —— 四条旧命令在**安装期**就判死。
//
// 为什么不在运行期拒绝就够了：`surfaceHostCommands` 清空之后，装了这种插件的用户
// 拿到的是一个「面板能打开、每次调用都 denied」的东西，index 里还多一条永远修不好的
// 记录。安装期拒绝把问题提前到作者手里 —— 回执里直接写「哪条命令、该改用 agent.*」。
//
// 每条用例都断言三件事：status=rejected、理由可读（命令名 + D11 + agent.*）、
// **磁盘一个字节没动**（index 不存在、目录没拷过去、没有暂存残留）。
// 最后一条是正对照：只用 agent.* 的 manifest 必须照旧装得上（这条闸不许扩权）。
func TestSurfaceInstallRejectsRetiredCommands(t *testing.T) {
	retired := []string{"workspace_list", "session_list", "git_snapshot", "ui_inspect"}
	for _, name := range retired {
		name := name
		t.Run(name, func(t *testing.T) {
			st, root := testSurfaceStore(t)
			src := surfaceFixture(t, "legacy-panel", "1.0.0", "export default () => null\n",
				withManifest(func(m map[string]any) {
					// 混在合法命令里：只要出现一条旧命令就整包拒绝。
					m["commands"] = []string{surfaceAgentSessionName, name}
				}))

			rec := st.Install(src, true)
			if rec.Status != surfaceRejected {
				t.Fatalf("status = %q，want %q（回执 %+v）", rec.Status, surfaceRejected, rec)
			}
			t.Logf("拒绝文案 = %s", rec.Reason)
			for _, want := range []string{name, "D11 已下线", "agent.*"} {
				if !strings.Contains(rec.Reason, want) {
					t.Fatalf("拒绝理由缺 %q（作者读不出该怎么办）：%q", want, rec.Reason)
				}
			}
			// 拒绝 = 不写 index、不拷目录：根目录只可能多出 audit.jsonl。
			notExist(t, filepath.Join(root, "index.json"))
			notExist(t, filepath.Join(root, "legacy-panel"))
			noStagingLeftovers(t, root)
			// 审计里要有一行 install 失败（装不上的原因必须可对账）。
			raw := string(readBytes(t, filepath.Join(root, "audit.jsonl")))
			if !strings.Contains(raw, `"action":"install"`) || !strings.Contains(raw, `"ok":false`) {
				t.Fatalf("audit.jsonl 没有 install 失败记录：%s", raw)
			}
			if !strings.Contains(raw, name) {
				t.Fatalf("审计没写清是哪条命令被拒：%s", raw)
			}
		})
	}

	t.Run("合法命令不受影响", func(t *testing.T) {
		st, root := testSurfaceStore(t)
		src := surfaceFixture(t, "agent-panel", "1.0.0", "export default () => null\n",
			withManifest(func(m map[string]any) {
				// agent.* / composer.quote / ui.* 都在这条闸之外（只关 D11 那四条）。
				m["commands"] = []string{
					surfaceAgentSessionName, surfaceAgentRunName, surfaceAgentCancelName,
					"composer.quote", "ui.inspect",
				}
			}))
		rec := st.Install(src, true)
		if rec.Status != surfaceApplied {
			t.Fatalf("只用 agent.* 的 manifest 被拒了（这条闸扩权了）：%+v", rec)
		}
		_, idx := readIndex(t, root)
		if len(idx.Items) != 1 || idx.Items[0]["id"] != "agent-panel" {
			t.Fatalf("index = %v，want 只有 agent-panel", idx.Items)
		}
		if _, err := os.Stat(filepath.Join(root, "agent-panel", "panel.js")); err != nil {
			t.Fatalf("正对照没真的拷贝：%v", err)
		}
	})
}

// ---- 工具声明 tools[]（P2a / 09-agent-bridge §5.3 · §6.4 · §7.3）----
//
// 与 entries 同一套不变式（见 TestSurfaceEntriesRejectedBeforeWrite 的文件头）：
// 校验先于写盘 —— 任何一条不过都 `rejected`，index **逐字节**不变、不留半个目录。
//
// 为什么单开：tools[] 是**模型可见**的声明（schema 直接进 provider 请求），
// 比 entries 多两类失败（schema 形状、通道声明），混进既有表格会让「哪一条坏了」
// 只能靠 reason 猜。

// toolChannels 一份声明了两条工具通道的 manifest 片段（缺通道的用例自己删）。
func toolChannels() (cmds, data []string) {
	return []string{surfaceAgentToolResult}, []string{surfaceToolCallEvent}
}

// toolDecl 造一条工具声明（input 由调用方给，可用 map 或 []any 等任意 JSON 值）。
func toolDecl(name, desc string, input any) map[string]any {
	d := map[string]any{"name": name, "description": desc}
	if input != nil {
		d["input"] = input
	}
	return d
}

// withChannelTools 把 tools / commands / data 一起写进 manifest（三者必须同进同退）。
func withChannelTools(cmds, data []string, tools ...any) fixtureOpt {
	return withManifest(func(m map[string]any) {
		m["commands"] = cmds
		m["data"] = data
		if tools != nil {
			m["tools"] = tools
		}
	})
}

// withTools 声明两条工具通道都已就位的 tools（绝大多数用例的起点）。
func withTools(tools ...any) fixtureOpt {
	cmds, data := toolChannels()
	return withChannelTools(cmds, data, tools...)
}

// wantObjSchema 一份最小合法 input。
func wantObjSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
		"required":   []string{"q"},
	}
}

func TestSurfaceToolsRejectedBeforeWrite(t *testing.T) {
	// 17 项：前 16 项都合法（任一项单独都能过），第 17 项让「超上限」成为唯一失败原因。
	tooMany := make([]any, 0, 17)
	for i := 0; i < 17; i++ {
		tooMany = append(tooMany, toolDecl(fmt.Sprintf("tool_%02d", i), fmt.Sprintf("工具 %d", i), wantObjSchema()))
	}

	cases := []struct {
		name   string
		opts   []fixtureOpt
		reason string // reason 里必须出现的子串
	}{
		{
			name: "data 未声明 tool.call",
			opts: []fixtureOpt{withChannelTools([]string{surfaceAgentToolResult}, []string{"agent.delta"},
				toolDecl("echo", "回显", wantObjSchema()))},
			reason: surfaceToolCallEvent,
		},
		{
			name: "commands 未声明 tool.result",
			opts: []fixtureOpt{withChannelTools([]string{surfaceAgentRunName}, []string{surfaceToolCallEvent},
				toolDecl("echo", "回显", wantObjSchema()))},
			reason: surfaceAgentToolResult,
		},
		{
			name: "重复 name",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "第一次", wantObjSchema()),
				toolDecl("echo", "第二次", wantObjSchema()))},
			reason: "name 重复",
		},
		{
			// `__` 是模型侧名 plugin__<id>__<name> 的分隔符：工具名带头会把归属判成一团。
			name: "name 含 __",
			opts: []fixtureOpt{withTools(
				toolDecl("echo__v2", "带分隔符的名字", wantObjSchema()))},
			reason: "__",
		},
		{
			name: "name 非法（大写 + 连字符）",
			opts: []fixtureOpt{withTools(
				toolDecl("Echo-Tool", "大写与连字符", wantObjSchema()))},
			reason: "name 不合法",
		},
		{
			name: "缺 description",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "   ", wantObjSchema()))},
			reason: "description",
		},
		{
			name: "description 超 300 字符",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", strings.Repeat("回", 301), wantObjSchema()))},
			reason: "过长",
		},
		{
			name: "input 缺失",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "回显", nil))},
			reason: "input 缺失",
		},
		{
			name: "input 不是 object（数组）",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "回显", []any{}))},
			reason: "JSON object",
		},
		{
			name: "input.type 不是 object（string）",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "回显", map[string]any{"type": "string"}))},
			reason: `"type" 必须是 "object"`,
		},
		{
			name: "input 含 $ref",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "回显", map[string]any{
					"type":       "object",
					"properties": map[string]any{"q": map[string]any{"$ref": "#/definitions/q"}},
				}))},
			reason: "$ref",
		},
		{
			name: "input.required 不是字符串数组",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "回显", map[string]any{
					"type": "object", "properties": map[string]any{}, "required": []any{1, 2},
				}))},
			reason: "required",
		},
		{
			name: "input.properties 不是对象",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "回显", map[string]any{
					"type": "object", "properties": []any{"q"},
				}))},
			reason: "properties",
		},
		{
			// 16 KiB 按紧凑序列化后算：17 KiB 的描述性字段让「超体积」成为唯一失败原因。
			// 其余字段全部合法 —— 否则这条会红在别处，测不到体积闸。
			name: "input 超 16 KiB",
			opts: []fixtureOpt{withTools(
				toolDecl("echo", "回显", map[string]any{
					"type":       "object",
					"properties": map[string]any{"q": map[string]any{"type": "string", "description": strings.Repeat("x", 17<<10)}},
				}))},
			reason: "16 KiB",
		},
		{
			// id 与 name **各自**都合法（id ≤41、name ≤41），拼出来的
			// plugin__<id>__<name>（3+41+2+41 = 87）却超过 OpenAI 的 64 —— 这类超限
			// 只能在这里拦：provider 侧超长是整轮 400，不是截断。
			name: "模型侧全名超 64",
			opts: []fixtureOpt{
				// id 41 + name 41 都各自合法，拼起来 3+41+2+41 = 87 > 64。
				withManifest(func(m map[string]any) { m["id"] = strings.Repeat("i", 41) }),
				withTools(toolDecl(strings.Repeat("n", 41), "名字很长但各自合法", wantObjSchema())),
			},
			reason: "超过 64",
		},
		{
			name:   "17 项（超 16 上限）",
			opts:   []fixtureOpt{withTools(tooMany...)},
			reason: "超过 16 项",
		},
		{
			name: "timeout_ms 太小",
			opts: []fixtureOpt{withTools(
				func() map[string]any {
					d := toolDecl("echo", "回显", wantObjSchema())
					d["timeout_ms"] = 50
					return d
				}())},
			reason: "timeout_ms",
		},
		{
			name: "timeout_ms 太大",
			opts: []fixtureOpt{withTools(
				func() map[string]any {
					d := toolDecl("echo", "回显", wantObjSchema())
					d["timeout_ms"] = 60001
					return d
				}())},
			reason: "timeout_ms",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, root := testSurfaceStore(t)
			// 先装一个合法的：让「index 一个字节没变」有真内容可比。
			okSrc := surfaceFixture(t, "baseline", "1.0.0", "export default () => null\n")
			if r := st.Install(okSrc, true); r.Status != surfaceApplied {
				t.Fatalf("基线安装失败: %+v", r)
			}
			before := readBytes(t, filepath.Join(root, "index.json"))
			entriesBefore, _ := os.ReadDir(root)

			bad := surfaceFixture(t, "tool-panel", "1.0.0", "export default () => null\n", c.opts...)
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
			// 没半个目录：根目录条目集合不变（只可能多出 audit.jsonl）。
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
			notExist(t, filepath.Join(root, "tool-panel"))
			noStagingLeftovers(t, root)
		})
	}
}

// TestSurfaceToolsInstallRoundTrip：合法 tools[] 装得上，且 Manifest() 读回来的是
// 作者写的那一份（模型侧 schema 与审批声明都来自这次读取，不是安装期缓存）。
func TestSurfaceToolsInstallRoundTrip(t *testing.T) {
	st, root := testSurfaceStore(t)
	src := surfaceFixture(t, "tool-panel", "1.0.0", "export default () => null\n",
		withTools(
			func() map[string]any {
				d := toolDecl("echo", "回显一段文本", wantObjSchema())
				d["read_only"] = true
				return d
			}(),
			func() map[string]any {
				d := toolDecl("write_note", "写一条便签", wantObjSchema())
				d["requires_panel"] = true
				d["timeout_ms"] = 30000
				return d
			}()))
	rec := st.Install(src, true)
	if rec.Status != surfaceApplied {
		t.Fatalf("合法 tools 装不上：%+v", rec)
	}
	mf, err := st.Manifest("tool-panel")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(mf.Tools) != 2 {
		t.Fatalf("tools 条数 = %d，want 2：%+v", len(mf.Tools), mf.Tools)
	}
	echo, note := mf.Tools[0], mf.Tools[1]
	if echo.Name != "echo" || echo.Description != "回显一段文本" || !echo.ReadOnly || echo.RequiresPanel {
		t.Fatalf("echo 读回来变了：%+v", echo)
	}
	if echo.TimeoutMS != 0 || echo.timeout() != time.Duration(surfaceToolDefaultTimeoutMS)*time.Millisecond {
		t.Fatalf("echo timeout = %d/%s，want 0 且生效 10s", echo.TimeoutMS, echo.timeout())
	}
	if note.Name != "write_note" || note.ReadOnly || !note.RequiresPanel || note.TimeoutMS != 30000 {
		t.Fatalf("write_note 读回来变了：%+v", note)
	}
	// schema 原样可读（渲染层/模型侧都读这一份：键名与 required 一个不丢）。
	var schema map[string]any
	if err := json.Unmarshal(note.Input, &schema); err != nil {
		t.Fatalf("input 不是合法 JSON：%v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("input.type = %v，want object", schema["type"])
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "q" {
		t.Fatalf("input.required = %v，want [q]", schema["required"])
	}
	_, idx := readIndex(t, root)
	if len(idx.Items) != 1 || idx.Items[0]["id"] != "tool-panel" {
		t.Fatalf("index = %v，want 只有 tool-panel", idx.Items)
	}
	if _, err := os.Stat(filepath.Join(root, "tool-panel", "surface.json")); err != nil {
		t.Fatalf("正对照没真的拷贝：%v", err)
	}
}

// TestSurfaceToolsAbsentUnaffected：老 manifest（一个 tools 字段都没有）不受影响 ——
// 这条闸只关「声明了工具」的插件，不能把既有插件判死。
func TestSurfaceToolsAbsentUnaffected(t *testing.T) {
	st, root := testSurfaceStore(t)
	src := surfaceFixture(t, "legacy-panel", "1.0.0", "export default () => null\n")
	if rec := st.Install(src, true); rec.Status != surfaceApplied {
		t.Fatalf("无 tools 的老 manifest 被拒了：%+v", rec)
	}
	mf, err := st.Manifest("legacy-panel")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(mf.Tools) != 0 {
		t.Fatalf("没有 tools 的 manifest 读出了 %+v", mf.Tools)
	}
	_, idx := readIndex(t, root)
	if len(idx.Items) != 1 {
		t.Fatalf("index = %v", idx.Items)
	}
}
