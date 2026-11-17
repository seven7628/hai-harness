// surfaces_external_test.go —— 「别人的插件目录能不能装进来」的验收工具（R16b / Creator Mode）。
//
// 为什么单开一个文件而不是又一条夹具：Creator Mode 的判据**不是**「我们把插件写对了」，
// 而是「照 desktop/vendor/create-mode-skills/create-mode 写出来的目录，被真安装器装上了、界面上点得开」。
// 这条链路横跨两侧，所以拆成两个可重复执行的入口（两个都只**读**那个目录，不改它）：
//
//	go test ./desktop/bridge -run TestSurfaceInstallExternal -count=1 -v -args -dir=<插件目录>
//	node e2e/verify_external_surface.mjs --dir=<插件目录>        （npm run e2e:external-surface）
//
// 本文件只管前半段（安装器）：装机走**生产入口**（临时 HOME → appDataDir → newSurfaceStore
// → Install，与 main.go 同一个构造路径），断言 applied / index 一条 / hash = sha256(入口文件)
// / entries 条数与声明一致 / 权限 0600·0700，并把回执与 index 原样打印出来（-v 时可见）。
//
// 反例也在这里：`TestSurfaceInstallExternalRejectsMissingPanel` 不需要任何参数，永远跑 ——
// 「工具链只会说 OK」是最危险的失败形态，所以「缺 panel.js 必须装不上」是常驻用例。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// surfaceExternalDir 是 `-dir` 的值。
//
// 为什么用 flag 而不是环境变量：00-conventions 的「一个插件目录一条命令」形态 —— 命令行
// 自带路径，报告里能原样复述；`-args` 之后的 flag 由测试二进制自己解析（testing 在
// M.Run 里调 flag.Parse，包级 var 在 init 期已注册，不会报 "flag provided but not defined"）。
var surfaceExternalDir = flag.String("dir", "",
	"外部插件目录（含 surface.json + 入口文件）；配合 -run TestSurfaceInstallExternal 使用")

// surfaceExternalHome 是可选的「装到指定 HOME」（缺省 = t.TempDir() 用完即删）。
//
// 它存在的唯一理由是下一步：`HAI_DATA_DIR=<HOME>` 启动真 Electron 时，桥进程的 HOME 就是
// 这个目录（见 electron/main.ts 的多实例注释）——「装到应用真会去读的那一份」才能验
// 「装完 → ＋ 菜单里有」。
var surfaceExternalHome = flag.String("home", "",
	"把插件装到这个 HOME（缺省：临时目录）；配合 HAI_DATA_DIR 启动应用时用")

// surfaceExternalExpect 允许同一条外部安装链验证**拒绝**路径（接受 external 探针用）。
// 缺省 applied：保持既有「装不上就是失败」的语义；rejected 时断言回执 status/reason、
// 目录未拷、index 未新增 —— 其余 applied 专属断言（entries/hash/权限）不适用。
var surfaceExternalExpect = flag.String("expect", "applied",
	"期望安装结果：applied（缺省）或 rejected（用于 D11 旧命令的安装期拒绝）")

// surfaceExternalDev 让同一条外部安装链装成 **dev**（软链 + 热加载）。
//
// 存在的理由与 -home 相同：热加载只有在「应用真会去读的那一份」上才有意义 ——
// `-dev=1 -home=<HOME>` 装上，再 `HAI_DATA_DIR=<HOME>` 起真 Electron，改源目录里的
// panel.js 就能断言面板自己换内容（e2e/_surface_dev_reload.mjs）。缺省 false =
// 与既有拷贝快照语义逐字相同（模板/第三方目录的安装仍然是「拷一份进来」）。
var surfaceExternalDev = flag.Bool("dev", false,
	"以 dev 模式安装（软链源目录 + 热加载）；缺省 false = 拷贝快照")

