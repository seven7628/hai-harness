package officekit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSoffice 写一个可执行的「假 soffice」脚本（内容即返回值）。测试全程离线。
func fakeSoffice(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeKit 在 <root>/<version>/<platform>/ 落一套 kit（假二进制 + KIT.json），返回目录。
// mutate 可选地改 KIT.json（做 sha256/版本/平台不匹配的用例）。
func writeKit(t *testing.T, root, version, platform string, mutate func(*Manifest)) string {
	t.Helper()
	dir := filepath.Join(root, version, platform)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "soffice")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(bin)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Version: version, Platform: platform, SHA256: sum,
		BuiltAt: "2026-11-13", License: licenseName, LicenseURL: licenseURL, SourceURL: sourceURL,
	}
	if mutate != nil {
		mutate(&m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// noSystem 让「系统 soffice 不存在」成为确定事实（不受本机是否装了 LibreOffice 影响）。
func noSystem(opts ProbeOptions) ProbeOptions {
	opts.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	opts.SystemPaths = []string{filepath.Join(os.TempDir(), "no-such-libreoffice")}
	return opts
}

// 探测顺序：PATH 头部的 soffice 优先（真 exec.LookPath，不注入）。
//
// 为什么必须走真 LookPath：这条断言的语义就是「PATH 里有就先用它」，
// 注入一个假 LookPath 只能证明「我调了那个函数」，证不了查找真的按 PATH 顺序。
func TestProbePrefersSystemSofficeOnPath(t *testing.T) {
	dir := t.TempDir()
	bin := fakeSoffice(t, dir, "soffice", "exit 0\n")
	// 同时放一套「可用的 kit」：系统版优先才是有意义的断言（否则只是「只有它」）。
	kitRoot := t.TempDir()
	writeKit(t, kitRoot, PinnedVersion, CurrentPlatform(), nil)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st := Probe(ProbeOptions{KitRoot: kitRoot})
	if !st.Available || st.Source != string(SourceSystem) {
		t.Fatalf("期望系统 soffice 优先，得到 available=%v source=%q reason=%q", st.Available, st.Source, st.Reason)
	}
	if st.Bin != bin {
		t.Fatalf("Bin = %q，期望 PATH 里的 %q", st.Bin, bin)
	}
}

// 没有系统 soffice → 回落到 kit 目录。
func TestProbeFallsBackToKit(t *testing.T) {
	kitRoot := t.TempDir()
	dir := writeKit(t, kitRoot, PinnedVersion, CurrentPlatform(), nil)
	st := Probe(noSystem(ProbeOptions{KitRoot: kitRoot}))
	if !st.Available || st.Source != string(SourceKit) {
		t.Fatalf("期望回落 kit，得到 available=%v state=%q reason=%q errs=%v", st.Available, st.State, st.Reason, st.Errors)
	}
	if st.Bin != filepath.Join(dir, "soffice") {
		t.Fatalf("Bin = %q，期望 %q", st.Bin, filepath.Join(dir, "soffice"))
	}
	if st.Kit == nil || st.Kit.Version != PinnedVersion {
		t.Fatalf("Kit = %+v", st.Kit)
	}
}

// 三种校验失败各一条：sha256 不匹配 / 版本不匹配 / 文件缺失 → 一律不可用。
func TestProbeRejectsBadKit(t *testing.T) {
	platform := CurrentPlatform()
	cases := []struct {
		name    string
		mutate  func(*Manifest)
		missing string // 非空 = 删掉这个文件
		want    string // 期望出错串里出现的片段
	}{
		{name: "sha256", mutate: func(m *Manifest) { m.SHA256 = strings.Repeat("0", 64) }, want: "sha256"},
		{name: "version", mutate: func(m *Manifest) { m.Version = "0.0.0-old" }, want: "版本"},
		{name: "no-binary", missing: "soffice", want: "可执行文件"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := writeKit(t, root, PinnedVersion, platform, tc.mutate)
			if tc.missing != "" {
				if err := os.Remove(filepath.Join(dir, tc.missing)); err != nil {
					t.Fatal(err)
				}
			}
			st := Probe(noSystem(ProbeOptions{KitRoot: root}))
			if st.Available || st.Kit != nil {
				t.Fatalf("坏 kit 被当成可用：state=%q bin=%q", st.State, st.Bin)
			}
			if st.State != StateInvalid {
				t.Fatalf("state = %q，期望 invalid（reason=%q）", st.State, st.Reason)
			}
			joined := strings.Join(append([]string{st.Reason}, st.Errors...), " | ")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("原因里没有 %q：%s", tc.want, joined)
			}
		})
	}
}

