package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMobilePairCodeLifecycle(t *testing.T) {
	p := newMobilePairing(filepath.Join(t.TempDir(), "devices.json"))
	now := time.Now()

	code, exp := p.newCode(now)
	if len(code) != 6 {
		t.Fatalf("配对码应为 6 位，得到 %q", code)
	}
	if !exp.After(now) {
		t.Fatal("过期时间应在未来")
	}
	// 未过期时复用同一个码（设置页反复打开不能把码刷掉）
	if again, _ := p.newCode(now); again != code {
		t.Fatalf("未过期应复用：%q != %q", again, code)
	}

	token, deviceID, err := p.pair(code, "iPhone", now)
	if err != nil {
		t.Fatalf("配对失败：%v", err)
	}
	if token == "" || deviceID == "" {
		t.Fatal("token/deviceID 不应为空")
	}
	if id, ok := p.validate(token, now); !ok || id != deviceID {
		t.Fatalf("validate 失败：%q %v", id, ok)
	}

	// 单次使用
	if _, _, err := p.pair(code, "iPad", now); err == nil {
		t.Fatal("同一配对码不应能二次使用")
	}

	// 重新加载（模拟重启）：token 仍有效（存的是 sha256）
	p2 := newMobilePairing(p.path)
	if _, ok := p2.validate(token, now); !ok {
		t.Fatal("重启后 token 应仍有效")
	}
	if !p2.revoke(deviceID) {
		t.Fatal("撤销应成功")
	}
	if _, ok := p2.validate(token, time.Now()); ok {
		t.Fatal("撤销后 token 不应再通过")
	}
}

func TestMobilePairRejectsWrongCodeAndRateLimits(t *testing.T) {
	p := newMobilePairing(filepath.Join(t.TempDir(), "devices.json"))
	now := time.Now()
	code, _ := p.newCode(now)

	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < mobileCodeAttempts; i++ {
		if _, _, err := p.pair(wrong, "", now); err == nil {
			t.Fatalf("第 %d 次错误码不应成功", i+1)
		}
	}
	// 第 6 次：即使码正确也应被限流（先限流后比对，避免用响应差异探测）
	if _, _, err := p.pair(code, "", now); err != errMobileRateLimited {
		t.Fatalf("应被限流，得到 %v", err)
	}
	// 窗口滑过后恢复
	later := now.Add(2 * time.Minute)
	if _, _, err := p.pair(code, "", later); err != nil {
		t.Fatalf("过了限流窗口应可配对：%v", err)
	}
}

func TestMobilePairExpiredCode(t *testing.T) {
	p := newMobilePairing(filepath.Join(t.TempDir(), "devices.json"))
	now := time.Now()
	code, _ := p.newCode(now)
	if _, _, err := p.pair(code, "", now.Add(mobileCodeTTL+time.Second)); err != errMobileCodeInvalid {
		t.Fatalf("过期码应无效，得到 %v", err)
	}
}

func TestWriteMobileAtomicPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	if err := writeMobileAtomic(path, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600，得到 %v", st.Mode().Perm())
	}
}
