package main

// 内嵌编辑器读写通道（file_open / file_save / file_close）+ 外部变更推送的集成测试。
//
// 覆盖 docs/creator-mode-v0.1/02-pr-plan.md PR-05a 的验收条款：
//  1. open 的 content / hash / size 正确，越界路径拒绝；
//  2. save 成功后文件内容与 hash 同步更新；
//  3. baseHash 不匹配 → conflict，且**文件未被改**（冲突不是「先写后报」）；
//  4. 锁被别的写者持有 → conflict（不排队、不覆盖）；
//  5. 外部改动后 watcher 推 file_changed，自家 save 不推，close 之后不再推；
//  6. 打开集合有 64 条 LRU 上限，多标签引用（refs>=2）的路径不被淘汰。
//
// 等待一律用「轮询 + 上限」而不是固定 sleep：去抖是 300ms，机器慢时会更晚。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// editManager 与 testManager 同源，额外保证收尾时停掉文件监听（watcher 不泄漏）。
func editManager(t *testing.T) (*manager, *syncBuf) {
	t.Helper()
	m, buf := testManager(t)
	t.Cleanup(m.stopFileWatches)
	return m, buf
}

// editDispatch 派发一条命令并取回它的响应。用 readAllLines（不清空）：
// 同一批输出里还可能有 file_changed 事件，被 Take 清掉会让后面的轮询永远等不到。
func editDispatch(t *testing.T, m *manager, buf *syncBuf, id int, typ string, payload map[string]any) map[string]any {
	t.Helper()
	m.dispatch(cmd(id, typ, payload))
	for _, l := range readAllLines(buf) {
		if l["event_type"] == "command_response" && l["id"] == float64(id) {
			return l
		}
	}
	t.Fatalf("%s 无响应", typ)
	return nil
}

