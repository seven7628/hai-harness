package officekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeConvertScript 假 soffice 的脚本体。**路径全部写死在脚本里**（这一点是被迫的，
// 也是对的）：sandbox.RunArgs 走 isolatedEnv，子进程只看得到 PATH/HOME/TMPDIR/代理，
// 测试不能靠环境变量传参 —— 那正好也证明了生产路径没有偷偷依赖调用方的环境。
//
// 参数：log = 每次调用的 argv 摘要；plog = "pid profileUrl"（并发/杀组断言用）；
// sleep = 每次调用先睡多久；failFirst = 第一次调用退出码 1（预热失败用例）。
func fakeConvertScript(log, plog, counter string, sleep time.Duration, failFirst, writePDF bool) string {
	var b strings.Builder
	b.WriteString("LOG='" + log + "'\n")
	b.WriteString("PLOG='" + plog + "'\n")
	b.WriteString("COUNTER='" + counter + "'\n")
	b.WriteString(`PROFILE=''
OUTDIR=''
SRC=''
while [ $# -gt 0 ]; do
  case "$1" in
    -env:UserInstallation=*) PROFILE="${1#-env:UserInstallation=}" ;;
    --outdir) shift; OUTDIR="$1" ;;
    *) SRC="$1" ;;
  esac
  shift
done
echo "$$ $PROFILE" >> "$PLOG"
`)
	if sleep > 0 {
		// 先记 pid 再睡：超时用例要在进程还在睡的时候就能拿到 pid。
		fmt.Fprintf(&b, "sleep %g\n", sleep.Seconds())
	}
	if failFirst {
		b.WriteString(`N=$(cat "$COUNTER" 2>/dev/null || echo 0)
N=$((N+1))
echo "$N" > "$COUNTER"
if [ "$N" -eq 1 ]; then echo "first run fails" >&2; exit 1; fi
`)
	} else {
		b.WriteString("echo 1 > \"$COUNTER\"\n")
	}
	if writePDF {
		b.WriteString(`if [ -n "$OUTDIR" ] && [ -n "$SRC" ]; then
  base=$(basename "$SRC")
  base=${base%.*}
  printf '%%PDF-1.4\n%% fake soffice output\n' > "$OUTDIR/$base.pdf"
fi
`)
	}
	b.WriteString("exit 0\n")
	return b.String()
}

// kitWithScript 造一个「已通过校验」的 kit，二进制是给定的假脚本。
func kitWithScript(t *testing.T, body string) *Kit {
	t.Helper()
	dir := t.TempDir()
	bin := fakeSoffice(t, dir, "soffice", body)
	return &Kit{Bin: bin, Source: SourceKit, Version: PinnedVersion}
}

// srcDoc 造一个输入文件（内容无所谓：假 soffice 不解析它）。
func srcDoc(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("fake docx bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// profileOf 从 file URL 取回本地路径。
func profileOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		t.Fatalf("UserInstallation 不是 file URL: %q (%v)", raw, err)
	}
	return u.Path
}

// 并发 3 个 job：三个 UserInstallation 互不相同、都在临时目录下，
// 且没有任何两个 job 共用同一个 profile 目录。
//
// 这是本 PR 最贵的一条纪律（05 §8.2 第 1 条）：共用 profile 时第二个转换会撞
// LibreOffice 的 user-installation 锁，表现为「转第二个文件就没反应」。
//
// **剖面刻意共享同一个 ProfileRoot**：生产里它是 job 临时目录，这里必须用共享根，
// 否则「固定目录名」这种变异会被各自独立的根掩盖掉（实测：改成固定名，用独立根时
// 这条用例仍然全绿 —— 等于没测）。
func TestConvertIsolatesProfilesPerJob(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	plog := filepath.Join(t.TempDir(), "profiles.log")
	kit := kitWithScript(t, fakeConvertScript(log, plog, filepath.Join(t.TempDir(), "n"), 150*time.Millisecond, false, true))
	src := srcDoc(t, t.TempDir(), "deck.pptx")
	profileRoot := t.TempDir()

	const jobs = 3
	outDirs := make([]string, jobs)
	for i := range outDirs {
		outDirs[i] = t.TempDir()
	}
	var wg sync.WaitGroup
	errs := make([]error, jobs)
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = kit.Convert(context.Background(), src, outDirs[i], ConvertOptions{ProfileRoot: profileRoot})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("job %d 失败: %v", i, err)
		}
	}

	raw, err := os.ReadFile(plog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != jobs {
		t.Fatalf("假 soffice 被调 %d 次，期望 %d：\n%s", len(lines), jobs, raw)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("plog 行格式不对: %q", line)
		}
		path := profileOf(t, fields[1])
		if seen[path] {
			t.Fatalf("两个 job 共用了同一个 profile 目录: %s", path)
		}
		seen[path] = true
		if !strings.HasPrefix(path, profileRoot+string(os.PathSeparator)) {
			t.Fatalf("profile %s 不在临时目录 %s 下", path, profileRoot)
		}
		// 每次转换的 profile 都是**新建的空目录**，不是一个被复用的既有目录。
		if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("profile %s 在转换结束后应被清理（stat err = %v）", path, serr)
		}
	}
	if len(seen) != jobs {
		t.Fatalf("去重后只剩 %d 个 profile", len(seen))
	}
}

