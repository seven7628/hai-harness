// mobile_web.go —— 把手机新页（Vite dist-mobile）打进 bridge 二进制。
//
// P8-03b：旧的无构建页（mobile_web/*.js 手写 PWA）退役，/m/ 默认改成新页。
// 新页是 Vite 产物（452 个文件、18MB），直接 go:embed 目录会把几百个哈希名文件塞进
// git，每次构建都是大片删除+新增；所以 embed 的是**一个确定性 zip**
// （desktop/bridge/mobile_web.zip，约 5.3MB，由 desktop/app/scripts/build-mobile-embed.mjs
// 生成，npm run mobile:embed）。`go build` 在干净检出里只依赖这个已提交的 zip。
//
// 运行时把 zip 解到缓存目录再交给 http.FileServer —— go:embed 的 FS 不能直接当
// http.FileSystem 用（没有目录索引/Seek 语义），而 zip 里的文件数不值得常驻内存。
//
// 缓存键是 zip 的 SHA-256 前缀：换版本自然落到新目录，旧目录留着不影响正确性
// （同版本重复启动只解一次）。解压走「临时目录 + rename + 完成标记」，多进程并发
// 启动时只有一个能 rename 成功，另一个直接复用已就绪的目录。
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed mobile_web.zip
var mobileWebZip []byte

// mobileWebMarker 完成标记：只有它存在才认为缓存目录可用（rename 是原子的，
// 标记在 rename 之前写进临时目录，所以「有标记」== 「解压完整」）。
const mobileWebMarker = ".hai-mobile-web.complete"

// mobileWebCacheBase 缓存根目录。默认 os.UserCacheDir()（macOS: ~/Library/Caches）；
// 可用 HAI_MOBILE_WEB_CACHE 覆盖（测试/排障用）。
var mobileWebCacheBase = func() (string, error) {
	if dir := os.Getenv("HAI_MOBILE_WEB_CACHE"); dir != "" {
		return dir, nil
	}
	return os.UserCacheDir()
}

// mobileWebRootFn 解压入口：测试注入失败用，生产恒为 mobileWebDir。
var mobileWebRootFn = mobileWebDir

var (
	mobileWebMu     sync.Mutex
	mobileWebCached string
	mobileWebErr    error
)

// mobileWebDir 内置手机页的解压目录（含 index.html）。进程内只解一次。
func mobileWebDir() (string, error) {
	mobileWebMu.Lock()
	defer mobileWebMu.Unlock()
	if mobileWebCached != "" || mobileWebErr != nil {
		return mobileWebCached, mobileWebErr
	}
	mobileWebCached, mobileWebErr = mobileWebRoot(mobileWebZip)
	return mobileWebCached, mobileWebErr
}

// resetMobileWebCache 丢掉进程内记忆（测试用；不清磁盘）。
func resetMobileWebCache() {
	mobileWebMu.Lock()
	defer mobileWebMu.Unlock()
	mobileWebCached, mobileWebErr = "", nil
}

// mobileWebRoot 解出（或复用）缓存目录，返回含 index.html 的目录。
func mobileWebRoot(zipBytes []byte) (string, error) {
	if len(zipBytes) == 0 {
		return "", errors.New("内置手机页 zip 为空（bridge 构建时缺 desktop/bridge/mobile_web.zip）")
	}
	sum := sha256.Sum256(zipBytes)
	key := hex.EncodeToString(sum[:8])

	var cacheErr error
	if base, err := mobileWebCacheBase(); err == nil && base != "" {
		root := filepath.Join(base, "hai", "mobile-web", key)
		if mobileWebReady(root) {
			return root, nil
		}
		if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
			cacheErr = err
		} else if err := installMobileWeb(zipBytes, key, root); err != nil {
			cacheErr = err
		} else {
			return root, nil
		}
	} else if err != nil {
		cacheErr = err
	}

	// 缓存目录不可用（只读 HOME、权限、沙箱）：退回一次性临时目录，页面照样能开，
	// 只是每次启动多解一遍。这里失败才算真的失败 —— 上层会把错误写进 /m/ 的响应。
	tmp, err := os.MkdirTemp("", "hai-mobile-web-")
	if err != nil {
		return "", fmt.Errorf("手机页解压失败（缓存目录: %v；临时目录: %v）", cacheErr, err)
	}
	if err := extractMobileWebZip(zipBytes, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("手机页解压失败: %w", err)
	}
	// 临时目录也写标记：mobileWebReady 只有一种语义（有标记 = 完整）。
	if err := writeMobileWebMarker(tmp, key); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

// mobileWebReady 目录里既有完成标记又有 index.html 才算就绪。
func mobileWebReady(root string) bool {
	if root == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, mobileWebMarker)); err != nil {
		return false
	}
	if st, err := os.Stat(filepath.Join(root, "index.html")); err != nil || st.IsDir() {
		return false
	}
	return true
}

func writeMobileWebMarker(dir, key string) error {
	return os.WriteFile(filepath.Join(dir, mobileWebMarker), []byte(key+"\n"), 0o644)
}

// installMobileWeb 原子安装到 root：解到兄弟临时目录 → 写标记 → rename。
func installMobileWeb(zipBytes []byte, key, root string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(root), filepath.Base(root)+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extractMobileWebZip(zipBytes, tmp); err != nil {
		return err
	}
	if err := writeMobileWebMarker(tmp, key); err != nil {
		return err
	}
	if err := os.Rename(tmp, root); err == nil {
		return nil
	}
	// 目标已存在：另一进程刚装好就复用；是半成品/旧残留就清掉重来一次。
	if mobileWebReady(root) {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	return os.Rename(tmp, root)
}

// extractMobileWebZip 把 zip 解到 dst（dst 必须已存在且为空目录）。
// 条目路径一律校验：绝对路径、`..`、反斜杠、符号链接都不许落地 —— zip 是
// 构建产物，但没有理由让它能写穿目标目录。
func extractMobileWebZip(zipBytes []byte, dst string) error {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return fmt.Errorf("读 zip 失败: %w", err)
	}
	for _, f := range zr.File {
		rel, ok := safeMobileWebPath(f.Name)
		if !ok {
			return fmt.Errorf("zip 条目路径非法: %q", f.Name)
		}
		target := filepath.Join(dst, rel)
		if !strings.HasPrefix(target, dst+string(os.PathSeparator)) {
			return fmt.Errorf("zip 条目越界: %q", f.Name)
		}
		mode := f.Mode()
		if mode&fs.ModeSymlink != 0 {
			return fmt.Errorf("zip 条目是符号链接（不支持）: %q", f.Name)
		}
		if mode.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() {
			return fmt.Errorf("zip 条目不是普通文件: %q", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeMobileWebFile(f, target); err != nil {
			return err
		}
	}
	// 完成标记由 installMobileWeb 在 rename 前写入，不来自 zip。这里只兜住
	// index.html 缺失，免得装出一个「解压成功但 /m/ 404」的目录。
	if _, err := os.Stat(filepath.Join(dst, "index.html")); err != nil {
		return fmt.Errorf("zip 里没有 index.html: %w", err)
	}
	return nil
}

func writeMobileWebFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// safeMobileWebPath 把 zip 条目名转成安全的相对路径。
func safeMobileWebPath(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, '\\') {
		return "", false
	}
	if path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", false
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	// 驱动器号（C:/...）在 Windows 上等于绝对路径
	if len(clean) >= 2 && clean[1] == ':' {
		return "", false
	}
	return filepath.FromSlash(clean), true
}
