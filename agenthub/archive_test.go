package agenthub

// archive_test.go：P4-04b-2a 冷归档存储层契约。
//
// 覆盖：阈值/保留下限、热+冷透明读、agent-safe/人类搜索口径、跨归档幂等、
// DM 未读、重启 seq、崩溃窗口（孤儿段 / manifest 已提交热未裁 / 段损坏）、
// 并发 append 与归档、以及「归档不删除其它数据」。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func archiveGroupMsg(text string, kind MessageKind, vis Visibility, at time.Time) Message {
	if kind == "" {
		kind = KindText
	}
	if vis == "" {
		vis = VisibilityGroup
	}
	return Message{
		GroupID:    "grp_test",
		Sender:     Sender{Kind: SenderHuman},
		Kind:       kind,
		Text:       text,
		Visibility: vis,
		CreatedAt:  at,
	}
}

func appendArchiveGroupMsg(t *testing.T, st *FileStore, m Message) Message {
	t.Helper()
	if m.GroupID == "" {
		m.GroupID = "grp_test"
	}
	if m.Visibility == "" {
		m.Visibility = VisibilityGroup
	}
	if m.Kind == "" {
		m.Kind = KindText
	}
	if m.Sender.Kind == "" {
		m.Sender = Sender{Kind: SenderHuman}
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = testTime()
	}
	if _, err := st.AppendMessage(context.Background(), &m); err != nil {
		t.Fatalf("AppendMessage(%q): %v", m.Text, err)
	}
	return m
}

func appendArchiveGroupN(t *testing.T, st *FileStore, n int, prefix string) []Message {
	t.Helper()
	out := make([]Message, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, appendArchiveGroupMsg(t, st, archiveGroupMsg(
			fmt.Sprintf("%s-%02d", prefix, i), KindText, VisibilityGroup,
			testTime().Add(time.Duration(i)*time.Second),
		)))
	}
	return out
}

func appendArchiveDMN(t *testing.T, st *FileStore, agentID AgentID, n int, prefix string) []Message {
	t.Helper()
	out := make([]Message, 0, n)
	for i := 1; i <= n; i++ {
		m := Message{
			Sender: Sender{Kind: SenderHuman}, Kind: KindText,
			Text: fmt.Sprintf("%s-%02d", prefix, i), Visibility: VisibilityGroup,
			CreatedAt: testTime().Add(time.Duration(i) * time.Second),
		}
		if _, err := st.AppendDirectMessage(context.Background(), agentID, &m); err != nil {
			t.Fatalf("AppendDirectMessage(%q): %v", m.Text, err)
		}
		out = append(out, m)
	}
	return out
}

func archiveSeqList(msgs []Message) []int64 {
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = m.Seq
	}
	return out
}

func archiveTextList(msgs []Message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = m.Text
	}
	return strings.Join(parts, "|")
}

func assertArchiveSeqs(t *testing.T, msgs []Message, want ...int64) {
	t.Helper()
	got := archiveSeqList(msgs)
	if len(got) != len(want) {
		t.Fatalf("seqs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seqs = %v, want %v", got, want)
		}
	}
}

func mustArchiveManifest(t *testing.T, st *FileStore, scope, id string) *ArchiveManifest {
	t.Helper()
	m, err := st.LoadArchiveManifest(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("LoadArchiveManifest(%s,%s): %v", scope, id, err)
	}
	return m
}

func archiveHotMessages(t *testing.T, st *FileStore, scope, id string) []Message {
	t.Helper()
	msgs, err := readJSONLTolerantTail[Message](st.archiveHotLogPath(scope, id))
	if err != nil {
		t.Fatalf("read hot %s/%s: %v", scope, id, err)
	}
	return msgs
}

func archiveSegmentMessages(t *testing.T, st *FileStore, scope, id string, seg ArchiveSegment) []Message {
	t.Helper()
	msgs, err := st.readArchiveSegment(scope, id, seg)
	if err != nil {
		t.Fatalf("read segment %s: %v", seg.File, err)
	}
	return msgs
}

// forceArchiveAll 把热日志剩余消息全部封成一段并清空热日志：模拟「人工全量归档」
// 或历史迁移后的状态，用于验证读取/搜索/未读在热日志为空时仍正确。
func forceArchiveAll(t *testing.T, st *FileStore, scope, id string) *ArchiveSegment {
	t.Helper()
	hotPath := st.archiveHotLogPath(scope, id)
	hot, err := readJSONLTolerantTail[Message](hotPath)
	if err != nil {
		t.Fatalf("read hot: %v", err)
	}
	if len(hot) == 0 {
		return nil
	}
	data, err := encodeMessages(hot)
	if err != nil {
		t.Fatalf("encode hot: %v", err)
	}
	seg := ArchiveSegment{
		StartSeq: hot[0].Seq, EndSeq: hot[len(hot)-1].Seq, Count: len(hot),
		Bytes: int64(len(data)), SHA256: sha256Hex(data),
		File:       archiveSegmentFile(hot[0].Seq, hot[len(hot)-1].Seq),
		ArchivedAt: time.Now().UTC(),
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	manifest, err := st.readArchiveManifest(scope, id)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if _, err := st.writeArchiveSegment(scope, id, seg, data); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	manifest.Segments = append(manifest.Segments, seg)
	sort.Slice(manifest.Segments, func(i, j int) bool {
		return manifest.Segments[i].StartSeq < manifest.Segments[j].StartSeq
	})
	manifest.UpdatedAt = time.Now().UTC()
	if err := validateArchiveManifest(manifest, scope, id); err != nil {
		t.Fatalf("force manifest invalid: %v", err)
	}
	if err := writeJSONAtomic(st.archiveManifestPath(scope, id), manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := rewriteHotLog(hotPath, nil); err != nil {
		t.Fatalf("clear hot: %v", err)
	}
	st.advanceSeqLocked(scope, id, manifest, nil)
	return &seg
}

func TestDefaultArchivePolicy(t *testing.T) {
	p := DefaultArchivePolicy()
	if p.MaxMessages != 20000 || p.MaxBytes != 50<<20 || p.KeepRecent != 2000 || p.Percent != 70 {
		t.Fatalf("default policy = %+v", p)
	}
}

func TestArchiveRejectsNonIncreasingBatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	hotPath := st.archiveHotLogPath(ArchiveScopeGroups, "grp_test")
	// 重复 seq 5 + 空洞 3-4：归档批次的 seq 不可能严格递增。
	for i, seq := range []int64{1, 2, 5, 5, 6, 7, 8, 9, 10, 11} {
		m := archiveGroupMsg(fmt.Sprintf("bad-%d", i), KindText, VisibilityGroup,
			testTime().Add(time.Duration(i)*time.Second))
		m.Seq = seq
		m.ID = MessageID(seq)
		if err := appendJSONL(hotPath, &m); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(hotPath)
	if err != nil {
		t.Fatal(err)
	}
	pol := ArchivePolicy{MaxMessages: 5, KeepRecent: 2, Percent: 100}

	if _, err := st.ArchiveGroup(ctx, "grp_test", pol); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ArchiveGroup duplicate-seq batch = %v, want ErrCorrupt", err)
	}
	if _, err := st.LoadArchiveManifest(ctx, ArchiveScopeGroups, "grp_test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manifest after rejected batch = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(st.archiveDir(ArchiveScopeGroups, "grp_test")); !os.IsNotExist(err) {
		t.Fatalf("archive dir created for rejected batch: %v", err)
	}
	after, err := os.ReadFile(hotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected archive must leave hot log byte-identical")
	}

	st2, err := OpenFileStore(st.Root())
	if err != nil {
		t.Fatalf("OpenFileStore after rejected batch: %v", err)
	}
	msgs, err := st2.ReadMessages(ctx, "grp_test", 0, 50)
	if err != nil {
		t.Fatalf("ReadMessages after rejected batch: %v", err)
	}
	assertArchiveSeqs(t, msgs, 1, 2, 5, 6, 7, 8, 9, 10, 11)

	// 第二次重试仍应干净失败，不能因为上次失败留下半状态而卡死。
	if _, err := st2.ArchiveGroup(ctx, "grp_test", pol); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("retry duplicate-seq batch = %v, want ErrCorrupt", err)
	}
	if _, err := st2.LoadArchiveManifest(ctx, ArchiveScopeGroups, "grp_test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manifest after retry = %v, want ErrNotFound", err)
	}
	afterRetry, err := os.ReadFile(hotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(afterRetry) {
		t.Fatal("retry must leave hot log byte-identical")
	}
}