// 超时：假 soffice 睡 5s、Options.Timeout=500ms →
// 断言 ①返回 ErrTimeout（不是等 5s 自然结束）②进程组被杀（kill(-pgid,0)=ESRCH）
// ③临时 profile 被清理。
func TestConvertTimeoutKillsProcessGroupAndCleansProfile(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	plog := filepath.Join(dir, "profiles.log")
	kit := kitWithScript(t, fakeConvertScript(log, plog, filepath.Join(dir, "n"), 5*time.Second, false, true))
	src := srcDoc(t, dir, "slow.docx")
	outDir := t.TempDir()
	profileRoot := t.TempDir()

	start := time.Now()
	_, err := kit.Convert(context.Background(), src, outDir, ConvertOptions{
		Timeout:     500 * time.Millisecond,
		ProfileRoot: profileRoot,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("期望 ErrTimeout，得到 %v", err)
	}
	// 500ms 的时限 + runExec 的有界收尾（4s + 2s 是它的最坏上界，正常远小于此）；
	// 若没杀进程组，这里会是脚本自己的 5s。
	if elapsed > 3*time.Second {
		t.Fatalf("超时后 %v 才返回：说明没有在 500ms 时限内杀掉，而是在等脚本自己结束", elapsed)
	}

	raw, rerr := os.ReadFile(plog)
	if rerr != nil {
		t.Fatal(rerr)
	}
	fields := strings.Fields(strings.TrimSpace(string(raw)))
	if len(fields) < 2 {
		t.Fatalf("plog 里没有 pid/profile: %q", raw)
	}
	var pid int
	if _, err := fmt.Sscanf(fields[0], "%d", &pid); err != nil || pid <= 0 {
		t.Fatalf("解析 pid 失败: %q", fields[0])
	}
	// 进程组被杀干净：kill(-pgid, 0) 返回 ESRCH（组里连僵尸都不剩）。
	// 轮询是必要的 —— 我们是杀的那一方，子进程退出/被回收与 Wait 返回之间有窗口。
	deadline := time.Now().Add(3 * time.Second)
	for {
		gerr := syscall.Kill(-pid, 0)
		if errors.Is(gerr, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("进程组 %d 仍存活（kill(-pgid,0) = %v）：超时没有杀进程组", pid, gerr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	profile := profileOf(t, fields[1])
	if _, serr := os.Stat(profile); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("profile %s 没被清理（stat err = %v）", profile, serr)
	}
}

// soffice 退出码 0 但没产出 PDF：必须失败（「0 退出 ≠ 有产物」，不能让上层拿到空路径）。
func TestConvertFailsWhenNoPDFProduced(t *testing.T) {
	dir := t.TempDir()
	kit := kitWithScript(t, fakeConvertScript(filepath.Join(dir, "a.log"), filepath.Join(dir, "p.log"), filepath.Join(dir, "n"), 0, false, false))
	src := srcDoc(t, dir, "empty.docx")
	_, err := kit.Convert(context.Background(), src, t.TempDir(), ConvertOptions{ProfileRoot: t.TempDir()})
	if !errors.Is(err, ErrConvertFailed) {
		t.Fatalf("期望 ErrConvertFailed，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "未产出 PDF") {
		t.Fatalf("错误串没说清原因: %v", err)
	}
}

// 预热：首次可用跑一次空转换；**失败不阻断**（假脚本第一次退出 1、第二次 0）。
func TestWarmFailureDoesNotDisableKit(t *testing.T) {
	dir := t.TempDir()
	plog := filepath.Join(dir, "profiles.log")
	// 走真探测（kit 目录 + KIT.json），确认「预热失败」不会把 State 变成 invalid。
	root := t.TempDir()
	kitDir := writeKit(t, root, PinnedVersion, CurrentPlatform(), nil)
	if err := os.WriteFile(filepath.Join(kitDir, "soffice"), []byte("#!/bin/sh\n"+fakeConvertScript(filepath.Join(dir, "a.log"), plog, filepath.Join(dir, "n"), 0, true, true)), 0o755); err != nil {
		t.Fatal(err)
	}
	// 二进制换了 → KIT.json 的 sha256 必须跟着换（否则校验不过是另一条断言的失败）。
	sum, err := fileSHA256(filepath.Join(kitDir, "soffice"))
	if err != nil {
		t.Fatal(err)
	}
	rewriteSHA(t, kitDir, sum)

	p := NewProber(noSystem(ProbeOptions{KitRoot: root}))
	if st := p.Status(); !st.Available {
		t.Fatalf("kit 应可用：%q %v", st.Reason, st.Errors)
	}
	if err := p.WarmOnce(context.Background()); err == nil {
		t.Fatal("预热本应失败（假脚本第一次退出 1）")
	}
	if st := p.Status(); !st.Available {
		t.Fatalf("预热失败把 kit 弄成不可用了：state=%q", st.State)
	}
	// 第二次调用（预热之后）应该正常 —— 预热失败没有留下坏状态（profile 已被清理）。
	src := srcDoc(t, dir, "after-warm.docx")
	res, err := p.Kit().Convert(context.Background(), src, t.TempDir(), ConvertOptions{ProfileRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("预热失败后的真实转换也失败了: %v", err)
	}
	if _, err := os.Stat(res.PDFPath); err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	// 预热只跑一次：再调 WarmOnce 不再起进程（sync.Once 的语义，也是「不把冷启
	// 成本反复摊在每次启动上」的要求）。
	before := countLines(t, plog)
	if err := p.WarmOnce(context.Background()); err != nil {
		t.Fatalf("已预热过，WarmOnce 应静默返回 nil，得到 %v", err)
	}
	if after := countLines(t, plog); after != before {
		t.Fatalf("WarmOnce 跑了第二次（plog %d → %d）", before, after)
	}
}

// rewriteSHA 改 KIT.json 里的 sha256（二进制被测试替换之后）。
func rewriteSHA(t *testing.T, kitDir, sum string) {
	t.Helper()
	m, err := readManifest(filepath.Join(kitDir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	m.SHA256 = sum
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kitDir, manifestName), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// countLines 数文件行数（不存在 = 0）。
func countLines(t *testing.T, p string) int {
	t.Helper()
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}
