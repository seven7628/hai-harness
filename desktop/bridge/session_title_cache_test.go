package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestListSessionsDiskTitleCache 磁盘会话标题缓存：
//
//	· 同一文件（mtime+size 未变）第二次 list 不再重读 256KB 尾部；
//	· 文件内容变化但 mtime/size 被恢复 → 仍命中缓存（证明不是每次都重读）；
//	· mtime 变化 → 重新读取，标题更新。
//
// 背景：199 个会话里 117 个没有 titles.json 缓存，旧实现每次 list 都读
// 117×256KB 尾部；冷页缓存 + checkpoint 写盘时实测 5.3s，是 new_session
// 响应 6–10s 的主项（用户报障「新建会话卡住约 10s」）。
func TestListSessionsDiskTitleCache(t *testing.T) {
	ws := t.TempDir()
	m, _ := testManager(t)
	sid := "s-title-cache"
	dir := wsSessionsDir(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")

	write := func(title string) {
		t.Helper()
		body := fmt.Sprintf(
			"{\"type\":\"history\",\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"content\":\"首条用户文本\"}]}]}\n"+
				"{\"type\":\"meta\",\"meta\":{\"title\":%q}}\n", title)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	titleOf := func() string {
		t.Helper()
		for _, s := range m.listSessions(ws) {
			if str(s, "id") == sid {
				return str(s, "title")
			}
		}
		t.Fatalf("会话 %s 不在 listSessions 结果里", sid)
		return ""
	}

	base := time.Unix(1_700_000_000, 0)
	write("标题A")
	if err := os.Chtimes(path, base, base); err != nil {
		t.Fatal(err)
	}
	if got := titleOf(); got != "标题A" {
		t.Fatalf("首次 list 标题=%q want 标题A", got)
	}

	// 同长度改内容 + 恢复 mtime/size：应命中缓存，仍返回旧标题。
	write("标题B")
	if err := os.Chtimes(path, base, base); err != nil {
		t.Fatal(err)
	}
	if got := titleOf(); got != "标题A" {
		t.Fatalf("mtime/size 未变时应命中缓存（got %q want 标题A）", got)
	}

	// 改 mtime → 缓存失效，重新读文件。
	newMtime := base.Add(2 * time.Second)
	if err := os.Chtimes(path, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}
	if got := titleOf(); got != "标题B" {
		t.Fatalf("mtime 变化后应重读（got %q want 标题B）", got)
	}
}
