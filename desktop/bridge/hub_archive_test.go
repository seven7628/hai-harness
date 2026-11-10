package main

// hub_archive_test.go：P4-04b-2b 冷归档后台触发（bridge 生命周期）。
//
// 关键断言：
//   - hubStore 首次打开/缓存命中都会启动归档循环，首轮立即跑，之后按短间隔 tick；
//   - 归档失败只发一条 hub_archive_failed（同一错误串去重），成功不发事件；
//   - 关停走 stopHubBackground，running 复位，不泄漏 goroutine；
//   - 低于阈值时热日志/activity/artifact 一律不动（不自动删除）。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

func waitHubArchiveRunning(t *testing.T, m *manager, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		m.hubArchiveMu.Lock()
		got := m.hubArchiveRunning
		m.hubArchiveMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hubArchiveRunning = %v, want %v (timeout %s)", got, want, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitHubArchiveManifest(t *testing.T, st *agenthub.FileStore, scope, id string, timeout time.Duration) *agenthub.ArchiveManifest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		m, err := st.LoadArchiveManifest(context.Background(), scope, id)
		if err == nil {
			return m
		}
		if !errors.Is(err, agenthub.ErrNotFound) {
			t.Fatalf("LoadArchiveManifest(%s,%s): %v", scope, id, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("archive manifest %s/%s not created within %s", scope, id, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hubHotLineCount(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strings.Count(string(data), "\n"), nil
}

func waitHubHotLines(t *testing.T, path string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, err := hubHotLineCount(path)
		if err != nil {
			t.Fatalf("read hot log: %v", err)
		}
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hot lines = %d, want %d (timeout %s)", got, want, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readHubFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertHubFileUnchanged(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file disappeared after archive: %s: %v", path, err)
	}
	if string(after) != string(before) {
		t.Fatalf("file changed after archive: %s", path)
	}
}

// TestHubArchiveTickerArchivesLargeGroup 用真实 FileStore + 直接写 20,001 行热日志，
// 避免 AppendMessage 20k 次 fsync 把测试拖慢。
func TestHubArchiveTickerArchivesLargeGroup(t *testing.T) {
	m, _ := testManager(t)
	ctx := context.Background()
	root := filepath.Join(appDataDir(), "hub")
	st, err := agenthub.OpenFileStore(root)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	gid := agenthub.GroupID("grp_archive_bg")
	if err := st.SaveGroup(ctx, &agenthub.Group{
		ID: gid, Name: "归档测试群", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveGroup: %v", err)
	}

	hotPath := filepath.Join(root, "groups", string(gid), "log.jsonl")
	f, err := os.OpenFile(hotPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open hot log: %v", err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	base := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 20001; i++ {
		msg := agenthub.Message{
			Seq: i, ID: agenthub.MessageID(i), GroupID: gid,
			Sender:     agenthub.Sender{Kind: agenthub.SenderHuman},
			Kind:       agenthub.KindText,
			Text:       fmt.Sprintf("bulk-%05d", i),
			Visibility: agenthub.VisibilityGroup,
			CreatedAt:  base.Add(time.Duration(i) * time.Millisecond),
		}
		raw, err := json.Marshal(&msg)
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		if _, err := bw.Write(raw); err != nil {
			t.Fatalf("write message %d: %v", i, err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			t.Fatalf("write newline %d: %v", i, err)
		}
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush hot log: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync hot log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close hot log: %v", err)
	}
	// 重开重建 seq（模拟进程重启后的真实存储）。
	if _, err := agenthub.OpenFileStore(root); err != nil {
		t.Fatalf("reopen after direct log write: %v", err)
	}

	m.hubArchiveInterval = 10 * time.Millisecond
	archiveStore, err := m.hubStore()
	if err != nil {
		t.Fatalf("hubStore: %v", err)
	}
	waitHubArchiveRunning(t, m, true, 2*time.Second)
	manifest := waitHubArchiveManifest(t, archiveStore, agenthub.ArchiveScopeGroups, string(gid), 10*time.Second)
	if len(manifest.Segments) != 1 {
		t.Fatalf("segments = %+v, want 1", manifest.Segments)
	}
	seg := manifest.Segments[0]
	if seg.StartSeq != 1 || seg.EndSeq != 14000 || seg.Count != 14000 || seg.File != "1-14000.jsonl" {
		t.Fatalf("segment = %+v, want 1-14000 count=14000", seg)
	}
	waitHubHotLines(t, hotPath, 6001, 10*time.Second)

	m.shutdownHubRuntimes()
	waitHubArchiveRunning(t, m, false, 2*time.Second)
}

func TestHubArchiveFailureAlertDeduped(t *testing.T) {
	m, buf := testManager(t)
	ctx := context.Background()
	root := filepath.Join(appDataDir(), "hub")
	st, err := agenthub.OpenFileStore(root)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	gid := agenthub.GroupID("grp_archive_fail")
	if err := st.SaveGroup(ctx, &agenthub.Group{
		ID: gid, Name: "失败测试群", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveGroup: %v", err)
	}
	var first agenthub.Message
	for i := 1; i <= 12; i++ {
		msg := &agenthub.Message{
			GroupID: gid, Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
			Kind: agenthub.KindText, Text: fmt.Sprintf("small-%02d", i),
			Visibility: agenthub.VisibilityGroup,
			CreatedAt:  time.Now().UTC().Add(time.Duration(i) * time.Second),
		}
		if _, err := st.AppendMessage(ctx, msg); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
		if i == 1 {
			first = *msg
		}
	}
	if _, err := st.ArchiveGroup(ctx, gid, agenthub.ArchivePolicy{
		MaxMessages: 5, KeepRecent: 3, Percent: 70,
	}); err != nil {
		t.Fatalf("seed ArchiveGroup: %v", err)
	}
	manifest, err := st.LoadArchiveManifest(ctx, agenthub.ArchiveScopeGroups, string(gid))
	if err != nil || len(manifest.Segments) != 1 {
		t.Fatalf("seed manifest = %+v, %v", manifest, err)
	}
	segPath := filepath.Join(root, "archive", "groups", string(gid), manifest.Segments[0].File)
	hotPath := filepath.Join(root, "groups", string(gid), "log.jsonl")

	// 让热日志覆盖已归档 seq（模拟 manifest 已提交、热未裁）：ArchiveGroup 的
	// prune 会真正读取段文件，否则只读热日志不会发现段内容损坏。
	f, err := os.OpenFile(hotPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open hot for duplicate: %v", err)
	}
	raw, err := json.Marshal(&first)
	if err != nil {
		t.Fatalf("marshal duplicate: %v", err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		t.Fatalf("append duplicate: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync duplicate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close duplicate: %v", err)
	}

	// 同尺寸改一个字节：OpenFileStore 的 size 校验仍应通过，读段时 hash 才报错。
	if err := os.Chmod(segPath, 0o600); err != nil {
		t.Fatalf("chmod segment: %v", err)
	}
	segData, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	segData[len(segData)/2] ^= 0x01
	if err := os.WriteFile(segPath, segData, 0o600); err != nil {
		t.Fatalf("corrupt segment: %v", err)
	}
	if _, err := agenthub.OpenFileStore(root); err != nil {
		t.Fatalf("OpenFileStore after same-size corruption (must pass size check): %v", err)
	}

	hotBefore := readHubFile(t, hotPath)
	segBefore := readHubFile(t, segPath)
	manifestPath := filepath.Join(root, "archive", "groups", string(gid), "manifest.json")
	manifestBefore := readHubFile(t, manifestPath)
	groupBefore := readHubFile(t, filepath.Join(root, "groups", string(gid), "group.json"))

	m.hubArchiveInterval = 10 * time.Millisecond
	if _, err := m.hubStore(); err != nil {
		t.Fatalf("hubStore: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var alerts []map[string]any
	for {
		alerts = hubEvents(t, buf, "hub_alert")
		if len(alerts) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub_archive_failed alert not emitted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ev := alerts[0]
	if ev["code"] != "hub_archive_failed" || ev["kind"] != "hub_archive_failed" {
		t.Fatalf("alert code = %v/%v, want hub_archive_failed", ev["code"], ev["kind"])
	}
	if ev["scope"] != "hub" || ev["severity"] != "error" {
		t.Fatalf("alert envelope = %+v, want hub/error", ev)
	}
	msg, _ := ev["message"].(string)
	if !strings.Contains(msg, "sha256") {
		t.Fatalf("alert message = %q, want archive error", msg)
	}
	if got := hubAlertSeverity("hub_archive_failed"); got != "error" {
		t.Fatalf("severity(hub_archive_failed) = %q, want error", got)
	}

	// 多个 tick 后仍只有一条：同一错误字符串必须去重。
	time.Sleep(200 * time.Millisecond)
	if got := len(hubEvents(t, buf, "hub_alert")); got != 1 {
		t.Fatalf("hub_alert count after multiple ticks = %d, want 1", got)
	}

	m.shutdownHubRuntimes()
	waitHubArchiveRunning(t, m, false, 2*time.Second)
	assertHubFileUnchanged(t, hotPath, hotBefore)
	assertHubFileUnchanged(t, segPath, segBefore)
	assertHubFileUnchanged(t, manifestPath, manifestBefore)
	assertHubFileUnchanged(t, filepath.Join(root, "groups", string(gid), "group.json"), groupBefore)
	if got := len(hubEvents(t, buf, "hub_alert")); got != 1 {
		t.Fatalf("hub_alert count after stop = %d, want 1", got)
	}
}

func TestHubArchiveLoopStopsOnShutdown(t *testing.T) {
	m, buf := testManager(t)
	m.hubArchiveInterval = 10 * time.Millisecond
	if _, err := m.hubStore(); err != nil {
		t.Fatalf("hubStore: %v", err)
	}
	waitHubArchiveRunning(t, m, true, 2*time.Second)

	m.shutdownHubRuntimes()
	waitHubArchiveRunning(t, m, false, 2*time.Second)
	if got := len(hubEvents(t, buf, "hub_alert")); got != 0 {
		t.Fatalf("empty store emitted %d alerts, want 0", got)
	}
}

func TestHubArchiveBelowThresholdLeavesDataUntouched(t *testing.T) {
	m, buf := testManager(t)
	ctx := context.Background()
	root := filepath.Join(appDataDir(), "hub")
	st, err := agenthub.OpenFileStore(root)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	gid := agenthub.GroupID("grp_archive_small")
	if err := st.SaveGroup(ctx, &agenthub.Group{
		ID: gid, Name: "小群", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveGroup: %v", err)
	}
	for i := 1; i <= 10; i++ {
		msg := &agenthub.Message{
			GroupID: gid, Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
			Kind: agenthub.KindText, Text: fmt.Sprintf("keep-%02d", i),
			Visibility: agenthub.VisibilityGroup,
			CreatedAt:  time.Now().UTC().Add(time.Duration(i) * time.Second),
		}
		if _, err := st.AppendMessage(ctx, msg); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}
	if err := st.AppendActivity(ctx, &agenthub.Activity{
		GroupID: gid, Kind: "status", Name: "keep", At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendActivity: %v", err)
	}
	artifactID := agenthub.ArtifactID("art_archive_keep")
	if err := st.SaveArtifact(ctx, &agenthub.Artifact{
		ID: artifactID, Name: "keep.txt", Path: "out/keep.txt", Kind: "created",
		Visibility: agenthub.VisibilityGroup, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}

	hotPath := filepath.Join(root, "groups", string(gid), "log.jsonl")
	activityPath := filepath.Join(root, "groups", string(gid), "activity.jsonl")
	artifactPath := filepath.Join(root, "artifacts", string(artifactID), "artifact.json")
	hotBefore := readHubFile(t, hotPath)
	activityBefore := readHubFile(t, activityPath)
	artifactBefore := readHubFile(t, artifactPath)

	m.hubArchiveInterval = 10 * time.Millisecond
	if _, err := m.hubStore(); err != nil {
		t.Fatalf("hubStore: %v", err)
	}
	waitHubArchiveRunning(t, m, true, 2*time.Second)
	time.Sleep(120 * time.Millisecond)

	m.shutdownHubRuntimes()
	waitHubArchiveRunning(t, m, false, 2*time.Second)
	if _, err := st.LoadArchiveManifest(ctx, agenthub.ArchiveScopeGroups, string(gid)); !errors.Is(err, agenthub.ErrNotFound) {
		t.Fatalf("below-threshold manifest = %v, want ErrNotFound", err)
	}
	assertHubFileUnchanged(t, hotPath, hotBefore)
	assertHubFileUnchanged(t, activityPath, activityBefore)
	assertHubFileUnchanged(t, artifactPath, artifactBefore)
	if _, err := os.Stat(filepath.Join(root, "archive")); !os.IsNotExist(err) {
		t.Fatalf("archive dir created below threshold: %v", err)
	}
	if got := len(hubEvents(t, buf, "hub_alert")); got != 0 {
		t.Fatalf("below-threshold run emitted %d alerts, want 0", got)
	}
}
