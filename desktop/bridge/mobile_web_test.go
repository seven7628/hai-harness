package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// P8-03b：/m/ 默认页面是新的 Vite 产物（dist-mobile → mobile_web.zip）。
// 旧的无构建页（mobile_web/app.js 等）已退役 —— 这里的断言同时防回归：
// 新页必须在 /m/ 根直接可服务、assets 相对路径能取到、旧文件不再出现。

var (
	mobileWebIndexScript = regexp.MustCompile(`src="\./(assets/[^"]+\.js)"`)
	mobileWebIndexStyle  = regexp.MustCompile(`href="\./(assets/[^"]+\.css)"`)
)

// zip 内容：目录结构正确、体积可控、没有旧页文件、条目路径全安全。
func TestMobileWebZipLayout(t *testing.T) {
	if len(mobileWebZip) > 16<<20 {
		t.Fatalf("mobile_web.zip %d 字节过大（原始 dist-mobile 不该入库）", len(mobileWebZip))
	}
	zr, err := zip.NewReader(bytes.NewReader(mobileWebZip), int64(len(mobileWebZip)))
	if err != nil {
		t.Fatalf("读 mobile_web.zip 失败: %v", err)
	}
	var haveIndex, haveJS, haveCSS bool
	var total uint64
	names := map[string]bool{}
	for _, f := range zr.File {
		if _, ok := safeMobileWebPath(f.Name); !ok {
			t.Errorf("zip 条目路径非法: %q", f.Name)
		}
		names[f.Name] = true
		total += f.UncompressedSize64
		switch {
		case f.Name == "index.html":
			haveIndex = true
		case strings.HasPrefix(f.Name, "assets/") && strings.HasSuffix(f.Name, ".js"):
			haveJS = true
		case strings.HasPrefix(f.Name, "assets/") && strings.HasSuffix(f.Name, ".css"):
			haveCSS = true
		}
	}
	if !haveIndex || !haveJS || !haveCSS {
		t.Fatalf("zip 结构不对：index.html=%v js=%v css=%v", haveIndex, haveJS, haveCSS)
	}
	if len(zr.File) < 100 {
		t.Errorf("zip 只有 %d 个条目，像是没把 dist-mobile 打全", len(zr.File))
	}
	if total > 40<<20 {
		t.Errorf("zip 解压后 %d 字节，远超预期", total)
	}
	for _, gone := range []string{
		"mobile.html", "app.js", "hub_model.js", "avatar_model.js",
		"luna-face.js", "mascot-bots.js", "style.css", "tokens.css",
	} {
		if names[gone] {
			t.Errorf("zip 里还有旧页文件 %s", gone)
		}
	}
}

// 内置页：/m/ 直接出 index.html，assets 相对路径能取到，旧页痕迹全无。
func TestMobileWebEmbeddedServesNewPage(t *testing.T) {
	useTempMobileWebCache(t)
	t.Setenv("HAI_MOBILE_WEB_DIR", "")
	_, ts := testMobileServer(t)

	resp, err := http.Get(ts.URL + "/m/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/m/ = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q，应为 no-store", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{`<div id="root">`, `type="module"`, `assets/`} {
		if !strings.Contains(html, want) {
			t.Errorf("/m/ 的 HTML 缺 %q", want)
		}
	}
	for _, old := range []string{"app.js", "hub_model.js", "avatar_model.js", "tokens.css", "style.css", "mobile.html"} {
		if strings.Contains(html, old) {
			t.Errorf("/m/ 仍引用旧页文件 %q", old)
		}
	}

	// 首屏脚本与样式都要能按 index.html 里的相对路径取到。
	for _, re := range []*regexp.Regexp{mobileWebIndexScript, mobileWebIndexStyle} {
		m := re.FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("index.html 里找不到 %v 的产物引用", re)
		}
		ar, err := http.Get(ts.URL + "/m/" + m[1])
		if err != nil {
			t.Fatal(err)
		}
		ab, _ := io.ReadAll(ar.Body)
		ar.Body.Close()
		if ar.StatusCode != http.StatusOK {
			t.Fatalf("/m/%s = %d", m[1], ar.StatusCode)
		}
		if len(ab) == 0 {
			t.Fatalf("/m/%s 内容为空", m[1])
		}
	}

	// / → /m/ 仍要跳转（手机扫完码直接敲域名也能到页面）
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	rr, err := noFollow.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	rr.Body.Close()
	if rr.StatusCode != http.StatusFound || rr.Header.Get("Location") != "/m/" {
		t.Fatalf("/ = %d Location=%q，应为 302 /m/", rr.StatusCode, rr.Header.Get("Location"))
	}
}

