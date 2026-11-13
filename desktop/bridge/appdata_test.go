package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAppDataLayout 守卫 APP 级存储约定：sessions/events 在 ~/.go-code/{sessions|events}/<wsKey>/
// （跨 workspace，工作区下不再落盘），每个 workspace 一个稳定子目录、不同 workspace 不冲突。
func TestAppDataLayout(t *testing.T) {
	appDataDirOverride = t.TempDir()
	t.Cleanup(func() { appDataDirOverride = "" })

	wsA := filepath.Join(t.TempDir(), "proj-a")
	wsB := filepath.Join(t.TempDir(), "proj-b")

	// 落在 APP 级目录内（含 sessions/events 段 + 每个 workspace 一个子目录）
	sA := wsSessionsDir(wsA)
	if !strings.HasPrefix(sA, appDataDir()+string(filepath.Separator)+"sessions"+string(filepath.Separator)) {
		t.Fatalf("sessions dir 应位于 ~/.go-code/sessions/<wsKey>/，got %s", sA)
	}
	if eA := wsEventsDir(wsA); !strings.HasPrefix(eA, appDataDir()+string(filepath.Separator)+"events"+string(filepath.Separator)) {
		t.Fatalf("events dir 应位于 ~/.go-code/events/<wsKey>/，got %s", eA)
	}

	// 同一 workspace 子目录稳定；不同 workspace 子目录不同（不共享/不冲突）
	if wsSessionsDir(wsA) != sA || wsEventsDir(wsA) != wsEventsDir(wsA) {
		t.Fatal("同一 workspace 应映射到稳定子目录")
	}
	if wsSessionsDir(wsA) == wsSessionsDir(wsB) || wsEventsDir(wsA) == wsEventsDir(wsB) {
		t.Fatal("不同 workspace 应映射到不同子目录")
	}

	// 子目录名安全：不含路径分隔符（可直接作为单层目录名）
	for _, k := range []string{workspaceKey(wsA), workspaceKey(wsB)} {
		if strings.ContainsAny(k, `/\`) {
			t.Fatalf("workspaceKey 不应含路径分隔符: %q", k)
		}
	}
}
