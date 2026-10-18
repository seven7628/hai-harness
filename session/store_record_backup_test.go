package session

import (
	"context"
	"os"
	"testing"
)

// 硬链接备份（.bak 零拷贝）的关键不变量。
//
// 提交路径用 os.Link(path, .bak) 代替「复制 90MB 再写盘 + fsync」，
// 在 91MB record 上省掉 ~138ms 与一半写入量。代价是 .bak 与旧 formal 共享
// inode —— 因此必须证明：
//  1. .bak 内容始终等于「上一次成功提交」的内容（不被新内容污染）；
//  2. .bak 不等于当前 formal（否则备份形同虚设）。
// 后续 rename 会把 path 换成新 inode，旧 inode 只剩 .bak 一个名字。
//
// 原注释：
// 不能被新提交污染（否则回退时会拿到半新半旧的数据）。
func TestHardlinkBackupIsolation(t *testing.T) {
	s, _ := NewFileStorage(t.TempDir())
	ctx := context.Background()

	// 三次提交，每次都应有独立的 .bak 内容
	var prevContents []string
	for i := 1; i <= 3; i++ {
		rec := testSessionRecord(t, "ws", "s1", uint64(i))
		if err := s.CommitRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
		p, _ := s.RecordPath("ws", "s1")
		cur, _ := os.ReadFile(p)
		prevContents = append(prevContents, string(cur))

		if i >= 2 {
			bak, err := os.ReadFile(p + ".bak")
			if err != nil {
				t.Fatalf("第 %d 次提交后 .bak 不存在: %v", i, err)
			}
			// .bak 必须等于**上一次**提交的内容
			if string(bak) != prevContents[i-2] {
				t.Fatalf("第 %d 次提交后 .bak 内容不是上一次的:\n got rev 前缀=%q\nwant rev 前缀=%q",
					i, string(bak[:min2(80, len(bak))]), string(prevContents[i-2][:min2(80, len(prevContents[i-2]))]))
			}
			// .bak 不能等于当前 formal（否则链接没生效或污染）
			if string(bak) == string(cur) {
				t.Fatalf("第 %d 次提交后 .bak 与 formal 相同（备份未生效）", i)
			}
		}
	}
}

func min2(a, b int) int { if a < b { return a }; return b }
