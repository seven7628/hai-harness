// surface_dev_test.go —— CreateMode 热加载（dev 安装 + 内容修订 + watcher）。
//
// 钉住四条（用户 2026-11 报障「CreateMode 插件不支持热加载」）：
//  1. 内容修订覆盖 manifest/图标：只改 surface.json 或 icon.svg 也要判「变了」，
//     否则安装器回 already_installed、渲染层 hash 不变 → 改了没反应；
//  2. dev 安装落的是**软链**（index dev:true），改源目录 = 改已装目录；
//  3. 卸载 dev 插件只移走软链，**源目录一个字节都不动**；
//  4. watcher：保存 panel.js → index hash 变化 + 推一次 surfaces_changed。
package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSurfaceContentRevisionCoversManifestAndIcon(t *testing.T) {
	st, _ := testSurfaceStore(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n", withFile(t, "icon.svg", "<svg/>"))
	first := st.Install(src, true)
	if first.Status != surfaceApplied {
		t.Fatalf("首次安装：%+v", first)
	}

	// 只改 icon.svg：入口文件一个字节没变 —— 旧口径（sha256(panel.js)）会判 already_installed。
	if err := os.WriteFile(filepath.Join(src, "icon.svg"), []byte("<svg id='v2'/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := st.Install(src, true)
	if rec.Status != surfaceApplied {
		t.Fatalf("只改图标后 status = %q，want applied（旧口径会在这里判 already_installed）", rec.Status)
	}
	if rec.Hash == first.Hash {
		t.Fatalf("只改图标 hash 没变：%q（渲染层会跳过重载）", rec.Hash)
	}

	// 只改 surface.json（entries/commands）：同样必须变。
	manifest := []byte(`{"id":"cost-board","name":{"zh":"成本看板"},"version":"0.1.0","kind":"plugin:cost-board","entry":"panel.js","icon":"icon.svg","entries":[{"id":"overview","title":"Overview"}]}`)
	if err := os.WriteFile(filepath.Join(src, "surface.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	rec2 := st.Install(src, true)
	if rec2.Status != surfaceApplied || rec2.Hash == rec.Hash {
		t.Fatalf("只改 manifest 后 = %+v（want applied + 新 hash %q）", rec2, rec.Hash)
	}
}

func TestSurfaceDevInstallLinksSourceAndKeepsItOnRemove(t *testing.T) {
	st, root := testSurfaceStore(t)
	panel := "export default () => 'v1'\n"
	src := surfaceFixture(t, "cost-board", "0.1.0", panel)
	realSrc, err := filepath.EvalSymlinks(src) // t.TempDir 在 macOS 上可能带 /private 前缀
	if err != nil {
		t.Fatal(err)
	}

	rec := st.InstallDev(src, true, true)
	if rec.Status != surfaceApplied || !rec.Dev {
		t.Fatalf("dev 安装 = %+v，want applied + Dev", rec)
	}
	link := filepath.Join(root, "cost-board")
	st1, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("dev 目录不存在：%v", err)
	}
	if st1.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dev 安装不是软链：mode = %v", st1.Mode())
	}
	if got, _ := filepath.EvalSymlinks(link); got != realSrc {
		t.Fatalf("软链指向 %q，want %q", got, realSrc)
	}

	// 同内容再装 = already_installed（幂等），且不会把软链变成拷贝。
	again := st.InstallDev(src, true, true)
	if again.Status != surfaceAlreadyInstalled || !again.Dev {
		t.Fatalf("重复 dev 安装 = %+v，want already_installed + Dev", again)
	}

	// 改源目录 = 改已装目录（热加载的物理前提）。
	live := []byte("export default () => 'v2'\n")
	if err := os.WriteFile(filepath.Join(src, "panel.js"), live, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readBytes(t, filepath.Join(link, "panel.js")); string(got) != string(live) {
		t.Fatalf("已装目录读到的仍是旧内容：%q", got)
	}

	// 卸载只移走软链：源目录仍在，内容未动。
	if r := st.Remove("cost-board"); r.Status != surfaceApplied {
		t.Fatalf("卸载 = %+v，want applied", r)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("卸载后软链仍在：%v", err)
	}
	if got := readBytes(t, filepath.Join(src, "panel.js")); string(got) != string(live) {
		t.Fatalf("卸载动了源目录：%q", got)
	}
}

// 只改图标也要能换下 hash（refreshDevHash 是 watcher 的落库一步）。
func TestSurfaceRefreshDevHashOnlyTouchesDevEntries(t *testing.T) {
	st, root := testSurfaceStore(t)
	src := surfaceFixture(t, "cost-board", "0.1.0", "export default () => null\n")
	if rec := st.Install(src, true); rec.Status != surfaceApplied {
		t.Fatalf("拷贝安装：%+v", rec)
	}
	if st.refreshDevHash("cost-board", "deadbeef") {
		t.Fatal("拷贝安装（非 dev）被 refreshDevHash 改了 hash")
	}
	if _, got := readIndex(t, root); got.Items[0]["hash"] == "deadbeef" {
		t.Fatal("index 被改了")
	}

	src2 := surfaceFixture(t, "dev-board", "0.1.0", "export default () => null\n")
	if rec := st.InstallDev(src2, true, true); rec.Status != surfaceApplied {
		t.Fatalf("dev 安装：%+v", rec)
	}
	if !st.refreshDevHash("dev-board", "deadbeef") {
		t.Fatal("dev 条目的 hash 没更新")
	}
	_, idx := readIndex(t, root)
	for _, it := range idx.Items {
		if it["id"] == "dev-board" {
			if it["hash"] != "deadbeef" || it["dev"] != true {
				t.Fatalf("dev 条目 = %v", it)
			}
		}
	}
}

// watcher 端到端：保存 panel.js → 250ms 合并窗口后 hash 更新 + 一次 surfaces_changed。
func TestSurfaceWatcherReloadsDevPluginOnSave(t *testing.T) {
	st, root := testSurfaceStore(t)
	src := surfaceFixture(t, "live-board", "0.1.0", "export default () => 'v1'\n")
	if rec := st.InstallDev(src, true, true); rec.Status != surfaceApplied {
		t.Fatalf("dev 安装：%+v", rec)
	}

	var pushes int32
	w := newSurfaceWatcher(st, func() { atomic.AddInt32(&pushes, 1) })
	w.Start()
	defer w.Stop()

	_, first := readIndex(t, root)
	before := first.Items[0]["hash"]
	live := []byte("export default () => 'v2'\n")
	if err := os.WriteFile(filepath.Join(src, "panel.js"), live, 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, idx := readIndex(t, root); idx.Items[0]["hash"] != before {
			if atomic.LoadInt32(&pushes) == 0 {
				time.Sleep(200 * time.Millisecond) // push 与写 index 同一轮，给一拍
			}
			if atomic.LoadInt32(&pushes) == 0 {
				t.Fatal("hash 更新了但没推 surfaces_changed（渲染层不会重载）")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("5s 内 watcher 没把新 hash 写进 index（热加载未生效）")
}