// 开发态覆盖：HAI_MOBILE_WEB_DIR 指向磁盘目录时优先，不存在则回落内置页。
func TestMobileWebDiskOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>disk-override</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "probe.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	useTempMobileWebCache(t)
	t.Setenv("HAI_MOBILE_WEB_DIR", dir)
	_, ts := testMobileServer(t)

	body := getBody(t, ts.URL+"/m/")
	if !strings.Contains(body, "disk-override") {
		t.Fatalf("HAI_MOBILE_WEB_DIR 未生效，/m/ = %q", body)
	}
	if code, _ := getStatus(t, ts.URL+"/m/assets/probe.js"); code != http.StatusOK {
		t.Fatalf("/m/assets/probe.js = %d", code)
	}

	// 目录不存在 → 回落内置页，不能变成 404
	t.Setenv("HAI_MOBILE_WEB_DIR", filepath.Join(dir, "nope"))
	_, ts2 := testMobileServer(t)
	body = getBody(t, ts2.URL+"/m/")
	if !strings.Contains(body, `<div id="root">`) {
		t.Fatalf("覆盖目录不存在时应回落内置页，/m/ = %q", body)
	}
}

// 解压：目录可复用（第二次不重解）、并发只装一次、缓存键随 zip 内容变。
func TestMobileWebExtractReuseAndConcurrency(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HAI_MOBILE_WEB_CACHE", base)
	resetMobileWebCache()
	t.Cleanup(resetMobileWebCache)

	root, err := mobileWebDir()
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if !strings.HasPrefix(root, base) {
		t.Errorf("缓存目录 %q 不在 HAI_MOBILE_WEB_CACHE(%q) 下", root, base)
	}
	if !mobileWebReady(root) {
		t.Fatalf("解压后目录未就绪: %s", root)
	}
	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		t.Fatalf("缺 index.html: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "assets"))
	if err != nil || len(entries) < 100 {
		t.Fatalf("assets 没解出来（%d 个）: %v", len(entries), err)
	}

	// 复用：手动塞一个哨兵文件，再调一次（清掉进程内记忆模拟新进程）→ 仍在。
	sentinel := filepath.Join(root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	resetMobileWebCache()
	again, err := mobileWebDir()
	if err != nil {
		t.Fatal(err)
	}
	if again != root {
		t.Errorf("第二次解压换了目录: %q != %q", again, root)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("第二次解压把已就绪的目录重解了（哨兵丢失）: %v", err)
	}

	// 并发：8 个 goroutine 同时装同一个缓存键，只应有一个目录、全部无错。
	concBase := t.TempDir()
	t.Setenv("HAI_MOBILE_WEB_CACHE", concBase)
	resetMobileWebCache()
	var wg sync.WaitGroup
	roots := make([]string, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			roots[i], errs[i] = mobileWebRoot(mobileWebZip)
		}(i)
	}
	wg.Wait()
	for i := range roots {
		if errs[i] != nil {
			t.Fatalf("并发解压第 %d 个失败: %v", i, errs[i])
		}
		if roots[i] != roots[0] {
			t.Fatalf("并发解压出了多个目录: %q vs %q", roots[i], roots[0])
		}
	}
	if !mobileWebReady(roots[0]) {
		t.Fatalf("并发解压后目录未就绪: %s", roots[0])
	}
	// 半成品临时目录不能留在缓存根下。
	siblings, err := os.ReadDir(filepath.Join(concBase, "hai", "mobile-web"))
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 1 {
		t.Fatalf("缓存根下应只有 1 个目录，实际 %d 个", len(siblings))
	}
}

// zip 条目路径校验：绝对路径 / `..` / 反斜杠 / 符号链接一律拒绝，且不落地。
func TestMobileWebRejectsUnsafeZipEntries(t *testing.T) {
	cases := map[string]string{
		"parent":    "../evil.txt",
		"nested":    "assets/../../evil.txt",
		"absolute":  "/etc/evil.txt",
		"backslash": `assets\evil.txt`,
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			dst := t.TempDir()
			err := extractMobileWebZip(zipWith(t, map[string]string{entry: "x", "index.html": "<html></html>"}), dst)
			if err == nil {
				t.Fatalf("条目 %q 应被拒绝", entry)
			}
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dst), "evil.txt")); statErr == nil {
				t.Fatalf("条目 %q 写出了目标目录", entry)
			}
		})
	}

	// 缺 index.html 的 zip 也要报错（否则装出一个「成功但 /m/ 404」的目录）
	if err := extractMobileWebZip(zipWith(t, map[string]string{"assets/a.js": "x"}), t.TempDir()); err == nil {
		t.Fatal("缺 index.html 应报错")
	}
}

