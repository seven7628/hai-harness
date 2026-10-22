package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// 回归：main.go 用固定地址 "127.0.0.1:47800" 调 start()，曾把 config.json 的 lan
// 完全覆盖 —— 用户开了局域网，重启后又只剩回环，手机连不上，只能再点一次面板开关。
// 约定：preferred 只提供端口，绑定接口由 lan 决定。
func TestMobileStartHonoursLanConfig(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "mobile")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCfg := func(lan bool) {
		t.Helper()
		b := []byte(`{"lan": false}`)
		if lan {
			b = []byte(`{"lan": true}`)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		lan  bool
		want string
	}{
		{"lan=false 绑回环", false, "127.0.0.1"},
		{"lan=true 绑全接口", true, "0.0.0.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeCfg(c.lan)
			s := newMobileServer(nil, dir)
			// 传回环地址（与 main.go 的调用一致）；端口用 0 以免和本机在跑的 bridge 抢
			if err := s.start("127.0.0.1:0"); err != nil {
				t.Fatalf("start: %v", err)
			}
			defer s.close()

			host, _, err := net.SplitHostPort(s.addr)
			if err != nil {
				t.Fatalf("addr %q: %v", s.addr, err)
			}
			// 绑 0.0.0.0 时 Go 可能报 [::]（双栈），两者都算「全接口」
			if c.lan {
				ip := net.ParseIP(host)
				if ip == nil || !ip.IsUnspecified() {
					t.Fatalf("lan=true 应绑全接口，实际 %s", s.addr)
				}
				return
			}
			if host != c.want {
				t.Fatalf("lan=%v 应绑 %s，实际 %s", c.lan, c.want, s.addr)
			}
		})
	}
}