// TestSurfaceInstallExternal 把 `-dir` 指向的目录用真安装器装进临时 HOME。
//
// 不指定 -dir 时**跳过**（不失败）：这条命令属于 Creator Mode 的验收链，普通 `go test ./...`
// 不该因为「没给插件目录」变红；但一旦给了目录，任何一步不合规都必须非零退出。
func TestSurfaceInstallExternal(t *testing.T) {
	raw := strings.TrimSpace(*surfaceExternalDir)
	if raw == "" {
		t.Skip("未指定插件目录：go test ./desktop/bridge -run TestSurfaceInstallExternal -count=1 -v -args -dir=<插件目录>")
	}
	dir, err := resolveExternalDir(raw)
	if err != nil {
		t.Fatalf("-dir 解析失败：%v", err)
	}

	// HOME：安装器走 appDataDir() 的生产解析，测试不碰真实的 ~/.go-code。
	// 缺省临时目录（用完即删）；-home 给了就装到那里（应用随之启动的那种用法）。
	home := strings.TrimSpace(*surfaceExternalHome)
	var st *surfaceStore
	var root string
	if home == "" {
		st, root = testSurfaceStore(t)
	} else {
		if err := os.MkdirAll(home, surfaceDirMode); err != nil {
			t.Fatalf("建 -home 失败：%v", err)
		}
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		prev := appDataDirOverride
		appDataDirOverride = ""
		t.Cleanup(func() { appDataDirOverride = prev })
		st = newSurfaceStore("")
		root = st.Dir()
	}
	if !strings.HasPrefix(root, filepath.Clean(home)+string(os.PathSeparator)) && home != "" {
		t.Fatalf("store 根目录 = %q，必须在 -home 之下（否则应用读的是另一份）", root)
	}

	// dev 与拷贝快照是同一个 install 的两条分支（dev 形参）；不要先拷一份再改装 ——
	// 那会白写一遍磁盘，还会在 index 里留下一次「拷贝 → 软链」的中间态。
	var rec surfaceReceipt
	if *surfaceExternalDev {
		rec = st.InstallDev(dir, true, true)
	} else {
		rec = st.Install(dir, true)
	}
	recJSON, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("回执序列化失败：%v", err)
	}
	// 打给人看：**回执就是验收证据**（status / id / hash / entries / entryHint）。
	t.Logf("安装回执（含 -v 可见）：\n%s", recJSON)
	want := strings.TrimSpace(*surfaceExternalExpect)
	if want == "" {
		want = "applied"
	}
	switch want {
	case "rejected":
		if rec.Status != surfaceRejected {
			t.Fatalf("want rejected，实际 status=%q reason=%q\n（目录：%s）", rec.Status, rec.Reason, dir)
		}
		if !strings.Contains(rec.Reason, "D11 已下线") {
			t.Fatalf("拒绝理由没有说清 D11：%q", rec.Reason)
		}
		if _, err := os.Stat(filepath.Join(root, rec.ID)); err == nil {
			t.Fatalf("拒绝了却把目录拷进来了：%s", filepath.Join(root, rec.ID))
		}
		if raw, err := os.ReadFile(filepath.Join(root, "index.json")); err == nil {
			var idx surfaceIndex
			if err := json.Unmarshal(raw, &idx); err == nil {
				for _, item := range idx.Items {
					if item.ID == rec.ID {
						t.Fatalf("拒绝了却写进了 index：%s", raw)
					}
				}
			}
		}
		return
	case "applied":
		// 继续走下面的 applied 断言。
	default:
		t.Fatalf("-expect=%q 不认识（只支持 applied / rejected）", want)
	}
	if rec.Status != surfaceApplied {
		t.Fatalf("装不上：status=%q reason=%q\n（目录：%s）", rec.Status, rec.Reason, dir)
	}
	if rec.Entries == nil {
		t.Fatalf("回执里没有 entries：%s（D-2 起安装回执必须回答「装上了几个入口」）", recJSON)
	}
	if rec.EntryHint != surfaceEntryHint {
		t.Fatalf("回执 entryHint = %q，want %q", rec.EntryHint, surfaceEntryHint)
	}

	// —— index 一条 ——
	idxPath := filepath.Join(root, "index.json")
	rawIndex := readBytes(t, idxPath)
	t.Logf("index.json：\n%s", rawIndex)
	var idx surfaceIndex
	if err := json.Unmarshal(rawIndex, &idx); err != nil {
		t.Fatalf("index.json 不是合法 JSON：%v", err)
	}
	if len(idx.Items) != 1 {
		t.Fatalf("index 条数 = %d，want 1（%s）", len(idx.Items), rawIndex)
	}
	if got := idx.Items[0]; got.ID != rec.ID || got.Hash != rec.Hash || !got.Enabled {
		t.Fatalf("index[0] = %+v，want id=%s hash=%s enabled=true", got, rec.ID, rec.Hash)
	}

	// —— 装进来的那一份等于目录里声明的那一份 ——
	mf, err := st.Manifest(rec.ID)
	if err != nil {
		t.Fatalf("读回 manifest 失败：%v", err)
	}
	entryRel := strings.TrimSpace(mf.Entry)
	if entryRel == "" {
		entryRel = surfaceDefaultEntry
	}
	installedEntry := filepath.Join(root, rec.ID, entryRel)
	// hash 的口径随安装模式而异，这两条断言只对**拷贝快照**成立：
	//   · 拷贝：hash = sha256(入口文件) —— 装进来的字节就是源字节；
	//   · dev：hash = **内容修订**（surfaceContentHash 覆盖全部受追踪文件，见 surfaces.go），
	//     改 panel.js / surface.json / icon.svg 任何一处都会变 —— 否则 watcher 认不出改动。
	if !*surfaceExternalDev {
		sum := sha256.Sum256(readBytes(t, installedEntry))
		wantHash := hex.EncodeToString(sum[:])
		if rec.Hash != wantHash || idx.Items[0].Hash != wantHash {
			t.Fatalf("hash = %q（回执）/ %q（index），want sha256(%s) = %q",
				rec.Hash, idx.Items[0].Hash, entryRel, wantHash)
		}
		// 源文件没被安装器改动（安装是「读源、写目标」，不是「就地加工」）。
		srcSum := sha256.Sum256(readBytes(t, filepath.Join(dir, entryRel)))
		if hex.EncodeToString(srcSum[:]) != wantHash {
			t.Fatalf("装进来的 %s 与源目录里的字节不同（安装器改了内容？）", entryRel)
		}
	} else if len(rec.Hash) < 8 {
		t.Fatalf("dev 安装的内容修订太短：%q", rec.Hash)
	}

	// —— entries：条数、id、能渲染的标题 ——
	if *rec.Entries != len(mf.Entries) {
		t.Fatalf("回执 entries = %d，want manifest 里的 %d 条", *rec.Entries, len(mf.Entries))
	}
	for i, e := range mf.Entries {
		if strings.TrimSpace(e.ID) == "" {
			t.Fatalf("entries[%d] 没有 id：%+v", i, e)
		}
		if strings.TrimSpace(e.Title.Primary()) == "" {
			t.Fatalf("entries[%d] (%s) 没有可渲染的标题：%+v", i, e.ID, e.Title)
		}
	}
	titles := make([]string, 0, len(mf.Entries))
	for _, e := range mf.Entries {
		titles = append(titles, fmt.Sprintf("%s=%q", e.ID, e.Title.Primary()))
	}
	if len(titles) == 0 {
		titles = append(titles, "（零 entries → 宿主合成一个默认入口）")
	}

	// —— 权限：目录 0700 / 文件 0600（与他人共享的机器上，插件目录是用户数据）——
	// dev 安装落盘的是软链：这几处 os.Stat 会跟到**源目录**上，权限归源目录作者所有，
	// 安装器不该（也做不到）替它改权限 —— 只对拷贝快照断言。
	if !*surfaceExternalDev {
		wantMode(t, filepath.Join(root, rec.ID), surfaceDirMode)
		wantMode(t, filepath.Join(root, rec.ID, "surface.json"), surfaceFileMode)
		wantMode(t, installedEntry, surfaceFileMode)
	} else if fi, lerr := os.Lstat(filepath.Join(root, rec.ID)); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dev 安装的 %s 不是软链：%v", filepath.Join(root, rec.ID), lerr)
	}

	t.Logf("✅ 装上 %s → %s（%d 个入口：%s）", dir, filepath.Join(root, rec.ID), *rec.Entries, strings.Join(titles, ", "))
}

