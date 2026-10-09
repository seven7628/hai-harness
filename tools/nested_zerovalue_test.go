package tools

import (
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 零值 NestedRecorder（未经 NewNestedRecorder）并发 AddUsage —— box() 的惰性
// 初始化必须在锁内。初稿在锁外写 r.usageBox，-race 报 DATA RACE。
//
// 另覆盖 Add/Stats/TakeRecord 与 AddUsage 混合调用（锁序 r.mu → box.mu）。
func TestZeroValueRecorderConcurrentAddUsage(t *testing.T) {
	var rec core.NestedRecorder // 零值，usageBox == nil
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); rec.AddUsage(core.Usage{Input: 1}) }()
	}
	wg.Wait()
	if got := rec.Usage().Input; got != 50 {
		t.Fatalf("Input = %d, want 50 (zero-value recorder must not lose usage)", got)
	}
}