func editData(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	if resp["ok"] != true {
		t.Fatalf("命令失败: %v (code=%v)", resp["error"], resp["code"])
	}
	d, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 data: %+v", resp)
	}
	return d
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// waitFileChanged 轮询等 file_changed（上限 5s，命中即返回；不做固定 sleep）。
func waitFileChanged(t *testing.T, buf *syncBuf, wantPath string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range readAllLines(buf) {
			if l["event_type"] == "file_changed" && str(l, "path") == wantPath {
				return l
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等 file_changed(%s) 超时；已有输出: %s", wantPath, buf.String())
	return nil
}

// assertNoFileChanged 在给定的观察窗内断言不再出现该路径的 file_changed。
// 「不该发生」只能靠有界观察来证明：窗口取去抖的 3 倍，避免慢机器误判。
func assertNoFileChanged(t *testing.T, buf *syncBuf, wantPath string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		for _, l := range readAllLines(buf) {
			if l["event_type"] == "file_changed" && str(l, "path") == wantPath {
				t.Fatalf("不该出现 file_changed(%s): %+v", wantPath, l)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// open 的 content/hash/size 正确；越界路径统一拒绝（与 file_preview 同一条口径）。
func TestFileOpenContentHashAndPathGuard(t *testing.T) {
	ws := t.TempDir()
	body := "第一行\n世界 hello\n"
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)

	d := editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	if got := d["content"]; got != body {
		t.Fatalf("content = %q, want %q（行尾/换行必须原样往返）", got, body)
	}
	if got := d["hash"]; got != sha256Hex(body) {
		t.Fatalf("hash = %v, want %s", got, sha256Hex(body))
	}
	if got := d["size"]; got != float64(len(body)) {
		t.Fatalf("size = %v, want %d", got, len(body))
	}
	if got := d["mtime"]; got == float64(0) || got == nil {
		t.Fatalf("mtime 应非 0: %v", got)
	}
	if d["truncated"] != false || d["binary"] != false {
		t.Fatalf("普通文本不该被标 truncated/binary: %+v", d)
	}
	// 返回的 path 是解析后的绝对路径（file_changed 用它做匹配）
	if got := d["path"]; got != filepath.Join(ws, "a.txt") {
		t.Fatalf("path = %v, want 绝对路径", got)
	}

	// 相对路径逃逸工作区：拒绝（code=denied），且不登记打开集合
	resp := editDispatch(t, m, buf, 2, "file_open", map[string]any{"workspace": ws, "path": "../escape.txt"})
	if resp["ok"] != false || str(resp, "code") != "denied" {
		t.Fatalf("越界路径应 denied: %+v", resp)
	}
	// 受保护目录（~/.go-code）：走同一个解析器，同样拒绝（settings.json 可能含 api_key）
	if _, err := resolveWorkspacePath(ws, filepath.Join(os.Getenv("HOME"), ".go-code", "settings.json")); err == nil {
		t.Fatal("~/.go-code 下的路径应被拒绝（受保护目录）")
	}

	resp = editDispatch(t, m, buf, 3, "file_open", map[string]any{"workspace": ws, "path": "ghost.txt"})
	if resp["ok"] != false || str(resp, "code") != "not_found" {
		t.Fatalf("不存在的文件应 not_found: %+v", resp)
	}
}

// 超限截断与二进制：两种都只读（写回去 = 替用户截断/破坏文件）。
func TestFileOpenTruncatedAndBinaryAreReadOnly(t *testing.T) {
	ws := t.TempDir()
	big := strings.Repeat("a", maxEditBytes+10)
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := []byte{0x00, 0x01, 0x02, 'x', 0x00}
	if err := os.WriteFile(filepath.Join(ws, "b.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)

	d := editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "big.txt"}))
	if d["truncated"] != true {
		t.Fatalf("超限文件应标 truncated: %+v", d["truncated"])
	}
	if got := len(str(d, "content")); got != maxEditBytes {
		t.Fatalf("截断后 content 长度 = %d, want %d", got, maxEditBytes)
	}
	// hash 覆盖整份文件（保存时的冲突检测比的是磁盘真相，不是截断视图）
	if got := str(d, "hash"); got != sha256Hex(big) {
		t.Fatalf("hash 应是整份文件的 sha256")
	}
	resp := editDispatch(t, m, buf, 2, "file_save",
		map[string]any{"workspace": ws, "path": "big.txt", "content": "x", "baseHash": str(d, "hash")})
	if resp["ok"] != false || str(resp, "code") != "too_large" {
		t.Fatalf("截断打开的文件拒绝写回（too_large）: %+v", resp)
	}
	if raw, _ := os.ReadFile(filepath.Join(ws, "big.txt")); len(raw) != len(big) {
		t.Fatalf("被拒的写回不该改文件，长度 = %d, want %d", len(raw), len(big))
	}

	d = editData(t, editDispatch(t, m, buf, 3, "file_open", map[string]any{"workspace": ws, "path": "b.bin"}))
	if d["binary"] != true {
		t.Fatalf("NUL 文件应标 binary: %+v", d["binary"])
	}
	if str(d, "content") != "" {
		t.Fatalf("二进制不返回内容（前端只显示提示）: %q", str(d, "content"))
	}
	resp = editDispatch(t, m, buf, 4, "file_save",
		map[string]any{"workspace": ws, "path": "b.bin", "content": "y", "baseHash": str(d, "hash")})
	if resp["ok"] != false || str(resp, "code") != "denied" {
		t.Fatalf("二进制文件拒绝文本写回（denied）: %+v", resp)
	}
}

// save 成功 → 磁盘内容与响应里的 hash 同步更新，再次 open 读到新 hash。
func TestFileSaveWritesAndUpdatesHash(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)

	d := editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	base, _ := d["hash"].(string)

	next := "new 内容\n第二行\n"
	d = editData(t, editDispatch(t, m, buf, 2, "file_save",
		map[string]any{"workspace": ws, "path": "a.txt", "content": next, "baseHash": base}))
	if got := d["hash"]; got != sha256Hex(next) {
		t.Fatalf("save 返回 hash = %v, want %s", got, sha256Hex(next))
	}
	if got := d["mtime"]; got == nil || got == float64(0) {
		t.Fatalf("save 应返回非 0 mtime: %v", got)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != next {
		t.Fatalf("磁盘内容 = %q, want %q", raw, next)
	}

	// 再 open 一次：读到的 hash 必须与 save 返回的一致（否则前端下一次保存必误判冲突）
	d = editData(t, editDispatch(t, m, buf, 3, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	if got := d["hash"]; got != sha256Hex(next) {
		t.Fatalf("重开 hash = %v, want %s", got, sha256Hex(next))
	}
}

// baseHash 不匹配 → conflict，且磁盘文件**一个字节都没变**。
func TestFileSaveBaseHashConflictLeavesFileIntact(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)

	d := editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	stale, _ := d["hash"].(string)

	// 模拟 agent 抢先写了这个文件
	external := "agent 写的\n"
	if err := os.WriteFile(target, []byte(external), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := editDispatch(t, m, buf, 2, "file_save",
		map[string]any{"workspace": ws, "path": "a.txt", "content": "我的改动\n", "baseHash": stale})
	if resp["ok"] != false || str(resp, "code") != "conflict" {
		t.Fatalf("baseHash 不匹配应 conflict: %+v", resp)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != external {
		t.Fatalf("冲突时文件不该被改，磁盘 = %q, want %q", raw, external)
	}
	// 冲突响应带 currentHash：前端据此判断「磁盘上是哪一版」
	if got := str(resp["data"].(map[string]any), "currentHash"); got != sha256Hex(external) {
		t.Fatalf("currentHash = %q, want %s", got, sha256Hex(external))
	}
}

// 锁被别的写者持有（agent 正在 edit_file）→ conflict，不排队也不覆盖。
func TestFileSaveLockedReturnsConflict(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("locked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)

	d := editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	base, _ := d["hash"].(string)

	// 用另一个 fd 拿同一把 flock（atomicfile 的锁是 fd 上的 POSIX 建议锁）
	f, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("准备锁失败: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	resp := editDispatch(t, m, buf, 2, "file_save",
		map[string]any{"workspace": ws, "path": "a.txt", "content": "我的改动\n", "baseHash": base})
	if resp["ok"] != false || str(resp, "code") != "conflict" {
		t.Fatalf("锁被占用应 conflict: %+v", resp)
	}
	if msg := str(resp, "error"); msg == "" {
		t.Fatal("conflict 必须带可读 message")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "locked\n" {
		t.Fatalf("锁被占用时文件不该被改，磁盘 = %q", raw)
	}
}

// 未打开的文件默认拒绝覆盖（否则渲染层等于拿到一个无授权写接口）；
// allowNew 显式放行才允许新建/另存为。
func TestFileSaveUnopenedPathDenied(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("origin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)

	resp := editDispatch(t, m, buf, 1, "file_save",
		map[string]any{"workspace": ws, "path": "a.txt", "content": "覆盖\n", "baseHash": ""})
	if resp["ok"] != false || str(resp, "code") != "denied" {
		t.Fatalf("未打开的文件应 denied: %+v", resp)
	}
	if raw, _ := os.ReadFile(target); string(raw) != "origin\n" {
		t.Fatalf("被拒的保存不该落盘，磁盘 = %q", raw)
	}

	// allowNew + baseHash 为空 = 显式无条件新建
	d := editData(t, editDispatch(t, m, buf, 2, "file_save",
		map[string]any{"workspace": ws, "path": "b.txt", "content": "新建\n", "baseHash": "", "allowNew": true}))
	if got := d["hash"]; got != sha256Hex("新建\n") {
		t.Fatalf("新建后 hash = %v, want %s", got, sha256Hex("新建\n"))
	}
	if raw, _ := os.ReadFile(filepath.Join(ws, "b.txt")); string(raw) != "新建\n" {
		t.Fatalf("allowNew 应能建文件，磁盘 = %q", raw)
	}
}

// openSetSnapshot 在锁内读打开集合的态（测试断言用；watcher goroutine 也会写这些字段）。
func openSetSnapshot(e *fileEditor) (count int, dirs map[string]int, refs map[string]int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dirs = make(map[string]int, len(e.dirs))
	for d, c := range e.dirs {
		dirs[d] = c
	}
	refs = make(map[string]int, len(e.files))
	for p, st := range e.files {
		refs[p] = st.refs
	}
	return len(e.files), dirs, refs
}

// 打开 70 个路径（各 1 个持有者）：集合压回 64，最早的被淘汰，目录引用计数同步配平。
func TestFileWatchOpenSetLRUCapEvictsOldest(t *testing.T) {
	ws := t.TempDir()
	m, buf := editManager(t)

	const n = 70
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		if err := os.WriteFile(filepath.Join(ws, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		editData(t, editDispatch(t, m, buf, i+1, "file_open", map[string]any{"workspace": ws, "path": name}))
	}

	count, dirs, refs := openSetSnapshot(m.fileEditor())
	if count != maxOpenFiles {
		t.Fatalf("打开集合 = %d, want %d（超限应淘汰最久未用）", count, maxOpenFiles)
	}
	if _, ok := refs[filepath.Join(ws, "f00.txt")]; ok {
		t.Fatal("最早的 f00.txt 应被淘汰")
	}
	if _, ok := refs[filepath.Join(ws, fmt.Sprintf("f%02d.txt", n-1))]; !ok {
		t.Fatalf("最新的 f%02d.txt 必须还在", n-1)
	}
	// 目录引用计数必须与集合条目数配平（重复挂 watch 会泄漏，直到进程退出）
	if got := dirs[ws]; got != maxOpenFiles {
		t.Fatalf("目录引用计数 = %d, want %d（每条目只算一次）", got, maxOpenFiles)
	}

	// 淘汰后：写回被拒（未授权路径），close 仍幂等（前端清理路径不该报错）
	resp := editDispatch(t, m, buf, 99, "file_save",
		map[string]any{"workspace": ws, "path": "f00.txt", "content": "y\n", "baseHash": ""})
	if resp["ok"] != false || str(resp, "code") != "denied" {
		t.Fatalf("被淘汰的路径应拒绝写入: %+v", resp)
	}
	if resp := editDispatch(t, m, buf, 100, "file_close", map[string]any{"workspace": ws, "path": "f00.txt"}); resp["ok"] != true {
		t.Fatalf("file_close 必须幂等: %+v", resp)
	}
}

// 被两个标签引用（refs=2）的路径，即使是最早打开的也不许淘汰；超限去淘汰别人。
func TestFileWatchOpenSetLRUKeepsMultiReferenced(t *testing.T) {
	ws := t.TempDir()
	m, buf := editManager(t)
	shared := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(shared, []byte("shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 两个标签打开同一个文件 → refs=2（这是最早进表的条目）
	editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	editData(t, editDispatch(t, m, buf, 2, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))

	const n = 70
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		if err := os.WriteFile(filepath.Join(ws, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		editData(t, editDispatch(t, m, buf, i+3, "file_open", map[string]any{"workspace": ws, "path": name}))
	}

	count, dirs, refs := openSetSnapshot(m.fileEditor())
	if count != maxOpenFiles {
		t.Fatalf("打开集合 = %d, want %d", count, maxOpenFiles)
	}
	// 同一路径 open 两次只该占**一个**目录引用（否则 Remove 永远配不平 → watch 泄漏）
	if got := dirs[ws]; got != count {
		t.Fatalf("目录引用计数 = %d, want %d（重复 open 不该重复挂目录）", got, count)
	}
	if got := refs[shared]; got != 2 {
		t.Fatalf("两标签引用的路径应保留且 refs=2，得到 refs=%d（0=被误淘汰）", got)
	}
	if _, ok := refs[filepath.Join(ws, "f00.txt")]; ok {
		t.Fatal("单引用的最旧条目才该被淘汰（f00.txt 应已淘汰）")
	}
}

// 外部改动 → watcher 推 file_changed（300ms 去抖后）。
func TestFileChangePushedOnExternalWrite(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)
	editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	buf.Reset() // 只观察 open 之后的事件

	external := "v2 外部写入\n"
	if err := os.WriteFile(target, []byte(external), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := waitFileChanged(t, buf, target)
	if got := str(ev, "hash"); got != sha256Hex(external) {
		t.Fatalf("file_changed.hash = %q, want %s", got, sha256Hex(external))
	}
	if got := str(ev, "by"); got != "external" {
		t.Fatalf("file_changed.by = %q, want external", got)
	}
}

// 自家 save 触发的写不回调前端（2s 静默窗内同 hash 丢弃）。
func TestFileChangeSuppressedForOwnSave(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)
	d := editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	base, _ := d["hash"].(string)

	editData(t, editDispatch(t, m, buf, 2, "file_save",
		map[string]any{"workspace": ws, "path": "a.txt", "content": "v2 我写的\n", "baseHash": base}))
	buf.Reset()

	// 去抖 300ms → 观察窗取 3 倍，给慢机器余量
	assertNoFileChanged(t, buf, target, 3*fileChangeDebounce)
}

// file_close 退订之后不再推（关标签即停止跟踪，不是等进程退出）。
func TestFileChangeStopsAfterClose(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(target, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := editManager(t)
	editData(t, editDispatch(t, m, buf, 1, "file_open", map[string]any{"workspace": ws, "path": "a.txt"}))
	editData(t, editDispatch(t, m, buf, 2, "file_close", map[string]any{"workspace": ws, "path": "a.txt"}))
	buf.Reset()

	if err := os.WriteFile(target, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertNoFileChanged(t, buf, target, 3*fileChangeDebounce)
}