// 解压失败必须显式报错，不能静默 404（/m/ 返回 500 + 原因）。
func TestMobileWebExtractFailureIsLoud(t *testing.T) {
	t.Setenv("HAI_MOBILE_WEB_DIR", "")
	old := mobileWebRootFn
	mobileWebRootFn = func() (string, error) { return "", errors.New("boom-zip") }
	t.Cleanup(func() { mobileWebRootFn = old })

	_, ts := testMobileServer(t)
	resp, err := http.Get(ts.URL + "/m/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/m/ = %d，解压失败应为 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "boom-zip") {
		t.Fatalf("500 响应没带原因: %q", body)
	}

	// 缓存不可写 → 回落到临时目录，页面仍能开（不是直接失败）。
	// 用「父路径是普通文件」制造 MkdirAll 失败（比 chmod 0555 更可移植）。
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAI_MOBILE_WEB_CACHE", blocker)
	root, err := mobileWebRoot(mobileWebZip)
	if err != nil {
		t.Fatalf("缓存目录不可用时应回落临时目录: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if !mobileWebReady(root) {
		t.Fatalf("回落目录未就绪: %s", root)
	}
}

// useTempMobileWebCache 把解压目录钉到测试临时目录（别写用户 ~/Library/Caches）。
func useTempMobileWebCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HAI_MOBILE_WEB_CACHE", dir)
	resetMobileWebCache()
	t.Cleanup(resetMobileWebCache)
	return dir
}

func zipWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// safeMobileWebPath 的单测：正常相对路径要保留，越界一律 false。
func TestSafeMobileWebPath(t *testing.T) {
	ok := []string{"index.html", "assets/a.js", "./assets/a.js", "assets/deep/x.css"}
	for _, name := range ok {
		if _, valid := safeMobileWebPath(name); !valid {
			t.Errorf("%q 应通过", name)
		}
	}
	if p, _ := safeMobileWebPath("./assets/a.js"); p != path.Join("assets", "a.js") {
		t.Errorf("./assets/a.js → %q", p)
	}
	bad := []string{"", "/abs", "../up", "a/../../b", `a\b`, ".", "..", "C:/x"}
	for _, name := range bad {
		if _, valid := safeMobileWebPath(name); valid {
			t.Errorf("%q 应被拒绝", name)
		}
	}
}