// 完全没装 → missing（不是 invalid）：设置页要说「未安装」而不是「装坏了」。
func TestProbeMissing(t *testing.T) {
	st := Probe(noSystem(ProbeOptions{KitRoot: filepath.Join(t.TempDir(), "absent")}))
	if st.Available || st.State != StateMissing {
		t.Fatalf("期望 missing，得到 available=%v state=%q", st.Available, st.State)
	}
	if st.Reason == "" {
		t.Fatal("missing 也要给人话原因（设置页/横幅要展示）")
	}
}

// 只装了别的版本：必须报 invalid 且写明版本，**不许**静默回退到那个版本。
func TestProbeOtherVersionOnlyIsNotSilentlyUsed(t *testing.T) {
	root := t.TempDir()
	writeKit(t, root, "0.9.0", CurrentPlatform(), func(m *Manifest) { m.Version = "0.9.0" })
	st := Probe(noSystem(ProbeOptions{KitRoot: root}))
	if st.Available {
		t.Fatalf("用了非钉住版本的 kit：bin=%q", st.Bin)
	}
	if st.State != StateInvalid || !strings.Contains(strings.Join(st.Errors, " "), "0.9.0") {
		t.Fatalf("期望报出「只有 0.9.0」，得到 state=%q errs=%v", st.State, st.Errors)
	}
}

// Prober 缓存 + Refresh：安装/卸载发生在本进程之外，刷新必须真的重探。
func TestProberCachesAndRefreshes(t *testing.T) {
	root := t.TempDir()
	opts := noSystem(ProbeOptions{KitRoot: root})
	p := NewProber(opts)

	if st := p.Status(); st.Available {
		t.Fatal("空目录不该可用")
	}
	writeKit(t, root, PinnedVersion, CurrentPlatform(), nil)
	if st := p.Status(); st.Available {
		t.Fatal("缓存没有生效（未 Refresh 就看到新 kit）")
	}
	if st := p.Refresh(); !st.Available {
		t.Fatalf("Refresh 后应可用：state=%q reason=%q", st.State, st.Reason)
	}
	if p.Kit() == nil {
		t.Fatal("Refresh 后 Kit() 应为非 nil")
	}
}

// 平台矩阵：四个平台的供法/许可/体积口径齐全（设置页与 PR-13 都按这张表来）。
func TestPlatformMatrix(t *testing.T) {
	want := []string{"darwin-arm64", "darwin-x64", "windows-x64", "linux-x64"}
	for _, id := range want {
		info, ok := PlatformOf(id)
		if !ok {
			t.Fatalf("平台矩阵缺 %s", id)
		}
		if info.License == "" || info.LicenseURL == "" || info.SourceURL == "" {
			t.Fatalf("%s 缺许可/源码说明（MPL/LGPL 分发合规要求）", id)
		}
		if info.Supply == "kit" && info.SizeBytes <= 0 {
			t.Fatalf("%s 随包分发却没有体积口径", id)
		}
		if id == "linux-x64" && info.Supply != "system" {
			t.Fatalf("linux 应复用系统 soffice，得到 %q", info.Supply)
		}
	}
	// sha256 校验的期待值来自 KIT.json；本机平台必须在矩阵里，否则设置页无从解释。
	if _, ok := PlatformOf(CurrentPlatform()); !ok {
		t.Fatalf("本机平台 %s 不在矩阵里", CurrentPlatform())
	}
}