func TestArchiveOrphanSegmentConflictLeavesHotUntouched(t *testing.T) {
	st := newTestStore(t)
	appendArchiveGroupN(t, st, 12, "oc")
	dir := st.archiveDir(ArchiveScopeGroups, "grp_test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	conflict := []byte("{\"seq\":1,\"text\":\"conflict\"}\n")
	orphan := filepath.Join(dir, "1-8.jsonl")
	if err := os.WriteFile(orphan, conflict, 0o600); err != nil {
		t.Fatal(err)
	}
	hotPath := st.archiveHotLogPath(ArchiveScopeGroups, "grp_test")
	before, err := os.ReadFile(hotPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = st.ArchiveGroup(context.Background(), "grp_test", ArchivePolicy{
		MaxMessages: 10, KeepRecent: 4, Percent: 100,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("ArchiveGroup conflicting orphan = %v, want ErrConflict", err)
	}
	after, err := os.ReadFile(hotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("conflicting orphan must leave hot log untouched")
	}
	if _, err := st.LoadArchiveManifest(context.Background(), ArchiveScopeGroups, "grp_test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manifest after conflict = %v, want ErrNotFound", err)
	}
	got, err := os.ReadFile(orphan)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(conflict) {
		t.Fatal("conflicting orphan must not be overwritten")
	}
}

func TestArchiveIfNeededContinuesAfterCorruptConversation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	bad := testGroup()
	bad.ID = "grp_bad"
	dup := testGroup()
	dup.ID = "grp_dup"
	good := testGroup()
	good.ID = "grp_good"
	if err := st.SaveGroup(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroup(ctx, dup); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroup(ctx, good); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgent(ctx, testAgent()); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 12; i++ {
		for _, gid := range []GroupID{"grp_bad", "grp_good"} {
			m := archiveGroupMsg(fmt.Sprintf("%s-%02d", gid, i), KindText, VisibilityGroup,
				testTime().Add(time.Duration(i)*time.Second))
			m.GroupID = gid
			appendArchiveGroupMsg(t, st, m)
		}
	}
	appendArchiveDMN(t, st, "ag_test", 12, "dm")

	// grp_dup：重复 seq 会让归档批次不可写，必须只影响自己。
	dupHotPath := st.archiveHotLogPath(ArchiveScopeGroups, "grp_dup")
	for i, seq := range []int64{1, 2, 5, 5, 6, 7, 8, 9, 10, 11} {
		m := archiveGroupMsg(fmt.Sprintf("dup-%d", i), KindText, VisibilityGroup,
			testTime().Add(time.Duration(i)*time.Second))
		m.GroupID = "grp_dup"
		m.Seq = seq
		m.ID = MessageID(seq)
		if err := appendJSONL(dupHotPath, &m); err != nil {
			t.Fatal(err)
		}
	}
	dupBefore, err := os.ReadFile(dupHotPath)
	if err != nil {
		t.Fatal(err)
	}

	// 坏会话的 manifest 读不出来：它必须只污染自己的结果，不能阻断其它会话。
	badDir := st.archiveDir(ArchiveScopeGroups, "grp_bad")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "manifest.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	badHotPath := st.archiveHotLogPath(ArchiveScopeGroups, "grp_bad")
	badBefore, err := os.ReadFile(badHotPath)
	if err != nil {
		t.Fatal(err)
	}

	results, err := st.archiveIfNeeded(ctx, ArchivePolicy{MaxMessages: 5, KeepRecent: 3, Percent: 70})
	if err == nil || !errors.Is(err, ErrCorrupt) ||
		!strings.Contains(err.Error(), "grp_bad") || !strings.Contains(err.Error(), "grp_dup") {
		t.Fatalf("archiveIfNeeded error = %v, want joined ErrCorrupt for grp_bad+grp_dup", err)
	}
	seen := map[string]int{}
	for _, r := range results {
		seen[r.Scope+"/"+r.ID] = r.Archived
	}
	if seen[ArchiveScopeGroups+"/grp_good"] != 8 || seen[ArchiveScopeDMs+"/ag_test"] != 8 {
		t.Fatalf("healthy conversations results = %v, want group+dm archived=8", seen)
	}
	if _, ok := seen[ArchiveScopeGroups+"/grp_bad"]; ok {
		t.Fatalf("corrupt group unexpectedly in results: %v", seen)
	}
	if _, ok := seen[ArchiveScopeGroups+"/grp_dup"]; ok {
		t.Fatalf("duplicate-seq group unexpectedly in results: %v", seen)
	}
	badAfter, err := os.ReadFile(badHotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(badBefore) != string(badAfter) {
		t.Fatal("corrupt conversation hot log changed")
	}
	dupAfter, err := os.ReadFile(dupHotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(dupBefore) != string(dupAfter) {
		t.Fatal("duplicate-seq conversation hot log changed")
	}
	if _, err := st.LoadArchiveManifest(ctx, ArchiveScopeGroups, "grp_dup"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate-seq group manifest = %v, want ErrNotFound", err)
	}
	if _, err := st.LoadArchiveManifest(ctx, ArchiveScopeGroups, "grp_good"); err != nil {
		t.Fatalf("healthy group not archived: %v", err)
	}
}

func TestArchiveDirectMessageClientIDWhitespace(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	first := Message{
		Sender: Sender{Kind: SenderHuman}, Kind: KindText, Text: "dm padded",
		Visibility: VisibilityGroup, Refs: map[string]any{"client_msg_id": " cid-dm-pad "},
		CreatedAt: testTime(),
	}
	if _, err := st.AppendDirectMessage(ctx, "ag_pad", &first); err != nil {
		t.Fatal(err)
	}
	appendArchiveDMN(t, st, "ag_pad", 5, "pad")
	for _, q := range []string{"cid-dm-pad", " cid-dm-pad ", "\tcid-dm-pad\n"} {
		if seq, ok, err := st.FindDirectMessageByClientID(ctx, "ag_pad", q); err != nil || !ok || seq != 1 {
			t.Fatalf("FindDirectMessageByClientID(%q) = %d/%v/%v, want 1/true", q, seq, ok, err)
		}
	}
	if _, ok, err := st.FindDirectMessageByClientID(ctx, "ag_pad", "   "); err != nil || ok {
		t.Fatalf("blank client id = %v/%v, want false", ok, err)
	}

	if _, err := st.ArchiveDM(ctx, "ag_pad", ArchivePolicy{MaxMessages: 3, KeepRecent: 2, Percent: 100}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"cid-dm-pad", " cid-dm-pad "} {
		if seq, ok, err := st.FindDirectMessageByClientID(ctx, "ag_pad", q); err != nil || !ok || seq != 1 {
			t.Fatalf("FindDirectMessageByClientID cold(%q) = %d/%v/%v, want 1/true", q, seq, ok, err)
		}
	}

	hotBefore, err := os.ReadFile(st.archiveHotLogPath(ArchiveScopeDMs, "ag_pad"))
	if err != nil {
		t.Fatal(err)
	}
	// bridge 的幂等重发路径：找到就不再 AppendDirectMessage。
	if _, ok, err := st.FindDirectMessageByClientID(ctx, "ag_pad", "cid-dm-pad"); err != nil || !ok {
		t.Fatalf("retry lookup = %v/%v", ok, err)
	}
	hotAfter, err := os.ReadFile(st.archiveHotLogPath(ArchiveScopeDMs, "ag_pad"))
	if err != nil {
		t.Fatal(err)
	}
	if string(hotBefore) != string(hotAfter) {
		t.Fatal("deduped DM retry must not append")
	}
}

func TestArchiveHotPrioritySearchFindUnread(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	groupFirst := archiveGroupMsg("needle cold group", KindText, VisibilityGroup, testTime())
	groupFirst.Refs = map[string]any{"client_msg_id": "cid-hot-group"}
	appendArchiveGroupMsg(t, st, groupFirst)
	for i := 2; i <= 12; i++ {
		appendArchiveGroupMsg(t, st, archiveGroupMsg(fmt.Sprintf("g-%02d", i), KindText, VisibilityGroup,
			testTime().Add(time.Duration(i)*time.Second)))
	}
	if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{MaxMessages: 10, KeepRecent: 4, Percent: 100}); err != nil {
		t.Fatal(err)
	}
	groupManifest := mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test")
	coldGroup := archiveSegmentMessages(t, st, ArchiveScopeGroups, "grp_test", groupManifest.Segments[0])
	groupHotPath := st.archiveHotLogPath(ArchiveScopeGroups, "grp_test")
	for _, msg := range coldGroup {
		dup := msg
		if dup.Seq == 1 {
			dup.Text = "needle hot group"
		} else {
			dup.Text = fmt.Sprintf("dup-%d", dup.Seq)
		}
		if err := appendJSONL(groupHotPath, &dup); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := st.SearchMessages(ctx, "grp_test", "needle", 0, 50)
	if err != nil || len(hits) != 1 || hits[0].Seq != 1 || hits[0].Text != "needle hot group" {
		t.Fatalf("agent search hot priority = %+v/%v", hits, err)
	}
	hits, err = st.SearchMessagesHuman(ctx, "grp_test", "needle", 0, 50)
	if err != nil || len(hits) != 1 || hits[0].Text != "needle hot group" {
		t.Fatalf("human search hot priority = %+v/%v", hits, err)
	}
	if got, err := st.FindGroupMessageByClientID(ctx, "grp_test", "cid-hot-group"); err != nil || got == nil || got.Text != "needle hot group" {
		t.Fatalf("group find hot priority = %+v/%v", got, err)
	}

	dmFirst := Message{Sender: Sender{Kind: SenderHuman}, Kind: KindText, Text: "needle cold dm",
		Visibility: VisibilityGroup, Refs: map[string]any{"client_msg_id": "cid-hot-dm"}, CreatedAt: testTime()}
	if _, err := st.AppendDirectMessage(ctx, "ag_hot", &dmFirst); err != nil {
		t.Fatal(err)
	}
	appendArchiveDMN(t, st, "ag_hot", 7, "dm")
	if _, err := st.ArchiveDM(ctx, "ag_hot", ArchivePolicy{MaxMessages: 3, KeepRecent: 2, Percent: 75}); err != nil {
		t.Fatal(err)
	}
	dmManifest := mustArchiveManifest(t, st, ArchiveScopeDMs, "ag_hot")
	coldDM := archiveSegmentMessages(t, st, ArchiveScopeDMs, "ag_hot", dmManifest.Segments[0])
	dmHotPath := st.archiveHotLogPath(ArchiveScopeDMs, "ag_hot")
	for _, msg := range coldDM {
		dup := msg
		if dup.Seq == 1 {
			dup.Text = "needle hot dm"
		} else {
			dup.Text = fmt.Sprintf("dm-dup-%d", dup.Seq)
		}
		if err := appendJSONL(dmHotPath, &dup); err != nil {
			t.Fatal(err)
		}
	}
	dmHits, err := st.SearchDirectMessages(ctx, "ag_hot", "needle", 0, 50)
	if err != nil || len(dmHits) != 1 || dmHits[0].Seq != 1 || dmHits[0].Text != "needle hot dm" {
		t.Fatalf("dm search hot priority = %+v/%v", dmHits, err)
	}
	if seq, ok, err := st.FindDirectMessageByClientID(ctx, "ag_hot", "cid-hot-dm"); err != nil || !ok || seq != 1 {
		t.Fatalf("dm find hot priority = %d/%v/%v", seq, ok, err)
	}
	unread, last, err := st.DMUnread(ctx, "ag_hot")
	if err != nil || unread != 8 || last == nil || last.Seq != 8 {
		t.Fatalf("dm unread hot dedupe = %d/%+v/%v, want 8/seq8", unread, last, err)
	}
}

func TestArchiveFullyColdMultiSegmentPagination(t *testing.T) {
	ctx := context.Background()
	pol := ArchivePolicy{MaxMessages: 10, KeepRecent: 5, Percent: 50}

	t.Run("group", func(t *testing.T) {
		st := newTestStore(t)
		appendArchiveGroupN(t, st, 30, "fg")
		for i := 0; i < 2; i++ {
			if _, err := st.ArchiveGroup(ctx, "grp_test", pol); err != nil {
				t.Fatal(err)
			}
		}
		forceArchiveAll(t, st, ArchiveScopeGroups, "grp_test")
		if hot := archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test"); len(hot) != 0 {
			t.Fatalf("hot = %d, want 0", len(hot))
		}
		if m := mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test"); len(m.Segments) != 3 {
			t.Fatalf("segments = %d, want 3", len(m.Segments))
		}
		for _, tc := range []struct {
			before int64
			limit  int
			want   []int64
		}{
			{0, 7, []int64{24, 25, 26, 27, 28, 29, 30}},
			{17, 10, []int64{7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
			{23, 8, []int64{15, 16, 17, 18, 19, 20, 21, 22}},
			{9, 5, []int64{4, 5, 6, 7, 8}},
		} {
			page, err := st.ReadMessages(ctx, "grp_test", tc.before, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			assertArchiveSeqs(t, page, tc.want...)
		}
	})

	t.Run("dm", func(t *testing.T) {
		st := newTestStore(t)
		appendArchiveDMN(t, st, "ag_fg", 30, "fd")
		for i := 0; i < 2; i++ {
			if _, err := st.ArchiveDM(ctx, "ag_fg", pol); err != nil {
				t.Fatal(err)
			}
		}
		forceArchiveAll(t, st, ArchiveScopeDMs, "ag_fg")
		if hot := archiveHotMessages(t, st, ArchiveScopeDMs, "ag_fg"); len(hot) != 0 {
			t.Fatalf("hot = %d, want 0", len(hot))
		}
		if m := mustArchiveManifest(t, st, ArchiveScopeDMs, "ag_fg"); len(m.Segments) != 3 {
			t.Fatalf("segments = %d, want 3", len(m.Segments))
		}
		for _, tc := range []struct {
			before int64
			limit  int
			want   []int64
		}{
			{0, 7, []int64{24, 25, 26, 27, 28, 29, 30}},
			{17, 10, []int64{7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
			{23, 8, []int64{15, 16, 17, 18, 19, 20, 21, 22}},
			{9, 5, []int64{4, 5, 6, 7, 8}},
		} {
			page, err := st.ReadDirectMessages(ctx, "ag_fg", tc.before, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			assertArchiveSeqs(t, page, tc.want...)
		}
	})
}

func TestArchiveGroupThresholdAndManifest(t *testing.T) {
	st := newTestStore(t)
	appendArchiveGroupN(t, st, 12, "m")
	ctx := context.Background()

	res, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{
		MaxMessages: 10, KeepRecent: 3, Percent: 70,
	})
	if err != nil {
		t.Fatalf("ArchiveGroup: %v", err)
	}
	if res.Skipped || res.Archived != 8 || res.HotRemaining != 4 {
		t.Fatalf("result = %+v, want archived=8 hot=4", res)
	}
	if res.Segment == nil || res.Segment.StartSeq != 1 || res.Segment.EndSeq != 8 {
		t.Fatalf("segment = %+v", res.Segment)
	}

	hot := archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test")
	assertArchiveSeqs(t, hot, 9, 10, 11, 12)

	m := mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test")
	if m.SchemaVersion != ArchiveSchemaVersion || m.Scope != ArchiveScopeGroups || m.ID != "grp_test" {
		t.Fatalf("manifest header = %+v", m)
	}
	if len(m.Segments) != 1 {
		t.Fatalf("manifest segments = %+v", m.Segments)
	}
	seg := m.Segments[0]
	if seg.StartSeq != 1 || seg.EndSeq != 8 || seg.Count != 8 ||
		seg.File != "1-8.jsonl" || seg.ArchivedAt.IsZero() {
		t.Fatalf("manifest segment = %+v", seg)
	}
	segPath := filepath.Join(st.archiveDir(ArchiveScopeGroups, "grp_test"), seg.File)
	data, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatalf("read segment file: %v", err)
	}
	sum := sha256.Sum256(data)
	if seg.Bytes != int64(len(data)) || seg.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("segment bytes/sha = %d/%s, actual %d/%s", seg.Bytes, seg.SHA256, len(data), hex.EncodeToString(sum[:]))
	}
	fi, err := os.Stat(segPath)
	if err != nil {
		t.Fatalf("stat segment: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o400 {
		t.Fatalf("segment perm = %o, want 400", perm)
	}
	if got := archiveTextList(archiveSegmentMessages(t, st, ArchiveScopeGroups, "grp_test", seg)); got != "m-01|m-02|m-03|m-04|m-05|m-06|m-07|m-08" {
		t.Fatalf("segment texts = %q", got)
	}

	// 再跑一次：热日志只剩 4 条，低于阈值，不写新段。
	res, err = st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{MaxMessages: 10, KeepRecent: 3, Percent: 70})
	if err != nil {
		t.Fatalf("second ArchiveGroup: %v", err)
	}
	if !res.Skipped || res.Archived != 0 || res.HotRemaining != 4 {
		t.Fatalf("second result = %+v", res)
	}
	if len(mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test").Segments) != 1 {
		t.Fatal("second archive must not add a segment")
	}
}

func TestArchiveSizeOnlyKeepsRecentFloor(t *testing.T) {
	st := newTestStore(t)
	appendArchiveGroupN(t, st, 3, "small")
	ctx := context.Background()
	hotPath := st.archiveHotLogPath(ArchiveScopeGroups, "grp_test")
	before, err := os.ReadFile(hotPath)
	if err != nil {
		t.Fatal(err)
	}
	pol := ArchivePolicy{MaxMessages: 1 << 30, MaxBytes: 10, KeepRecent: 3, Percent: 70}

	res, err := st.ArchiveGroup(ctx, "grp_test", pol)
	if err != nil {
		t.Fatalf("ArchiveGroup: %v", err)
	}
	if !res.Skipped || res.Archived != 0 || res.Reason != "keep_recent" {
		t.Fatalf("size-only under keep floor = %+v, want skipped/keep_recent", res)
	}
	after, err := os.ReadFile(hotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("skip must not rewrite or delete hot log")
	}
	if _, err := st.LoadArchiveManifest(ctx, ArchiveScopeGroups, "grp_test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manifest after skip = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(st.archiveDir(ArchiveScopeGroups, "grp_test")); !os.IsNotExist(err) {
		t.Fatalf("archive dir created on skip: %v", err)
	}

	// 超过 KeepRecent 后，体积触发可以归档最旧的 2 条（5*70/100=3，但 5-3=2）。
	appendArchiveGroupN(t, st, 2, "extra")
	res, err = st.ArchiveGroup(ctx, "grp_test", pol)
	if err != nil {
		t.Fatalf("size-triggered ArchiveGroup: %v", err)
	}
	if res.Skipped || res.Archived != 2 {
		t.Fatalf("size-triggered result = %+v, want archived=2", res)
	}
	assertArchiveSeqs(t, archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test"), 3, 4, 5)
}

func TestArchiveSequentialSegments(t *testing.T) {
	st := newTestStore(t)
	appendArchiveGroupN(t, st, 30, "s")
	ctx := context.Background()
	pol := ArchivePolicy{MaxMessages: 10, KeepRecent: 5, Percent: 50}

	res, err := st.ArchiveGroup(ctx, "grp_test", pol)
	if err != nil || res.Archived != 15 {
		t.Fatalf("first archive = %+v/%v, want 15", res, err)
	}
	res, err = st.ArchiveGroup(ctx, "grp_test", pol)
	if err != nil || res.Archived != 7 {
		t.Fatalf("second archive = %+v/%v, want 7", res, err)
	}
	m := mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test")
	if len(m.Segments) != 2 {
		t.Fatalf("segments = %+v, want 2", m.Segments)
	}
	if m.Segments[0].File != "1-15.jsonl" || m.Segments[1].File != "16-22.jsonl" {
		t.Fatalf("segment files = %q,%q", m.Segments[0].File, m.Segments[1].File)
	}
	if m.Segments[1].StartSeq <= m.Segments[0].EndSeq {
		t.Fatalf("segments overlap: %+v", m.Segments)
	}
	all, err := st.ReadMessages(ctx, "grp_test", 0, 500)
	if err != nil || len(all) != 30 {
		t.Fatalf("read all = %d/%v", len(all), err)
	}
	for i, msg := range all {
		if msg.Seq != int64(i+1) {
			t.Fatalf("all[%d].seq = %d, want %d", i, msg.Seq, i+1)
		}
	}
}

func TestArchiveReadMessagesHotColdPages(t *testing.T) {
	st := newTestStore(t)
	appendArchiveGroupN(t, st, 30, "g")
	ctx := context.Background()
	if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{
		MaxMessages: 10, KeepRecent: 5, Percent: 50,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := st.ReadMessages(ctx, "grp_test", 0, 500)
	if err != nil {
		t.Fatalf("ReadMessages all: %v", err)
	}
	if len(all) != 30 {
		t.Fatalf("all len = %d, want 30", len(all))
	}
	for i, m := range all {
		if m.Seq != int64(i+1) {
			t.Fatalf("all[%d].seq = %d, want %d", i, m.Seq, i+1)
		}
	}

	page, err := st.ReadMessages(ctx, "grp_test", 17, 4)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSeqs(t, page, 13, 14, 15, 16)

	page, err = st.ReadMessages(ctx, "grp_test", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSeqs(t, page, 28, 29, 30)

	page, err = st.ReadMessages(ctx, "grp_test", 6, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSeqs(t, page, 1, 2, 3, 4, 5)

	page, err = st.ReadMessages(ctx, "grp_test", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 0 {
		t.Fatalf("beforeSeq=1 = %v, want empty", archiveSeqList(page))
	}

	// 全量归档：热日志为空时仍能读，且跨两个段。
	forceArchiveAll(t, st, ArchiveScopeGroups, "grp_test")
	all, err = st.ReadMessages(ctx, "grp_test", 0, 500)
	if err != nil {
		t.Fatalf("ReadMessages fully archived: %v", err)
	}
	if len(all) != 30 || all[0].Seq != 1 || all[29].Seq != 30 {
		t.Fatalf("fully archived read = %v", archiveSeqList(all))
	}
	page, err = st.ReadMessages(ctx, "grp_test", 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSeqs(t, page, 2, 3, 4)
	if len(mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test").Segments) != 2 {
		t.Fatal("want two archive segments after full archive")
	}
}

func TestArchiveReadDirectMessagesHotColdPages(t *testing.T) {
	st := newTestStore(t)
	appendArchiveDMN(t, st, "ag_dm", 30, "d")
	ctx := context.Background()
	if _, err := st.ArchiveDM(ctx, "ag_dm", ArchivePolicy{
		MaxMessages: 10, KeepRecent: 5, Percent: 50,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := st.ReadDirectMessages(ctx, "ag_dm", 0, 500)
	if err != nil {
		t.Fatalf("ReadDirectMessages all: %v", err)
	}
	if len(all) != 30 || all[0].Seq != 1 || all[29].Seq != 30 {
		t.Fatalf("all = %v", archiveSeqList(all))
	}
	page, err := st.ReadDirectMessages(ctx, "ag_dm", 17, 4)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSeqs(t, page, 13, 14, 15, 16)
	if page, err = st.ReadDirectMessages(ctx, "ag_dm", 0, 0); err != nil || len(page) != 30 {
		t.Fatalf("default limit = %v, %v", archiveSeqList(page), err)
	}

	forceArchiveAll(t, st, ArchiveScopeDMs, "ag_dm")
	page, err = st.ReadDirectMessages(ctx, "ag_dm", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSeqs(t, page, 28, 29, 30)
}

func TestArchiveSearchVisibilityAndNoise(t *testing.T) {
	st := newTestStore(t)
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	specs := []struct {
		text string
		kind MessageKind
		vis  Visibility
	}{
		{"alpha normal one", KindText, VisibilityGroup},
		{"alpha owner secret", KindText, VisibilityOwner},
		{"alpha receipt targets", KindRouting, VisibilityGroup},
		{"alpha normal two", KindText, VisibilityGroup},
		{"alpha summary", KindRoundSummary, VisibilityGroup},
		{"alpha stub secret", KindApprovalStub, VisibilityGroup},
		{"alpha normal three", KindText, VisibilityGroup},
		{"alpha owner secret two", KindText, VisibilityOwner},
		{"alpha normal four", KindText, VisibilityGroup},
		{"beta normal", KindText, VisibilityGroup},
	}
	for i, s := range specs {
		appendArchiveGroupMsg(t, st, archiveGroupMsg(s.text, s.kind, s.vis, base.Add(time.Duration(i)*time.Second)))
	}
	ctx := context.Background()
	if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{
		MaxMessages: 4, KeepRecent: 3, Percent: 75,
	}); err != nil {
		t.Fatal(err)
	}

	agentHits, err := st.SearchMessages(ctx, "grp_test", "alpha", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha normal one|alpha normal two|alpha normal three|alpha normal four"; archiveTextList(agentHits) != want {
		t.Fatalf("agent search = %q, want %q", archiveTextList(agentHits), want)
	}
	agentHits, err = st.SearchMessages(ctx, "grp_test", "alpha", 8, 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha normal one|alpha normal two|alpha normal three"; archiveTextList(agentHits) != want {
		t.Fatalf("agent search before=8 = %q, want %q", archiveTextList(agentHits), want)
	}

	humanHits, err := st.SearchMessagesHuman(ctx, "grp_test", "alpha", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha normal one|alpha owner secret|alpha normal two|alpha normal three|alpha owner secret two|alpha normal four"; archiveTextList(humanHits) != want {
		t.Fatalf("human search = %q, want %q", archiveTextList(humanHits), want)
	}
	if humanHits[1].Visibility != VisibilityOwner {
		t.Fatalf("human search owner hit visibility = %q", humanHits[1].Visibility)
	}
	humanHits, err = st.SearchMessagesHuman(ctx, "grp_test", "alpha", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha owner secret two|alpha normal four"; archiveTextList(humanHits) != want {
		t.Fatalf("human search limit=2 = %q, want %q", archiveTextList(humanHits), want)
	}

	dmSpecs := []struct {
		text string
		kind MessageKind
		vis  Visibility
	}{
		{"alpha dm one", KindText, VisibilityGroup},
		{"alpha dm owner", KindText, VisibilityOwner},
		{"alpha dm receipt", KindRouting, VisibilityGroup},
		{"alpha dm two", KindText, VisibilityGroup},
		{"alpha dm summary", KindRoundSummary, VisibilityGroup},
		{"alpha dm three", KindText, VisibilityGroup},
		{"alpha dm owner two", KindText, VisibilityOwner},
		{"alpha dm four", KindText, VisibilityGroup},
	}
	for i, s := range dmSpecs {
		m := Message{Kind: s.kind, Sender: Sender{Kind: SenderHuman}, Text: s.text,
			Visibility: s.vis, CreatedAt: base.Add(time.Duration(i) * time.Second)}
		if _, err := st.AppendDirectMessage(ctx, "ag_dm", &m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ArchiveDM(ctx, "ag_dm", ArchivePolicy{
		MaxMessages: 3, KeepRecent: 2, Percent: 75,
	}); err != nil {
		t.Fatal(err)
	}
	dmHits, err := st.SearchDirectMessages(ctx, "ag_dm", "alpha", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha dm one|alpha dm owner|alpha dm two|alpha dm three|alpha dm owner two|alpha dm four"; archiveTextList(dmHits) != want {
		t.Fatalf("dm human search = %q, want %q", archiveTextList(dmHits), want)
	}
	dmHits, err = st.SearchDirectMessages(ctx, "ag_dm", "alpha", 6, 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alpha dm one|alpha dm owner|alpha dm two"; archiveTextList(dmHits) != want {
		t.Fatalf("dm human search before=6 = %q, want %q", archiveTextList(dmHits), want)
	}
}

func TestArchiveClientMsgIDDedupeGroupAndDM(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first := archiveGroupMsg("first", KindText, VisibilityGroup, testTime())
	first.Refs = map[string]any{"client_msg_id": "cid-g-1"}
	first = appendArchiveGroupMsg(t, st, first)
	for i := 0; i < 5; i++ {
		appendArchiveGroupMsg(t, st, archiveGroupMsg(fmt.Sprintf("g-%d", i), KindText, VisibilityGroup, testTime().Add(time.Duration(i)*time.Second)))
	}
	if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{
		MaxMessages: 3, KeepRecent: 2, Percent: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 {
		t.Fatalf("first seq = %d", first.Seq)
	}
	got, err := st.FindGroupMessageByClientID(ctx, "grp_test", "cid-g-1")
	if err != nil || got == nil || got.Seq != 1 || got.Text != "first" {
		t.Fatalf("FindGroupMessageByClientID cold = %+v, %v", got, err)
	}

	hotBefore, err := os.ReadFile(st.archiveHotLogPath(ArchiveScopeGroups, "grp_test"))
	if err != nil {
		t.Fatal(err)
	}
	retry := archiveGroupMsg("retry duplicate", KindText, VisibilityGroup, testTime())
	retry.Refs = map[string]any{"client_msg_id": "cid-g-1"}
	seq, deduped, err := st.AppendMessageWithRound(ctx, &retry, nil)
	if err != nil {
		t.Fatalf("AppendMessageWithRound dedupe: %v", err)
	}
	if !deduped || seq != 1 || retry.Seq != 1 || retry.Text != "first" {
		t.Fatalf("dedupe = seq %d deduped %v msg %+v", seq, deduped, retry)
	}
	hotAfter, err := os.ReadFile(st.archiveHotLogPath(ArchiveScopeGroups, "grp_test"))
	if err != nil {
		t.Fatal(err)
	}
	if string(hotBefore) != string(hotAfter) {
		t.Fatal("dedupe must not append to hot log")
	}
	if got == nil {
		t.Fatal("cold find returned nil")
	}

	fresh := archiveGroupMsg("fresh", KindText, VisibilityGroup, testTime())
	fresh.Refs = map[string]any{"client_msg_id": "cid-g-2"}
	seq, deduped, err = st.AppendMessageWithRound(ctx, &fresh, nil)
	if err != nil || deduped || seq != 7 {
		t.Fatalf("fresh append = %d/%v/%v, want 7/false", seq, deduped, err)
	}
	if got, err := st.FindGroupMessageByClientID(ctx, "grp_test", "cid-g-2"); err != nil || got == nil || got.Seq != 7 {
		t.Fatalf("FindGroupMessageByClientID hot = %+v, %v", got, err)
	}
	if got, err := st.FindGroupMessageByClientID(ctx, "grp_test", "missing"); err != nil || got != nil {
		t.Fatalf("FindGroupMessageByClientID missing = %+v, %v", got, err)
	}

	// DM：client_msg_id 归档后仍能命中（bridge 用 Find 做重发去重）。
	dmFirst := Message{Sender: Sender{Kind: SenderHuman}, Kind: KindText, Text: "dm first",
		Visibility: VisibilityGroup, Refs: map[string]any{"client_msg_id": "cid-d-1"}, CreatedAt: testTime()}
	if _, err := st.AppendDirectMessage(ctx, "ag_dm", &dmFirst); err != nil {
		t.Fatal(err)
	}
	appendArchiveDMN(t, st, "ag_dm", 5, "dm")
	if _, err := st.ArchiveDM(ctx, "ag_dm", ArchivePolicy{
		MaxMessages: 3, KeepRecent: 2, Percent: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if seq, ok, err := st.FindDirectMessageByClientID(ctx, "ag_dm", "cid-d-1"); err != nil || !ok || seq != 1 {
		t.Fatalf("FindDirectMessageByClientID cold = %d/%v/%v", seq, ok, err)
	}
	if _, ok, err := st.FindDirectMessageByClientID(ctx, "ag_dm", "missing"); err != nil || ok {
		t.Fatalf("FindDirectMessageByClientID missing = %v/%v", ok, err)
	}
	forceArchiveAll(t, st, ArchiveScopeDMs, "ag_dm")
	if seq, ok, err := st.FindDirectMessageByClientID(ctx, "ag_dm", "cid-d-1"); err != nil || !ok || seq != 1 {
		t.Fatalf("FindDirectMessageByClientID fully archived = %d/%v/%v", seq, ok, err)
	}
}

func TestArchiveClientMsgIDRetryDoesNotDuplicateReceipt(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	trigger := archiveGroupMsg("trigger", KindText, VisibilityGroup, testTime())
	trigger.Refs = map[string]any{"client_msg_id": "cid-receipt"}
	trigger = appendArchiveGroupMsg(t, st, trigger)
	// 补一条真实回执（seq=2）。
	receipt := Message{GroupID: "grp_test", Sender: Sender{Kind: SenderSystem}, Kind: KindRouting,
		Text: "receipt real", Visibility: VisibilityGroup, Refs: map[string]any{"trigger_seq": float64(trigger.Seq)},
		CreatedAt: testTime().Add(2 * time.Second)}
	if _, err := st.AppendMessage(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		appendArchiveGroupMsg(t, st, archiveGroupMsg(fmt.Sprintf("tail-%d", i), KindText, VisibilityGroup, testTime().Add(time.Duration(i+3)*time.Second)))
	}
	if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{
		MaxMessages: 2, KeepRecent: 2, Percent: 100,
	}); err != nil {
		t.Fatal(err)
	}
	hotBefore := archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test")

	retry := archiveGroupMsg("trigger retry", KindText, VisibilityGroup, testTime())
	retry.Refs = map[string]any{"client_msg_id": "cid-receipt"}
	round := &Round{ID: "rnd_archive", GroupID: "grp_test", TriggerSeq: 1}
	seq, deduped, err := st.AppendMessageWithRound(ctx, &retry, round)
	if err != nil || !deduped || seq != 1 {
		t.Fatalf("retry = %d/%v/%v", seq, deduped, err)
	}
	hotAfter := archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test")
	if len(hotBefore) != len(hotAfter) {
		t.Fatalf("retry appended duplicate receipt: hot %d -> %d", len(hotBefore), len(hotAfter))
	}
}

func TestArchiveDMUnreadAcrossCold(t *testing.T) {
	root := t.TempDir()
	st, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	appendArchiveDMN(t, st, "ag_unread", 10, "u")
	if err := st.SaveDMCursor(ctx, "ag_unread", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ArchiveDM(ctx, "ag_unread", ArchivePolicy{
		MaxMessages: 5, KeepRecent: 3, Percent: 80,
	}); err != nil {
		t.Fatal(err)
	}
	unread, last, err := st.DMUnread(ctx, "ag_unread")
	if err != nil || unread != 7 || last == nil || last.Seq != 10 {
		t.Fatalf("unread after archive = %d/%+v/%v, want 7/seq10", unread, last, err)
	}

	forceArchiveAll(t, st, ArchiveScopeDMs, "ag_unread")
	st2, err := OpenFileStore(root)
	if err != nil {
		t.Fatalf("reopen fully archived DM: %v", err)
	}
	unread, last, err = st2.DMUnread(ctx, "ag_unread")
	if err != nil || unread != 7 || last == nil || last.Seq != 10 || last.Text != "u-10" {
		t.Fatalf("unread fully cold = %d/%+v/%v", unread, last, err)
	}
	if err := st2.SaveDMCursor(ctx, "ag_unread", 8); err != nil {
		t.Fatal(err)
	}
	unread, last, err = st2.DMUnread(ctx, "ag_unread")
	if err != nil || unread != 2 || last == nil || last.Seq != 10 {
		t.Fatalf("unread cursor=8 = %d/%+v/%v", unread, last, err)
	}

	// 模拟 manifest 已提交、热未裁：热里重复的已归档 seq 不能重复计入未读。
	cold, err := st2.readAllArchived(ArchiveScopeDMs, "ag_unread")
	if err != nil {
		t.Fatal(err)
	}
	for i := range cold {
		if err := appendJSONL(st2.dmLogPath("ag_unread"), &cold[i]); err != nil {
			t.Fatal(err)
		}
	}
	unread, last, err = st2.DMUnread(ctx, "ag_unread")
	if err != nil || unread != 2 || last == nil || last.Seq != 10 {
		t.Fatalf("unread with duplicate hot = %d/%+v/%v, want dedupe", unread, last, err)
	}
	if res, err := st2.ArchiveDM(ctx, "ag_unread", ArchivePolicy{MaxMessages: 1000, KeepRecent: 3, Percent: 70}); err != nil || !res.Repaired {
		t.Fatalf("repair result = %+v/%v", res, err)
	}
	if got := len(archiveHotMessages(t, st2, ArchiveScopeDMs, "ag_unread")); got != 0 {
		t.Fatalf("hot after repair = %d lines, want 0", got)
	}
}

func TestArchiveRestartSeq(t *testing.T) {
	ctx := context.Background()

	t.Run("hot and cold", func(t *testing.T) {
		root := t.TempDir()
		st, err := OpenFileStore(root)
		if err != nil {
			t.Fatal(err)
		}
		appendArchiveGroupN(t, st, 20, "r")
		if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{MaxMessages: 10, KeepRecent: 5, Percent: 100}); err != nil {
			t.Fatal(err)
		}
		st2, err := OpenFileStore(root)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		m := archiveGroupMsg("after restart", KindText, VisibilityGroup, testTime())
		if seq, err := st2.AppendMessage(ctx, &m); err != nil || seq != 21 {
			t.Fatalf("append after restart = %d/%v, want 21", seq, err)
		}
	})

	t.Run("fully archived hot empty", func(t *testing.T) {
		root := t.TempDir()
		st, err := OpenFileStore(root)
		if err != nil {
			t.Fatal(err)
		}
		appendArchiveGroupN(t, st, 20, "r")
		if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{MaxMessages: 10, KeepRecent: 5, Percent: 100}); err != nil {
			t.Fatal(err)
		}
		forceArchiveAll(t, st, ArchiveScopeGroups, "grp_test")
		if got := len(archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test")); got != 0 {
			t.Fatalf("hot = %d, want 0", got)
		}
		st2, err := OpenFileStore(root)
		if err != nil {
			t.Fatalf("reopen fully archived: %v", err)
		}
		if got, err := st2.ReadMessages(ctx, "grp_test", 0, 500); err != nil || len(got) != 20 {
			t.Fatalf("read after reopen = %d/%v", len(got), err)
		}
		m := archiveGroupMsg("after full archive", KindText, VisibilityGroup, testTime())
		if seq, err := st2.AppendMessage(ctx, &m); err != nil || seq != 21 {
			t.Fatalf("append after full archive = %d/%v, want 21", seq, err)
		}
	})

	t.Run("dm fully archived", func(t *testing.T) {
		root := t.TempDir()
		st, err := OpenFileStore(root)
		if err != nil {
			t.Fatal(err)
		}
		appendArchiveDMN(t, st, "ag_r", 20, "r")
		if _, err := st.ArchiveDM(ctx, "ag_r", ArchivePolicy{MaxMessages: 10, KeepRecent: 5, Percent: 100}); err != nil {
			t.Fatal(err)
		}
		forceArchiveAll(t, st, ArchiveScopeDMs, "ag_r")
		st2, err := OpenFileStore(root)
		if err != nil {
			t.Fatalf("reopen fully archived DM: %v", err)
		}
		m := Message{Sender: Sender{Kind: SenderHuman}, Kind: KindText, Text: "after", Visibility: VisibilityGroup}
		if seq, err := st2.AppendDirectMessage(ctx, "ag_r", &m); err != nil || seq != 21 {
			t.Fatalf("dm append after full archive = %d/%v, want 21", seq, err)
		}
	})
}

func TestArchiveReusesOrphanSegment(t *testing.T) {
	st := newTestStore(t)
	msgs := appendArchiveGroupN(t, st, 12, "o")
	pol := ArchivePolicy{MaxMessages: 10, KeepRecent: 4, Percent: 100}
	// 先手工写出「段已完成、manifest 未提交」的孤儿段（与归档将产生的字节一致）。
	batch := msgs[:8]
	data, err := encodeMessages(batch)
	if err != nil {
		t.Fatal(err)
	}
	seg := ArchiveSegment{
		StartSeq: batch[0].Seq, EndSeq: batch[len(batch)-1].Seq, Count: len(batch),
		Bytes: int64(len(data)), SHA256: sha256Hex(data), File: "1-8.jsonl",
		ArchivedAt: time.Now().UTC(),
	}
	dir := st.archiveDir(ArchiveScopeGroups, "grp_test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	segPath := filepath.Join(dir, seg.File)
	if err := os.WriteFile(segPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(segPath)
	if err != nil {
		t.Fatal(err)
	}

	res, err := st.ArchiveGroup(context.Background(), "grp_test", pol)
	if err != nil {
		t.Fatalf("ArchiveGroup with orphan segment: %v", err)
	}
	if res.Archived != 8 || res.Segment == nil || res.Segment.File != "1-8.jsonl" {
		t.Fatalf("result = %+v", res)
	}
	afterInfo, err := os.Stat(segPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("orphan segment must be reused, not rewritten")
	}
	if perm := afterInfo.Mode().Perm(); perm != 0o400 {
		t.Fatalf("reused segment perm = %o, want 400", perm)
	}
	if got := len(mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test").Segments); got != 1 {
		t.Fatalf("segments = %d, want 1", got)
	}
	assertArchiveSeqs(t, archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test"), 9, 10, 11, 12)
}

func TestArchiveRepairsManifestCommittedHotNotPruned(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	appendArchiveGroupN(t, st, 12, "c")
	if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{MaxMessages: 10, KeepRecent: 4, Percent: 100}); err != nil {
		t.Fatal(err)
	}
	m := mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test")
	cold := archiveSegmentMessages(t, st, ArchiveScopeGroups, "grp_test", m.Segments[0])

	// 模拟崩溃窗口：manifest 已提交，但热日志没有裁剪；热副本内容不同以验证热优先。
	hotPath := st.archiveHotLogPath(ArchiveScopeGroups, "grp_test")
	for _, msg := range cold {
		dup := msg
		dup.Text = fmt.Sprintf("hot-dup-%d", msg.Seq)
		if err := appendJSONL(hotPath, &dup); err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.ReadMessages(ctx, "grp_test", 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 12 {
		t.Fatalf("read with duplicate hot = %d, want 12", len(all))
	}
	for i := 0; i < 8; i++ {
		if all[i].Seq != int64(i+1) || all[i].Text != fmt.Sprintf("hot-dup-%d", i+1) {
			t.Fatalf("all[%d] = seq %d text %q, want hot copy", i, all[i].Seq, all[i].Text)
		}
	}
	// 分页同样按 seq 取最新 limit 条，不能因为热里重复的旧 seq 打乱顺序。
	page, err := st.ReadMessages(ctx, "grp_test", 13, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSeqs(t, page, 10, 11, 12)
	if page[0].Text != "c-10" || page[2].Text != "c-12" {
		t.Fatalf("hot page texts = %q", archiveTextList(page))
	}
	page, err = st.ReadMessages(ctx, "grp_test", 9, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSeqs(t, page, 6, 7, 8)
	if page[0].Text != "hot-dup-6" || page[2].Text != "hot-dup-8" {
		t.Fatalf("dedupe boundary texts = %q", archiveTextList(page))
	}

	res, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{MaxMessages: 1000, KeepRecent: 3, Percent: 70})
	if err != nil {
		t.Fatalf("repair ArchiveGroup: %v", err)
	}
	if !res.Skipped || !res.Repaired || res.HotRemaining != 4 {
		t.Fatalf("repair result = %+v, want skipped/repaired/hot=4", res)
	}
	hot := archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test")
	assertArchiveSeqs(t, hot, 9, 10, 11, 12)
	all, err = st.ReadMessages(ctx, "grp_test", 0, 500)
	if err != nil || len(all) != 12 {
		t.Fatalf("read after repair = %d/%v", len(all), err)
	}
}

func TestArchiveCorruptSegmentErrors(t *testing.T) {
	seed := func(t *testing.T) (*FileStore, string, ArchiveSegment) {
		t.Helper()
		st := newTestStore(t)
		appendArchiveGroupN(t, st, 12, "x")
		if _, err := st.ArchiveGroup(context.Background(), "grp_test", ArchivePolicy{
			MaxMessages: 10, KeepRecent: 4, Percent: 100,
		}); err != nil {
			t.Fatalf("seed archive: %v", err)
		}
		seg := mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test").Segments[0]
		return st, filepath.Join(st.archiveDir(ArchiveScopeGroups, "grp_test"), seg.File), seg
	}

	t.Run("missing", func(t *testing.T) {
		st, path, _ := seed(t)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ReadMessages(context.Background(), "grp_test", 0, 50); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("read missing segment = %v, want ErrCorrupt", err)
		}
		if _, err := OpenFileStore(st.Root()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open missing segment = %v, want ErrCorrupt", err)
		}
	})

	t.Run("size", func(t *testing.T) {
		st, path, _ := seed(t)
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if _, err := st.ReadMessages(context.Background(), "grp_test", 0, 50); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("read size-mismatched segment = %v, want ErrCorrupt", err)
		}
		if _, err := OpenFileStore(st.Root()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open size-mismatched segment = %v, want ErrCorrupt", err)
		}
	})

	t.Run("sha256", func(t *testing.T) {
		st, path, _ := seed(t)
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[0] ^= 0x01
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ReadMessages(context.Background(), "grp_test", 0, 50); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("read hash-mismatched segment = %v, want ErrCorrupt", err)
		}
		// 尺寸没变，OpenFileStore 的结构校验应通过；真正读到内容时才报 ErrCorrupt。
		if _, err := OpenFileStore(st.Root()); err != nil {
			t.Fatalf("open same-size corrupt segment = %v, want nil (hash checked on read)", err)
		}
	})
}

func TestOpenFileStoreRejectsCorruptArchiveManifest(t *testing.T) {
	seed := func(t *testing.T) (string, ArchiveSegment) {
		t.Helper()
		root := t.TempDir()
		st, err := OpenFileStore(root)
		if err != nil {
			t.Fatal(err)
		}
		appendArchiveGroupN(t, st, 12, "x")
		if _, err := st.ArchiveGroup(context.Background(), "grp_test", ArchivePolicy{
			MaxMessages: 10, KeepRecent: 4, Percent: 100,
		}); err != nil {
			t.Fatal(err)
		}
		return root, mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test").Segments[0]
	}
	manifestPath := func(root string) string {
		return filepath.Join(root, "archive", ArchiveScopeGroups, "grp_test", "manifest.json")
	}
	readManifest := func(t *testing.T, root string) ArchiveManifest {
		t.Helper()
		var m ArchiveManifest
		if err := readJSON(manifestPath(root), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	writeManifest := func(t *testing.T, root string, m ArchiveManifest) {
		t.Helper()
		if err := writeJSONAtomic(manifestPath(root), &m); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("schema", func(t *testing.T) {
		root, _ := seed(t)
		m := readManifest(t, root)
		m.SchemaVersion = 99
		writeManifest(t, root, m)
		if _, err := OpenFileStore(root); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open schema=99 = %v, want ErrCorrupt", err)
		}
	})

	t.Run("scope", func(t *testing.T) {
		root, _ := seed(t)
		m := readManifest(t, root)
		m.Scope = ArchiveScopeDMs
		writeManifest(t, root, m)
		if _, err := OpenFileStore(root); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open wrong scope = %v, want ErrCorrupt", err)
		}
	})

	t.Run("overlap", func(t *testing.T) {
		root, seg := seed(t)
		m := readManifest(t, root)
		extra := seg
		extra.StartSeq = 8
		extra.EndSeq = 12
		extra.Count = 5
		extra.File = "8-12.jsonl"
		extra.Bytes = 0
		extra.SHA256 = strings.Repeat("0", 64)
		m.Segments = append(m.Segments, extra)
		writeManifest(t, root, m)
		if _, err := OpenFileStore(root); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open overlapping manifest = %v, want ErrCorrupt", err)
		}
	})

	t.Run("orphan not fatal", func(t *testing.T) {
		root, _ := seed(t)
		orphanDir := filepath.Join(root, "archive", ArchiveScopeGroups, "grp_test")
		if err := os.WriteFile(filepath.Join(orphanDir, "99-100.jsonl"), []byte("{}\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenFileStore(root); err != nil {
			t.Fatalf("orphan segment without manifest must be ignored: %v", err)
		}
	})
}

func TestArchiveConcurrentAppendNoLoss(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	appendArchiveGroupN(t, st, 100, "base")

	const extra = 100
	var errMu sync.Mutex
	var appendErr error
	setErr := func(err error) {
		errMu.Lock()
		if appendErr == nil {
			appendErr = err
		}
		errMu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < extra; i++ {
			m := archiveGroupMsg(fmt.Sprintf("live-%03d", i), KindText, VisibilityGroup, testTime().Add(time.Duration(i)*time.Millisecond))
			if _, err := st.AppendMessage(ctx, &m); err != nil {
				setErr(err)
				return
			}
			time.Sleep(300 * time.Microsecond)
		}
	}()

	var archived int64
	var wg sync.WaitGroup
	for a := 0; a < 2; a++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				res, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{
					MaxMessages: 50, KeepRecent: 20, Percent: 50,
				})
				if err != nil {
					setErr(err)
					return
				}
				if res.Archived > 0 {
					atomic.AddInt64(&archived, int64(res.Archived))
				}
			}
		}()
	}
	<-done
	wg.Wait()
	errMu.Lock()
	concurrentErr := appendErr
	errMu.Unlock()
	if concurrentErr != nil {
		t.Fatalf("concurrent append/archive: %v", concurrentErr)
	}
	if atomic.LoadInt64(&archived) == 0 {
		t.Fatal("archive never ran while appends were in flight")
	}

	all, err := st.ReadMessages(ctx, "grp_test", 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 100+extra {
		t.Fatalf("messages after concurrent archive = %d, want %d", len(all), 100+extra)
	}
	for i, m := range all {
		if m.Seq != int64(i+1) {
			t.Fatalf("all[%d].seq = %d, want %d (lost/reused seq)", i, m.Seq, i+1)
		}
	}
	// 热 + 冷各段条数之和也必须等于总数：不允许归档期间重复或丢失。
	total := len(archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test"))
	m := mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test")
	for _, seg := range m.Segments {
		total += seg.Count
	}
	if total != 100+extra {
		t.Fatalf("hot+segments count = %d, want %d", total, 100+extra)
	}
}

func TestArchiveDoesNotDeleteOtherData(t *testing.T) {
	root := t.TempDir()
	st, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SaveAgent(ctx, testAgent()); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroup(ctx, testGroup()); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendActivity(ctx, &Activity{GroupID: "grp_test", Kind: "status", Name: "keep", At: testTime()}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveArtifact(ctx, testArtifact()); err != nil {
		t.Fatal(err)
	}
	// 归档前快照所有非热日志文件；归档后必须逐字节不变且仍存在。
	before := map[string][]byte{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Base(path) == "log.jsonl" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		before[path] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	appendArchiveGroupN(t, st, 30, "keep")
	if _, err := st.ArchiveGroup(ctx, "grp_test", ArchivePolicy{
		MaxMessages: 10, KeepRecent: 5, Percent: 100,
	}); err != nil {
		t.Fatal(err)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("file disappeared after archive: %s: %v", path, err)
		}
		if string(got) != string(want) {
			t.Fatalf("file changed after archive: %s", path)
		}
	}
	if got := len(archiveHotMessages(t, st, ArchiveScopeGroups, "grp_test")); got != 5 {
		t.Fatalf("hot messages = %d, want 5", got)
	}
	m := mustArchiveManifest(t, st, ArchiveScopeGroups, "grp_test")
	if len(m.Segments) != 1 || m.Segments[0].Count != 25 {
		t.Fatalf("segments = %+v, want one 25-message segment", m.Segments)
	}
	if _, err := os.Stat(filepath.Join(root, "archive", "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("artifact archive must not exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "archive", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("session archive must not exist: %v", err)
	}
}

func TestArchiveIfNeededScansGroupsAndDMs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.SaveAgent(ctx, testAgent()); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroup(ctx, testGroup()); err != nil {
		t.Fatal(err)
	}
	deadGroup := testGroup()
	deadGroup.ID = "grp_dead"
	deadGroup.Deleted = true
	if err := st.SaveGroup(ctx, deadGroup); err != nil {
		t.Fatal(err)
	}
	deadAgent := testAgent()
	deadAgent.ID = "ag_dead"
	deadAgent.Deleted = true
	if err := st.SaveAgent(ctx, deadAgent); err != nil {
		t.Fatal(err)
	}
	appendArchiveGroupN(t, st, 12, "g")
	appendArchiveDMN(t, st, "ag_test", 12, "d")
	appendArchiveGroupMsg(t, st, Message{GroupID: "grp_dead", Kind: KindText, Text: "dead", Visibility: VisibilityGroup, Sender: Sender{Kind: SenderHuman}})
	deadDM := Message{Sender: Sender{Kind: SenderHuman}, Kind: KindText, Text: "dead", Visibility: VisibilityGroup}
	if _, err := st.AppendDirectMessage(ctx, "ag_dead", &deadDM); err != nil {
		t.Fatal(err)
	}

	results, err := st.archiveIfNeeded(ctx, ArchivePolicy{MaxMessages: 10, KeepRecent: 3, Percent: 70})
	if err != nil {
		t.Fatalf("archiveIfNeeded: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want group+dm only", results)
	}
	seen := map[string]int{}
	for _, r := range results {
		seen[r.Scope+"/"+r.ID] = r.Archived
	}
	if seen[ArchiveScopeGroups+"/grp_test"] != 8 || seen[ArchiveScopeDMs+"/ag_test"] != 8 {
		t.Fatalf("archived counts = %v", seen)
	}
	if _, err := st.LoadArchiveManifest(ctx, ArchiveScopeGroups, "grp_dead"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted group archived: %v", err)
	}
	if _, err := st.LoadArchiveManifest(ctx, ArchiveScopeDMs, "ag_dead"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted agent archived: %v", err)
	}

	// 公开入口用默认策略：小日志只报告 skipped，不删任何消息。
	results, err = st.ArchiveIfNeeded(ctx)
	if err != nil {
		t.Fatalf("ArchiveIfNeeded: %v", err)
	}
	for _, r := range results {
		if !r.Skipped {
			t.Fatalf("default policy should skip small log: %+v", r)
		}
	}
}

func TestArchiveInvalidInputs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.ArchiveGroup(ctx, "../g", ArchivePolicy{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ArchiveGroup invalid id = %v", err)
	}
	if _, err := st.ArchiveDM(ctx, "", ArchivePolicy{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ArchiveDM invalid id = %v", err)
	}
	if _, err := st.LoadArchiveManifest(ctx, "bogus", "grp_test"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("LoadArchiveManifest bad scope = %v", err)
	}
	if _, err := st.LoadArchiveManifest(ctx, ArchiveScopeGroups, "grp_test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadArchiveManifest missing = %v, want ErrNotFound", err)
	}
}