// resolveExternalDir 把 -dir 解析成绝对路径。
//
// 为什么不能只用 filepath.Abs：`go test ./desktop/bridge` 的**进程 cwd 是包目录**
// （desktop/bridge），而人在仓库根敲 `-dir=templates/surface` 才是自然的写法。所以按
// 「绝对路径 → 相对 cwd → 相对仓库根（go.mod 所在目录）」依次试，第一个存在且是目录的胜出；
// 都不存在时把试过的路径全列出来（报错里没有路径 = 使用者只能猜）。
func resolveExternalDir(raw string) (string, error) {
	if filepath.IsAbs(raw) {
		if st, err := os.Stat(raw); err == nil && st.IsDir() {
			return raw, nil
		}
		return "", fmt.Errorf("不是可读目录：%s", raw)
	}
	tried := []string{}
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, raw))
	}
	if root, err := moduleRoot(); err == nil {
		candidates = append(candidates, filepath.Join(root, raw))
	}
	for _, c := range candidates {
		tried = append(tried, c)
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("不是可读目录（试过：%s）", strings.Join(tried, " / "))
}

// moduleRoot 从 cwd 向上找 go.mod（`go test ./desktop/bridge` 的 cwd 是包目录）。
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("向上找不到 go.mod")
		}
		dir = parent
	}
}

