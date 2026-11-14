package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// P8 方案 A：手机页是**一个磁盘目录**（dist-mobile / Resources/mobile-web），
// 不 embed、不 zip、不做运行时解压缓存。这里锁三件事：
//   1. 目录里有 index.html 时 /m/ 直接可服务，assets 相对路径能取到；
//   2. HAI_MOBILE_WEB_DIR 缺失/不是目录/缺 index.html 时 /m/ 返回 500 + 明确原因；
//   3. / → /m/ 的跳转仍在。
//
// 旧无构建页（手写 app.js 那套）已退役，不再有内置回退。

var (
	mobileWebIndexScript = regexp.MustCompile(`src="\./(assets/[^"]+\.js)"`)
	mobileWebIndexStyle  = regexp.MustCompile(`href="\./(assets/[^"]+\.css)"`)
)

func writeMobileWebFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	html := `<!doctype html><html><body><div id="root"></div>` +
		`<script type="module" src="./assets/main.js"></script>` +
		`<link rel="stylesheet" href="./assets/main.css"></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "main.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "main.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMobileWebSubRequiresDir(t *testing.T) {
	if _, err := mobileWebSub(""); err == nil || !strings.Contains(err.Error(), "HAI_MOBILE_WEB_DIR") {
		t.Fatalf("空目录应返回明确错误，得到 %v", err)
	}
	if _, err := mobileWebSub(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("不存在的目录应报错")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mobileWebSub(file); err == nil || !strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("文件路径应报「不是目录」，得到 %v", err)
	}
	if _, err := mobileWebSub(t.TempDir()); err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("缺 index.html 应报错，得到 %v", err)
	}
}

func TestMobileWebDiskDirServesNewPage(t *testing.T) {
	dir := writeMobileWebFixture(t)
	t.Setenv("HAI_MOBILE_WEB_DIR", dir)
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
		if ar.StatusCode != http.StatusOK || len(ab) == 0 {
			t.Fatalf("/m/%s = %d，内容 %d 字节", m[1], ar.StatusCode, len(ab))
		}
	}
}

func TestMobileWebMissingReturns500(t *testing.T) {
	t.Setenv("HAI_MOBILE_WEB_DIR", "")
	_, ts := testMobileServer(t)
	code, body := getStatus(t, ts.URL+"/m/")
	if code != http.StatusInternalServerError {
		t.Fatalf("/m/ 无目录 = %d，want 500", code)
	}
	if !strings.Contains(body, "HAI_MOBILE_WEB_DIR") {
		t.Fatalf("/m/ 500 应说明缺 HAI_MOBILE_WEB_DIR，得到 %q", body)
	}
}

func TestMobileWebDiskOverrideDoesNotFallback(t *testing.T) {
	dir := writeMobileWebFixture(t)
	t.Setenv("HAI_MOBILE_WEB_DIR", dir)
	_, ts := testMobileServer(t)
	if body := getBody(t, ts.URL+"/m/"); !strings.Contains(body, `<div id="root">`) {
		t.Fatalf("磁盘目录未生效，/m/ = %q", body)
	}

	// 方案 A 没有内置回退：覆盖目录失效就明确 500，不能悄悄服务旧页。
	t.Setenv("HAI_MOBILE_WEB_DIR", filepath.Join(dir, "nope"))
	_, ts2 := testMobileServer(t)
	code, body := getStatus(t, ts2.URL+"/m/")
	if code != http.StatusInternalServerError || !strings.Contains(body, "HAI_MOBILE_WEB_DIR") {
		t.Fatalf("失效目录 = %d %q，want 500 + 原因", code, body)
	}
}

func TestMobileRootRedirectsToM(t *testing.T) {
	t.Setenv("HAI_MOBILE_WEB_DIR", writeMobileWebFixture(t))
	_, ts := testMobileServer(t)
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

func getBody(t *testing.T, url string) string {
	t.Helper()
	code, body := getStatus(t, url)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, code, body)
	}
	return body
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
