package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadMeshSettingsDefault 未配置 settings.json → 默认全关（安全默认）。
func TestLoadMeshSettingsDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 ~/.go-code（无 settings.json）
	ms := loadMeshSettings()
	if ms.SessionEnabled {
		t.Fatal("SessionEnabled should default to false")
	}
	if ms.RemoteEnabled {
		t.Fatal("RemoteEnabled should default to false")
	}
	if ms.Remote.AuthToken != "" {
		t.Fatal("AuthToken should default to empty")
	}
}

// TestLoadMeshSettingsEnabled 合法配置 → 正确解析 remote 段。
func TestLoadMeshSettingsEnabled(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"mesh":{
		"session_enabled": true,
		"remote_enabled": true,
		"remote": {
			"listen_addr": "0.0.0.0:17890",
			"auth_token": "sec",
			"node_id": "node-a",
			"peers": [{"id":"node-b","addr":"ws://127.0.0.1:17891","token":"sec"}]
		}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ms := loadMeshSettings()
	if !ms.SessionEnabled || !ms.RemoteEnabled {
		t.Fatalf("expected both enabled, got session=%v remote=%v", ms.SessionEnabled, ms.RemoteEnabled)
	}
	if ms.Remote.ListenAddr != "0.0.0.0:17890" || ms.Remote.AuthToken != "sec" || ms.Remote.NodeID != "node-a" {
		t.Fatalf("remote settings mismatch: %+v", ms.Remote)
	}
	if len(ms.Remote.Peers) != 1 || ms.Remote.Peers[0].ID != "node-b" || ms.Remote.Peers[0].Addr != "ws://127.0.0.1:17891" {
		t.Fatalf("peers mismatch: %+v", ms.Remote.Peers)
	}
}

// TestLoadMeshSettingsBadJSON 坏配置 → 回默认全关（不 panic）。
func TestLoadMeshSettingsBadJSON(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"mesh": {{{`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ms := loadMeshSettings()
	if ms.SessionEnabled || ms.RemoteEnabled {
		t.Fatal("bad mesh config should fall back to all-disabled")
	}
}

// TestLoadMeshSettingsRemoteDependsOnSession remote_enabled 依赖 session_enabled：
// session 关 + remote 开 → 视为关闭。
func TestLoadMeshSettingsRemoteDependsOnSession(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"mesh":{"session_enabled": false, "remote_enabled": true}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ms := loadMeshSettings()
	if ms.RemoteEnabled {
		t.Fatal("remote_enabled must be forced off when session_enabled is false")
	}
}

// TestLoadMeshSettingsNoMeshSection 无 mesh 段 → 默认全关。
func TestLoadMeshSettingsNoMeshSection(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"cron":{"enabled":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ms := loadMeshSettings()
	if ms.SessionEnabled || ms.RemoteEnabled {
		t.Fatal("missing mesh section should default to all-disabled")
	}
}

// TestLoadMeshSettingsDefaultListenAddr remote 开启但未配 listen_addr → 补默认 127.0.0.1:17890
// （防 Gateway.startServer 把空串当"纯客户端不监听"，导致远程开而本机不可连）。
func TestLoadMeshSettingsDefaultListenAddr(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// remote 开启但 remote 段未配 listen_addr
	cfg := `{"mesh":{"session_enabled":true,"remote_enabled":true,"remote":{"auth_token":"sec"}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ms := loadMeshSettings()
	if ms.Remote.ListenAddr != defaultMeshListenAddr {
		t.Fatalf("want default listen addr %q, got %q", defaultMeshListenAddr, ms.Remote.ListenAddr)
	}
	// 显式配置不被覆盖
	cfg2 := `{"mesh":{"session_enabled":true,"remote_enabled":true,"remote":{"listen_addr":"0.0.0.0:20000"}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(cfg2), 0o644); err != nil {
		t.Fatal(err)
	}
	ms2 := loadMeshSettings()
	if ms2.Remote.ListenAddr != "0.0.0.0:20000" {
		t.Fatalf("explicit listen addr should be kept, got %q", ms2.Remote.ListenAddr)
	}
}
