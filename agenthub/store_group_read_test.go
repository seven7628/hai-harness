package agenthub

// P3b-06：群级人类已读游标（Group.HumanReadSeq）的存储语义。

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func groupReadStores(t *testing.T) map[string]Store {
	t.Helper()
	fs, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Store{"file": fs, "memory": NewMemoryStore()}
}

// 只前进：重复/回退的 mark_read 不能把已读变回未读；未知群返回 ErrNotFound。
func TestStoreGroupReadCursorMonotonic(t *testing.T) {
	for name, st := range groupReadStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			g := testGroup()
			if err := st.SaveGroup(ctx, g); err != nil {
				t.Fatal(err)
			}
			if seq, err := st.MarkGroupRead(ctx, g.ID, 5); err != nil || seq != 5 {
				t.Fatalf("mark 5 = %d, %v", seq, err)
			}
			if seq, err := st.GroupReadSeq(ctx, g.ID); err != nil || seq != 5 {
				t.Fatalf("read seq = %d, %v; want 5", seq, err)
			}
			// 回退：返回生效值（5），磁盘不动。
			if seq, err := st.MarkGroupRead(ctx, g.ID, 3); err != nil || seq != 5 {
				t.Fatalf("backward mark = %d, %v; want 5", seq, err)
			}
			if seq, _ := st.GroupReadSeq(ctx, g.ID); seq != 5 {
				t.Fatalf("cursor regressed to %d", seq)
			}
			// 重复同一位置：幂等。
			if seq, err := st.MarkGroupRead(ctx, g.ID, 5); err != nil || seq != 5 {
				t.Fatalf("idempotent mark = %d, %v", seq, err)
			}
			// 参数校验。
			if _, err := st.MarkGroupRead(ctx, g.ID, 0); !errors.Is(err, ErrInvalid) {
				t.Fatalf("seq=0 err = %v, want ErrInvalid", err)
			}
			if _, err := st.MarkGroupRead(ctx, "grp_missing", 1); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing group err = %v, want ErrNotFound", err)
			}
			if _, err := st.GroupReadSeq(ctx, "grp_missing"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing group read = %v, want ErrNotFound", err)
			}
		})
	}
}

// 并发 mark_read：锁内读-改-写，最终值必须是最大值（不回退）。
func TestStoreGroupReadConcurrentMonotonic(t *testing.T) {
	for name, st := range groupReadStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			g := testGroup()
			if err := st.SaveGroup(ctx, g); err != nil {
				t.Fatal(err)
			}
			const n = 32
			var wg sync.WaitGroup
			errs := make(chan error, n)
			for i := 1; i <= n; i++ {
				wg.Add(1)
				go func(seq int64) {
					defer wg.Done()
					if _, err := st.MarkGroupRead(ctx, g.ID, seq); err != nil {
						errs <- err
					}
				}(int64(i))
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			seq, err := st.GroupReadSeq(ctx, g.ID)
			if err != nil {
				t.Fatal(err)
			}
			if seq != n {
				t.Fatalf("final read seq = %d, want %d", seq, n)
			}
		})
	}
}

// 群元数据整份写回（改名/置顶）不能把已读点倒退：SaveGroup 保留更大的游标。
func TestStoreSaveGroupPreservesReadCursor(t *testing.T) {
	for name, st := range groupReadStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			g := testGroup()
			if err := st.SaveGroup(ctx, g); err != nil {
				t.Fatal(err)
			}
			if _, err := st.MarkGroupRead(ctx, g.ID, 9); err != nil {
				t.Fatal(err)
			}
			// 模拟「改名命令在 mark_read 之前加载群」：手里是旧快照（游标 0）。
			stale := testGroup()
			stale.Name = "改名后的群"
			if err := st.SaveGroup(ctx, stale); err != nil {
				t.Fatal(err)
			}
			got, err := st.LoadGroup(ctx, g.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != "改名后的群" {
				t.Fatalf("metadata update lost: %+v", got)
			}
			if got.HumanReadSeq != 9 {
				t.Fatalf("human_read_seq rolled back to %d, want 9", got.HumanReadSeq)
			}
			// 显式带更大游标的保存仍然生效（创建/恢复路径）。
			next := testGroup()
			next.HumanReadSeq = 12
			if err := st.SaveGroup(ctx, next); err != nil {
				t.Fatal(err)
			}
			if seq, _ := st.GroupReadSeq(ctx, g.ID); seq != 12 {
				t.Fatalf("explicit cursor save = %d, want 12", seq)
			}
		})
	}
}

// 向后兼容：旧 group.json 没有 human_read_seq → 0（全未读），序列化时省略。
func TestGroupHumanReadSeqBackwardCompatible(t *testing.T) {
	var g Group
	raw := []byte(`{"id":"grp_legacy","name":"旧群","owners":[],"members":[]}`)
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if g.HumanReadSeq != 0 {
		t.Fatalf("legacy human_read_seq = %d, want 0", g.HumanReadSeq)
	}
	out, err := json.Marshal(Group{ID: "grp_zero"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["human_read_seq"]; ok {
		t.Fatalf("zero cursor must be omitted (backward compatible): %s", out)
	}
}
