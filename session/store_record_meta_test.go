package session

// LoadRecordMetadata 是 open_session 的性能优化：只读 record 头部三个标量，
// 不 materialize 24MB~90MB 的 sdk_state/view_checkpoint（实测 660ms → 104ms）。
//
// 风险不在快慢，而在**接受面**：若元数据读取比 LoadRecord 更宽松，原本
// 「record 损坏 → 回退 legacy 恢复」的会话会变成「报 checkpoint 恢复成功但内容
// 缺失」；更严格则让本可恢复的会话退化。所以下面的用例严格对齐两者判定。
//
// 测试设计要点（吃过教训）：每个用例只损坏 **formal**，让 .bak 保持合法。
// 否则两侧都会回退成功、判定「一致地通过」，把 formal 上的差异完全掩盖，
// 变异测试会假绿。每个用例都同时断言「formal 判定」与「回退后的结果」。

import (
	"context"
	"errors"
	"os"
	"testing"
)

func newStore(t *testing.T) *FileStorage {
	t.Helper()
	s, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// seedGoodFormalAndBackup 落一条合法 formal 与一条合法 .bak（revision 更低）。
func seedGoodFormalAndBackup(t *testing.T, s *FileStorage) (formal string) {
	t.Helper()
	ctx := context.Background()
	first := testSessionRecord(t, "workspace-a", "session-1", 4)
	if err := s.CommitRecord(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := testSessionRecord(t, "workspace-a", "session-1", 5)
	if err := s.CommitRecord(ctx, second); err != nil {
		t.Fatal(err)
	}
	p, err := s.RecordPath("workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".bak"); err != nil {
		t.Fatalf("用例需要合法 .bak 才能区分 formal 判定: %v", err)
	}
	return p
}

// assertBothRejectFormal 断言两个方法都**拒绝 formal**、并回退到 .bak。
// 回退结果必须完全相同（都是 revision 4 的那份）。
func assertBothRejectFormal(t *testing.T, s *FileStorage, label string) {
	t.Helper()
	ctx := context.Background()
	full, fullErr := s.LoadRecord(ctx, "workspace-a", "session-1")
	meta, metaErr := s.LoadRecordMetadata(ctx, "workspace-a", "session-1")
	if fullErr != nil || metaErr != nil {
		// 两侧都失败也可接受，但必须一致。
		if (fullErr == nil) != (metaErr == nil) {
			t.Fatalf("%s: 判定不一致 LoadRecord=%v Metadata=%v", label, fullErr, metaErr)
		}
		return
	}
	if full.Revision != meta.Revision || full.ReducerSeq != meta.ReducerSeq ||
		full.ClearGeneration != meta.ClearGeneration {
		t.Fatalf("%s: 回退后元数据不一致 LoadRecord rev=%d seq=%d gen=%d / Metadata rev=%d seq=%d gen=%d",
			label, full.Revision, full.ReducerSeq, full.ClearGeneration,
			meta.Revision, meta.ReducerSeq, meta.ClearGeneration)
	}
}

// assertBothAcceptFormal 断言两者都接受 formal（不触碰 .bak）。
func assertBothAcceptFormal(t *testing.T, s *FileStorage, label string) {
	t.Helper()
	ctx := context.Background()
	full, fullErr := s.LoadRecord(ctx, "workspace-a", "session-1")
	meta, metaErr := s.LoadRecordMetadata(ctx, "workspace-a", "session-1")
	if fullErr != nil || metaErr != nil {
		t.Fatalf("%s: 两侧都应接受，LoadRecord=%v Metadata=%v", label, fullErr, metaErr)
	}
	if full.Revision != meta.Revision || full.ReducerSeq != meta.ReducerSeq ||
		full.ClearGeneration != meta.ClearGeneration {
		t.Fatalf("%s: 元数据不一致 LoadRecord=%+v Metadata=%+v", label, full, meta)
	}
}

// 合法 record：两者都接受，且三个标量逐字段相同。
func TestLoadRecordMetadataMatchesOnValidRecord(t *testing.T) {
	s := newStore(t)
	p := seedGoodFormalAndBackup(t, s)
	assertBothAcceptFormal(t, s, "合法 formal")
	_ = p
}

// formal 损坏的每种形态：两个方法都必须拒绝 formal（并回退 .bak）。
// 注意「payload 被篡改」这一条：LoadRecord 会因 checksum 失配拒绝，
// 而 LoadRecordMetadata 不做 payload checksum 校验 —— 它**会接受**。
// 这是有意差异（打开会话不用 payload），但必须被显式记录下来，
// 所以这一条用专门的断言，不混进「必须一致」的集合。
func TestLoadRecordMetadataRejectsCorruptFormal(t *testing.T) {
	cases := []struct {
		label   string
		content string
	}{
		{"截断的 JSON", `{"type":"session_record","schema_ver`},
		{"空文件", ``},
		{"不是 JSON 对象", `[1,2,3]`},
		{"type 不匹配", `{"type":"other","schema_version":1,"session_id":"session-1",` +
			`"workspace_key":"workspace-a","revision":9,"view_checkpoint":{"schema_version":1,"blocks":[],"run_nodes":[],"turns":[]}}`},
		{"schema_version 不匹配", `{"type":"session_record","schema_version":99,"session_id":"session-1",` +
			`"workspace_key":"workspace-a","revision":9,"view_checkpoint":{"schema_version":1,"blocks":[],"run_nodes":[],"turns":[]}}`},
		{"session_id 不匹配", `{"type":"session_record","schema_version":1,"session_id":"someone-else",` +
			`"workspace_key":"workspace-a","revision":9,"view_checkpoint":{"schema_version":1,"blocks":[],"run_nodes":[],"turns":[]}}`},
		{"workspace_key 不匹配", `{"type":"session_record","schema_version":1,"session_id":"session-1",` +
			`"workspace_key":"other-ws","revision":9,"view_checkpoint":{"schema_version":1,"blocks":[],"run_nodes":[],"turns":[]}}`},
		{"revision 为 0", `{"type":"session_record","schema_version":1,"session_id":"session-1",` +
			`"workspace_key":"workspace-a","revision":0,"view_checkpoint":{"schema_version":1,"blocks":[],"run_nodes":[],"turns":[]}}`},
		{"view_checkpoint 缺失", `{"type":"session_record","schema_version":1,"session_id":"session-1",` +
			`"workspace_key":"workspace-a","revision":9}`},
		{"view_checkpoint 不是对象", `{"type":"session_record","schema_version":1,"session_id":"session-1",` +
			`"workspace_key":"workspace-a","revision":9,"view_checkpoint":[1,2]}`},
		{"view_checkpoint 缺 blocks", `{"type":"session_record","schema_version":1,"session_id":"session-1",` +
			`"workspace_key":"workspace-a","revision":9,"view_checkpoint":{"schema_version":1}}`},
		{"view_checkpoint schema 不对", `{"type":"session_record","schema_version":1,"session_id":"session-1",` +
			`"workspace_key":"workspace-a","revision":9,"view_checkpoint":{"schema_version":7,"blocks":[],"run_nodes":[],"turns":[]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			s := newStore(t)
			formal := seedGoodFormalAndBackup(t, s)
			if err := os.WriteFile(formal, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			// 关键：此时 .bak 合法，两个方法都应拒绝 formal 并回退到 .bak（rev=4）。
			assertBothRejectFormal(t, s, tc.label)

			// 再确认「回退确实发生了」而非「两侧都失败」：删掉 .bak 后，
			// 两个方法都必须报错（且都应是 corrupt 而非 not found）。
			if err := os.Remove(formal + ".bak"); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			_, fullErr := s.LoadRecord(ctx, "workspace-a", "session-1")
			_, metaErr := s.LoadRecordMetadata(ctx, "workspace-a", "session-1")
			if fullErr == nil || metaErr == nil {
				t.Fatalf("%s: 无 .bak 时两侧都应失败, LoadRecord=%v Metadata=%v", tc.label, fullErr, metaErr)
			}
			if errors.Is(metaErr, ErrRecordNotFound) {
				t.Fatalf("%s: 损坏的 formal 不应报 not found（会被当成无 record 去懒迁移）: %v", tc.label, metaErr)
			}
		})
	}
}

// 元数据路径**也**校验 payload 完整性（2026-10-18 加强）。
//
// 历史：最初为了性能，LoadRecordMetadata 刻意跳过 payload 校验。
// 后来改为「按磁盘字节算 checksum」（v2 口径）—— 校验一个 91MB 文件只要
// ~32ms，比解 payload 便宜一个数量级，于是**没有理由再削弱防线**，
// 两条路径的完整性保证统一了。
func TestLoadRecordMetadataAlsoValidatesIntegrity(t *testing.T) {
	s := newStore(t)
	formal := seedGoodFormalAndBackup(t, s)

	data, err := os.ReadFile(formal)
	if err != nil {
		t.Fatal(err)
	}
	orig := string(data)
	tampered := swapFirstBlockID(orig)
	if tampered == orig {
		t.Skip("夹具里没找到可篡改的 block id（结构变化）")
	}
	if err := os.Remove(formal + ".bak"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(formal, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := s.LoadRecord(ctx, "workspace-a", "session-1"); err == nil {
		t.Fatal("LoadRecord 应拒绝篡改过的 formal")
	}
	if _, err := s.LoadRecordMetadata(ctx, "workspace-a", "session-1"); err == nil {
		t.Fatal("LoadRecordMetadata 也必须拒绝篡改过的 formal（v2 字节口径让它足够便宜）")
	}
}

// swapFirstBlockID 把 JSON 里第一个 "id":"..." 的值改掉，制造 checksum 失配
// 而不破坏 envelope 结构。
func swapFirstBlockID(s string) string {
	const marker = `"id":"`
	i := indexOf(s, marker)
	if i < 0 {
		return s
	}
	j := i + len(marker)
	k := j
	for k < len(s) && s[k] != '"' {
		k++
	}
	if k >= len(s) {
		return s
	}
	repl := s[j:k]
	if repl == "tampered" {
		return s
	}
	return s[:j] + "tampered" + s[k:]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 没有 record 也没有备份：两者都必须报 ErrRecordNotFound（调用方据此走 legacy 懒迁移）。
func TestLoadRecordMetadataNotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.LoadRecord(ctx, "workspace-a", "session-1"); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("LoadRecord: want ErrRecordNotFound, got %v", err)
	}
	if _, err := s.LoadRecordMetadata(ctx, "workspace-a", "session-1"); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("LoadRecordMetadata: want ErrRecordNotFound, got %v", err)
	}
}

// formal 缺失、只有 .bak：两者都应回退成功（进程在 rename 中间崩溃的场景）。
func TestLoadRecordMetadataFallsBackToBackup(t *testing.T) {
	s := newStore(t)
	formal := seedGoodFormalAndBackup(t, s)
	if err := os.Remove(formal); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	full, err := s.LoadRecord(ctx, "workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.LoadRecordMetadata(ctx, "workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if full.Revision != meta.Revision {
		t.Fatalf("回退结果不一致: LoadRecord=%d Metadata=%d", full.Revision, meta.Revision)
	}
}

// 上下文已取消时两者都不应做 I/O。
func TestLoadRecordMetadataCanceled(t *testing.T) {
	s := newStore(t)
	_ = seedGoodFormalAndBackup(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.LoadRecordMetadata(ctx, "workspace-a", "session-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// record 存在但不可用时，LoadRecordMetadata 必须返回**非 not-found** 的错误。
//
// 调用方（open_session）用它区分两种情况：
//   ErrRecordNotFound → 真没 record → 允许懒迁移（重放 events 并覆盖写）
//   其它错误          → record 存在但坏了 → 不迁移、不覆盖，回退 legacy 快照
// 这条边界是 V2 P1-PERSIST-03 的红线：搞混会把损坏但可抢救的 record 覆盖掉。
func TestLoadRecordMetadataCorruptIsNotNotFound(t *testing.T) {
	s := newStore(t)
	formal := seedGoodFormalAndBackup(t, s)
	// 把 formal 与 .bak 都弄坏 → 无法回退
	if err := os.WriteFile(formal, []byte(`{"type":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formal+".bak", []byte(`{"type":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.LoadRecordMetadata(context.Background(), "workspace-a", "session-1")
	if err == nil {
		t.Fatal("损坏的 record 不应成功")
	}
	if errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("损坏的 record 绝不能报 not found（会让调用方去迁移覆盖它）: %v", err)
	}
	if !errors.Is(err, ErrRecordCorrupt) {
		t.Fatalf("应报 ErrRecordCorrupt，得到 %v", err)
	}

	// 对照：真没 record 时必须是 ErrRecordNotFound。
	empty := newStore(t)
	if _, err := empty.LoadRecordMetadata(context.Background(), "workspace-a", "session-1"); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("无 record 时应报 ErrRecordNotFound，得到 %v", err)
	}
}