// TestSurfaceInstallExternalRejectsMissingPanel 反例：**缺入口文件的目录必须装不上**。
//
// 不需要任何参数，所以它也在普通 `go test ./desktop/bridge -run TestSurface` 里跑 ——
// 验收工具最容易退化成「只会说 OK」的东西，这条把「它会说 NO」钉在门禁里。
func TestSurfaceInstallExternalRejectsMissingPanel(t *testing.T) {
	st, root := testSurfaceStore(t)
	dir := t.TempDir() // 只有 surface.json，没有 panel.js
	raw, err := json.MarshalIndent(map[string]any{
		"id": "no-panel", "name": map[string]any{"zh": "缺入口"}, "version": "1.0.0",
		"api": 1, "kind": "plugin:no-panel", "entry": "panel.js",
	}, "", "  ")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "surface.json"), raw, 0o600); err != nil {
		t.Fatalf("写 surface.json: %v", err)
	}

	rec := st.Install(dir, true)
	recJSON, _ := json.MarshalIndent(rec, "", "  ")
	t.Logf("缺 panel.js 的回执：\n%s", recJSON)
	if rec.Status != surfaceRejected {
		t.Fatalf("status = %q，want rejected（缺入口文件不许装进来）", rec.Status)
	}
	if !strings.Contains(rec.Reason, surfaceDefaultEntry) {
		t.Fatalf("reason = %q，必须点名 %s（模型据此补文件）", rec.Reason, surfaceDefaultEntry)
	}
	// 失败不许留下半装状态：没有 <id> 目录，index 也没有这条记录。
	notExist(t, filepath.Join(root, "no-panel"))
	if idx := st.List(false); len(idx.Items) != 0 {
		t.Fatalf("装失败却写进了 index：%+v", idx.Items)
	}
	noStagingLeftovers(t, root)
}
