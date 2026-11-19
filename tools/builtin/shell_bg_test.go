package builtin

// 「命令末尾的 `&`」识别与改写（2026-11-18 资源有界化）。
//
// 钉住两件事：
//  1. **识别要准**（改错 = 执行了用户没批准的语义）⇒ 引号内 / 转义过 /
//     重定向里的 & 都不能当成后台运算符；
//  2. **改写要生效**（不改 = 模型以为服务起来了、其实已经死了 ⇒ 静默失败）。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSplitTrailingBackground(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		wantB bool
	}{
		// 认得出的
		{"npm run dev &", "npm run dev", true},
		{"npm run dev&", "npm run dev", true},
		{"npm run dev & ", "npm run dev", true},
		{"npm run dev & ;", "npm run dev", true},
		{"cd /x && npm run dev &", "cd /x && npm run dev", true},
		{"cmd 2>&1 &", "cmd 2>&1", true},
		{"echo 'a & b' &", "echo 'a & b'", true},
		{"echo \"a & b\" &", "echo \"a & b\"", true},
		{"echo `date &` &", "echo `date &`", true},
		// 认不出的（保守：不改）
		{"npm run dev", "npm run dev", false},
		{"echo \"a & b\"", "echo \"a & b\"", false},
		{"echo 'a & b'", "echo 'a & b'", false},
		{"cmd &> out.log", "cmd &> out.log", false},
		{"cmd >& out.log", "cmd >& out.log", false},
		{"a && b", "a && b", false},
		{"a & b", "a & b", false}, // 中间的 &：a 后台、b 前景 ⇒ 不归我们管
		{"cmd \\&", "cmd \\&", false}, // 转义过的 &
		{"&", "&", false},
		{"cmd&&", "cmd&&", false}, // 末尾的 && 是语法错误，不该由我们"顺手修"
	}
	for _, c := range cases {
		got, ok := splitTrailingBackground(c.in)
		if ok != c.wantB || got != c.want {
			t.Errorf("splitTrailingBackground(%q) = (%q, %v)，期望 (%q, %v)",
				c.in, got, ok, c.want, c.wantB)
		}
	}
}

// TestBashTrailingAmpBecomesBackground：命令末尾的 `&` ⇒ 走后台模式。
//
// 改造前：`cmd &` 成功返回、输出为空 ⇒ 模型以为服务起来了，实际那个进程被
// 「命令结束即收组」杀掉 ⇒ **静默失败**（最难查的一类）。
func TestBashTrailingAmpBecomesBackground(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	tool := NewBashTool(dir, 5*time.Second, nil, nil)

	// 命令：起一个长跑进程（写自己的 pid），末尾带 `&`。
	cmd := "sh -c 'echo $$ > " + pidFile + "; sleep 60' &"
	out, err := tool.Call(context.Background(), "c1", `{"command":`+strconv.Quote(cmd)+`}`)
	if err != nil {
		t.Fatalf("执行失败: %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "started in background") {
		t.Fatalf("末尾的 `&` 没走后台模式: %q", out)
	}
	// 进程必须活着（= 模型的服务真的起来了）。
	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				pid, _ = strconv.Atoi(s)
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("拿不到后台进程 pid")
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("后台进程 %d 没了 ⇒ 服务没起来（静默失败）", pid)
	}
}
