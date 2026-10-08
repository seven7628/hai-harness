package codemode

// store_test.go：会话级 store 的回归网。硬要求各有钉子：
//   - 限额边界（单值 256 KiB / 总量 1 MiB）；
//   - 违法 JSON 一律拒绝（暂存时就拒，落盘前再验一遍）；
//   - 文件权限 0600；
//   - 失败脚本不写盘（由调用方决定的契约：不调 Commit 就不写）；
//   - 损坏行（半行 JSON）的 replay 行为（跳过 + 计数 + 下次追加前补换行）与**理由**。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// openTemp 建一个临时会话目录里的 store（模拟 <session dir>/codemode-store.jsonl）。
func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := StorePath(t.TempDir())
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s, path
}

// commit 暂存一批写入并提交（成功脚本的路径）。
func commit(t *testing.T, s *Store, kv map[string]string) {
	t.Helper()
	p := NewPending()
	for k, v := range kv {
		if err := p.Set(k, json.RawMessage(v)); err != nil {
			t.Fatalf("Pending.Set(%q, %s): %v", k, v, err)
		}
	}
	if err := s.Commit(p); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// pending 造一个只含一对 set 的待落盘批次（顺序稳定：单条）。
func pending(t *testing.T, key string, value json.RawMessage) *Pending {
	t.Helper()
	p := NewPending()
	if err := p.Set(key, value); err != nil {
		t.Fatalf("Pending.Set: %v", err)
	}
	return p
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(b)
}

func valuesOf(t *testing.T, s *Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, v := range s.Values() {
		out[k] = string(v)
	}
	return out
}

// jsonStringOfSize 造一个**恰好** n 字节的 JSON 字符串字面量（含两侧引号）。
func jsonStringOfSize(n int) json.RawMessage {
	if n < 2 {
		panic("jsonStringOfSize: n 至少 2（两个引号）")
	}
	return json.RawMessage(`"` + strings.Repeat("a", n-2) + `"`)
}

// ---- 基本写入与整文件 replay ----

// TestStoreCommitAndReplay 一次成功脚本的写入批 → 一行 jsonl → 重开进程后整文件 replay。
func TestStoreCommitAndReplay(t *testing.T) {
	s, path := openTemp(t)
	if err := s.Commit(nil); err != nil { // nil Pending = 空批，合法且不写盘
		t.Fatalf("Commit(nil): %v", err)
	}
	if got := readFile(t, path); got != "" {
		t.Fatalf("空批不该写盘，文件内容 %q", got)
	}
	commit(t, s, map[string]string{"answer": "42"})
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if got := valuesOf(t, s2); got["answer"] != "42" {
		t.Fatalf("replay 后 answer = %q, want 42（全部: %v）", got["answer"], got)
	}
	// 行形状：{set:{k:raw},delete:[...]}（设计 §16.5），键序稳定、值原样保留。
	p := NewPending()
	if err := p.Set("obj", json.RawMessage(`{"b":1,"a":[1,2]}`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := p.Delete("answer"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s2.Commit(p); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(readFile(t, path), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 行 jsonl，实际 %d 行: %q", len(lines), lines)
	}
	if lines[0] != `{"set":{"answer":42}}` {
		t.Errorf("第一行 = %s, want {\"set\":{\"answer\":42}}", lines[0])
	}
	if lines[1] != `{"set":{"obj":{"b":1,"a":[1,2]}},"delete":["answer"]}` {
		t.Errorf("第二行 = %s（键序必须稳定，值必须原样保留）", lines[1])
	}
	if got := valuesOf(t, s2); len(got) != 1 || got["obj"] != `{"b":1,"a":[1,2]}` {
		t.Errorf("删除后状态 = %v, want 只有 obj", got)
	}
}

// TestStoreCompactsValues 值里的空白（含换行）必须被压紧：一行 jsonl 里出现换行会把
// 记录劈成两行，整文件 replay 时**两条都读不出来**。
func TestStoreCompactsValues(t *testing.T) {
	s, path := openTemp(t)
	commit(t, s, map[string]string{"pretty": "{\n  \"a\": 1,\n  \"b\": [1, 2]\n}"})
	content := readFile(t, path)
	if strings.Count(content, "\n") != 1 {
		t.Fatalf("一次写入应当只落一行，实际 %d 行:\n%q", strings.Count(content, "\n"), content)
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if got := valuesOf(t, s2)["pretty"]; got != `{"a":1,"b":[1,2]}` {
		t.Errorf("压紧后的值 = %q", got)
	}
	if st := s2.Stats(); st.Applied != 1 || st.Skipped != 0 {
		t.Errorf("Stats = %+v, want 1 applied / 0 skipped", st)
	}
}

// ---- 限额 ----

func TestStoreLimits(t *testing.T) {
	// 记账口径（对抗复核 F6 修正）：总量 = Σ(键字节 + 值字节)。所以「恰好顶到 1 MiB」
	// 的值尺寸要扣掉键的开销 —— 4 个 1 字节的键 ⇒ 每个值 262143 字节时总量恰为 1048576。
	const keyCount = 4
	const exactFitValue = (MaxStoreTotalBytes - keyCount) / keyCount // 262143

	// seedFull 预置一个「总量恰好顶到 1 MiB」的 store。
	seedFull := func(t *testing.T, s *Store) {
		t.Helper()
		p := NewPending()
		for _, k := range []string{"a", "b", "c", "d"} {
			if err := p.Set(k, jsonStringOfSize(exactFitValue)); err != nil {
				t.Fatalf("预置: %v", err)
			}
		}
		if err := s.Commit(p); err != nil {
			t.Fatalf("预置满店失败: %v", err)
		}
	}
	noop := func(*testing.T, *Store) {}
	cases := []struct {
		name    string
		seed    func(*testing.T, *Store)
		write   func(*testing.T, *Store) error
		wantErr error
	}{
		{
			name: "单值恰好 256 KiB（合法）",
			seed: noop,
			write: func(t *testing.T, s *Store) error {
				return s.Commit(pending(t, "big", jsonStringOfSize(MaxStoreValueBytes)))
			},
		},
		{
			name: "单值 256 KiB + 1 字节（拒绝）",
			seed: noop,
			write: func(t *testing.T, s *Store) error {
				// 暂存路径先拒（脚本内就能看到错误，不用等脚本跑完）。
				if err := NewPending().Set("big", jsonStringOfSize(MaxStoreValueBytes+1)); !errors.Is(err, ErrStoreValueTooLarge) {
					t.Errorf("Pending.Set 应当先拒，实际 %v", err)
				}
				// 落盘前的第二道闸（白盒：绕过 Pending 自拼记录也必须被拦）。
				return s.appendRecord(StoreRecord{Set: map[string]json.RawMessage{"big": jsonStringOfSize(MaxStoreValueBytes + 1)}})
			},
			wantErr: ErrStoreValueTooLarge,
		},
		{
			name: "键 1 KiB + 1 字节（拒绝）",
			seed: noop,
			write: func(t *testing.T, s *Store) error {
				// 暂存路径先拒（脚本内可 catch）。
				if err := NewPending().Set(strings.Repeat("k", MaxStoreKeyBytes+1), json.RawMessage("1")); !errors.Is(err, ErrStoreKeyTooLarge) {
					t.Errorf("Pending.Set 应当先拒超长键，实际 %v", err)
				}
				// 落盘前的第二道闸（白盒）。
				return s.appendRecord(StoreRecord{Set: map[string]json.RawMessage{strings.Repeat("k", MaxStoreKeyBytes+1): json.RawMessage("1")}})
			},
			wantErr: ErrStoreKeyTooLarge,
		},
		{
			name: "总量恰好 1 MiB（键+值，合法）",
			seed: noop,
			write: func(t *testing.T, s *Store) error {
				p := NewPending()
				for _, k := range []string{"a", "b", "c", "d"} {
					if err := p.Set(k, jsonStringOfSize(exactFitValue)); err != nil {
						return err
					}
				}
				return s.Commit(p)
			},
		},
		{
			name: "总量 1 MiB + 1 字节（拒绝）",
			seed: seedFull,
			write: func(t *testing.T, s *Store) error {
				p := NewPending()
				for _, k := range []string{"a", "b", "c", "d"} {
					if err := p.Set(k, jsonStringOfSize(exactFitValue)); err != nil {
						return err
					}
				}
				if err := p.Set("e", json.RawMessage("1")); err != nil { // 再来 1 键 + 1 字节就超
					return err
				}
				return s.Commit(p)
			},
			wantErr: ErrStoreTotalTooLarge,
		},
		{
			name: "总量超限可被删除救回（先删再写）",
			seed: seedFull,
			write: func(t *testing.T, s *Store) error {
				p := NewPending()
				for _, k := range []string{"a", "b", "c"} {
					if err := p.Delete(k); err != nil {
						return err
					}
				}
				if err := p.Set("e", json.RawMessage("1")); err != nil {
					return err
				}
				return s.Commit(p)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, path := openTemp(t)
			c.seed(t, s)
			before := readFile(t, path)
			beforeKeys := len(valuesOf(t, s))
			err := c.write(t, s)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("应当成功，实际 %v", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("错误 = %v, want errors.Is(%v)", err, c.wantErr)
			}
			// 校验失败必须**一个字节都不写**、内存状态不变。
			if got := readFile(t, path); got != before {
				t.Error("限额校验失败时文件被改动了（半批写入）")
			}
			if got := len(valuesOf(t, s)); got != beforeKeys {
				t.Error("限额校验失败时内存状态被改动了")
			}
			// 报错必须能让模型看懂限额（它会照着这句改脚本），所以带上具体数字与建议。
			wantText := map[error]string{
				ErrStoreValueTooLarge: "256 KiB",
				ErrStoreKeyTooLarge:   "1 KiB",
				ErrStoreTotalTooLarge: "1 MiB",
			}[c.wantErr]
			if !strings.Contains(err.Error(), wantText) {
				t.Errorf("报错里缺少 %q: %v", wantText, err)
			}
		})
	}
}

// TestStoreTotalCountsKeys 键的字节必须计入总量。
//
// 为什么单列（对抗复核 F6）：只算值时，脚本能用大量长键把 store 撑到任意大 ——
// 实测 3000 个 1 KiB 的键 = 3 MB 落盘，且此后**每次调用**都带 3 MB 的 init 载荷。
func TestStoreTotalCountsKeys(t *testing.T) {
	t.Parallel()
	s, path := openTemp(t)
	p := NewPending()
	prefix := strings.Repeat("k", MaxStoreKeyBytes-4) // 1 KiB 键：前缀 + 4 位序号 = 恰好 1024
	for i := 0; i < 3000; i++ {
		if err := p.Set(fmt.Sprintf("%s%04d", prefix, i), json.RawMessage("1")); err != nil {
			t.Fatalf("第 %d 个键不该被拒（键恰好 1 KiB、值 1 字节）: %v", i, err)
		}
	}
	err := s.Commit(p)
	if !errors.Is(err, ErrStoreTotalTooLarge) {
		t.Fatalf("3000 × 1 KiB 的键把总量顶到 ~3 MB，必须被总量闸拒绝，实际 err=%v", err)
	}
	if got := readFile(t, path); got != "" {
		t.Errorf("被拒的批次不该写进文件，实际 %d 字节", len(got))
	}
	if got := len(valuesOf(t, s)); got != 0 {
		t.Errorf("被拒的批次不该改内存状态，实际 %d 个键", got)
	}
}

// ---- 违法 JSON ----

func TestStoreRejectsInvalidJSON(t *testing.T) {
	s, path := openTemp(t)
	p := NewPending()
	cases := []struct{ name, value string }{
		{"半截 JSON", `{"a":`},
		{"不是 JSON", `hello`},
		{"空值（删除要用 Delete 表达）", ``},
		{"未闭合字符串", `"abc`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := p.Set("k", json.RawMessage(c.value)); !errors.Is(err, ErrStoreInvalidJSON) {
				t.Errorf("Pending.Set(_, %q) = %v, want ErrStoreInvalidJSON", c.value, err)
			}
		})
	}
	if err := p.Set("", json.RawMessage(`1`)); err == nil {
		t.Error("空键应当拒绝")
	}
	if err := s.Commit(p); err != nil {
		t.Fatalf("被拒的写入不该进入 Pending（这时的 Commit 应当是空批 no-op）: %v", err)
	}
	if got := readFile(t, path); got != "" {
		t.Errorf("没有任何合法写入时文件必须为空，实际 %q", got)
	}
	// 白盒：绕过 Pending 直接落一条坏记录（Wave 2 若自拼 StoreRecord 也走这条校验）。
	if err := s.appendRecord(StoreRecord{Set: map[string]json.RawMessage{"k": []byte(`{"a":`)}}); !errors.Is(err, ErrStoreInvalidJSON) {
		t.Errorf("appendRecord(坏值) = %v, want ErrStoreInvalidJSON", err)
	}
	if got := readFile(t, path); got != "" {
		t.Errorf("坏记录不得落盘，实际 %q", got)
	}
	if err := s.appendRecord(StoreRecord{Set: map[string]json.RawMessage{"": []byte(`1`)}}); err == nil {
		t.Error("空键应当拒绝（白盒路径）")
	}
}

// ---- 权限 ----

func TestStoreFileMode0600(t *testing.T) {
	path := StorePath(t.TempDir())
	if _, err := OpenStore(path); err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	assertMode0600(t, path)

	// 已存在但权限更宽（老版本/别的工具建的）→ 打开时收紧。
	loose := StorePath(t.TempDir())
	if err := os.WriteFile(loose, []byte(`{"set":{"a":1}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := OpenStore(loose)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	assertMode0600(t, loose)
	if got := valuesOf(t, s)["a"]; got != "1" {
		t.Errorf("收紧权限不该影响 replay: a = %q", got)
	}
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("%s 权限 = %o, want 600", path, got)
	}
}

// ---- 失败脚本不写盘（契约）----

// TestStoreFailedScriptDoesNotPersist 「仅脚本成功时追加」的钉子。
//
// 契约（调用方 = Wave 2 的 tool.go）：桥接把沙箱内 store() 调用暂存进 Pending；
// 脚本成功 → Commit(p)；脚本失败/超时/取消/审批被拒 → **丢弃 p，不调 Commit**。
// 本类型没有任何自动落盘路径，所以「失败不写盘」就是默认行为，调用方不需要写分支。
func TestStoreFailedScriptDoesNotPersist(t *testing.T) {
	s, path := openTemp(t)
	commit(t, s, map[string]string{"kept": "1"}) // 先有一次成功脚本，留下基线
	baseline := readFile(t, path)

	// 第二次脚本写了一半就失败：调用方按契约把整个 Pending 丢掉。
	failed := NewPending()
	if err := failed.Set("ok_but_script_failed", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := failed.Set("bad", json.RawMessage(`{"half":`)); !errors.Is(err, ErrStoreInvalidJSON) {
		t.Errorf("非法值应在暂存时就被拒: %v", err)
	}
	if failed.Len() != 1 {
		t.Errorf("被拒的写入不该进入批次，Len = %d, want 1", failed.Len())
	}
	// —— 脚本失败：不调 Commit ——
	if got := readFile(t, path); got != baseline {
		t.Fatalf("失败脚本改动了 store 文件:\n%q\n%q", baseline, got)
	}
	if got := valuesOf(t, s); len(got) != 1 || got["kept"] != "1" {
		t.Fatalf("失败脚本改动了内存状态: %v", got)
	}

	// 成功但没有 store 调用的脚本同样不写盘（否则每次调用都白长一行）。
	if err := s.Commit(NewPending()); err != nil {
		t.Fatalf("Commit(空批): %v", err)
	}
	if got := readFile(t, path); got != baseline {
		t.Fatalf("空批写盘了:\n%q\n%q", baseline, got)
	}
}

// ---- 损坏行 replay ----

// TestStoreReplaySkipsCorruptLines replay 遇到损坏行 = **跳过 + 计数**，不报错。
//
// 理由：最可能的成因是上次写盘中途崩溃留下的半行（append-only 没有多行原子性），
// 而 store 是会话级草稿纸 —— 一条坏行不该让整个会话的 codemode 永久不可用
// （报错 = 之后每次调用都失败，用户还无从修复）。静默跳过也不行，所以计数留在
// Stats() 里，Wave 2 可以据此告警。
func TestStoreReplaySkipsCorruptLines(t *testing.T) {
	path := StorePath(t.TempDir())
	content := strings.Join([]string{
		`{"set":{"a":1}}`,
		`{"set":{"b":`,       // 崩溃留下的半行
		`not json at all`,    // 完全不是 JSON
		`{"set":{"c":3}}`,    // 好行
		`{}`,                 // 合法 JSON 但没有语义
		`{"set":{"d":"bad}}`, // 未闭合字符串
		`{"set":{"e":5`,      // 末尾半行且**没有换行**
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore 不该因为坏行失败: %v", err)
	}
	got := valuesOf(t, s)
	if len(got) != 2 || got["a"] != "1" || got["c"] != "3" {
		t.Fatalf("replay 结果 = %v, want a/c（好行必须照常 apply）", got)
	}
	if st := s.Stats(); st.Lines != 7 || st.Applied != 2 || st.Skipped != 5 {
		t.Errorf("Stats = %+v, want {Lines:7 Applied:2 Skipped:5}（跳过必须可见）", st)
	}

	// 坏行之后仍能写：追加前先补换行，否则新记录会和末尾半行粘成一行，两条都读不出来。
	if err := s.Commit(pending(t, "f", json.RawMessage(`6`))); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	raw := readFile(t, path)
	if !strings.HasSuffix(raw, "\n") {
		t.Errorf("追加后文件必须以换行结尾: %q", raw)
	}
	if !strings.Contains(raw, "\n{\"set\":{\"f\":6}}\n") {
		t.Errorf("新记录必须自成一行（半行修补失败）:\n%q", raw)
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	got2 := valuesOf(t, s2)
	if got2["f"] != "6" || got2["a"] != "1" || got2["c"] != "3" {
		t.Fatalf("修复后 replay = %v, want a/c/f", got2)
	}
	if st := s2.Stats(); st.Applied != 3 || st.Skipped != 5 {
		t.Errorf("Stats = %+v, want {Applied:3 Skipped:5}", st)
	}
}

// TestStoreReplayDoesNotEnforceLimits 历史文件不该因为限额后来调小就整段读不出来
// （读不出来比读多了更糟）。限额是**写入路径**上的闸门。
func TestStoreReplayDoesNotEnforceLimits(t *testing.T) {
	path := StorePath(t.TempDir())
	big := jsonStringOfSize(MaxStoreValueBytes + 1024) // 超过当前单值上限
	line, err := json.Marshal(StoreRecord{Set: map[string]json.RawMessage{"legacy": big}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if got := valuesOf(t, s)["legacy"]; len(got) != len(big) {
		t.Fatalf("历史超限值应当照常 replay（长度 %d, want %d）", len(got), len(big))
	}
	// 但它**不该**再写一次（写入路径依然受限额约束）。
	if err := s.appendRecord(StoreRecord{Set: map[string]json.RawMessage{"legacy": big}}); !errors.Is(err, ErrStoreValueTooLarge) {
		t.Errorf("写入路径必须仍然拦截超限值，实际 %v", err)
	}
	if got := valuesOf(t, s)["legacy"]; len(got) != len(big) {
		t.Error("被拒的写入不该影响内存状态")
	}
}

// ---- 其它边界 ----

func TestStoreDeleteSemantics(t *testing.T) {
	s, path := openTemp(t)
	commit(t, s, map[string]string{"a": "1", "b": "2"})
	// 同键 set 后 delete（= store(k, undefined)）→ 以删除为准，记录里不该再有该键的 set。
	p := NewPending()
	if err := p.Set("a", json.RawMessage(`{"v":2}`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := p.Delete("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := p.Delete("c"); err != nil { // 删不存在的键 = no-op
		t.Fatalf("Delete: %v", err)
	}
	if err := p.Delete(""); err == nil {
		t.Error("空键应当拒绝（Delete 路径）")
	}
	if p.Len() != 2 {
		t.Errorf("Len = %d, want 2（一次删除 + 一次 no-op 删除）", p.Len())
	}
	if rec := p.Record(); len(rec.Set) != 0 {
		t.Errorf("同键的后一次 delete 应当撤销 set，实际 Set = %v", rec.Set)
	}
	if err := s.Commit(p); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	s2, _ := OpenStore(path)
	got := valuesOf(t, s2)
	if _, ok := got["a"]; ok {
		t.Errorf("store(a, undefined) 之后 a 不该还在: %v", got)
	}
	if got["b"] != "2" {
		t.Errorf("b = %q, want 2", got["b"])
	}
}

// TestPendingLastWriteWins Pending 内部保证「同键最后一次操作生效」：delete 之后又
// set，则撤销那次 delete —— 一行 jsonl 里不会出现「既 set 又 delete 同一个键」的歧义。
func TestPendingLastWriteWins(t *testing.T) {
	p := NewPending()
	if err := p.Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := p.Set("k", json.RawMessage(`7`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	rec := p.Record()
	if _, ok := rec.Set["k"]; !ok {
		t.Errorf("Set 应当生效: %+v", rec)
	}
	if len(rec.Delete) != 0 {
		t.Errorf("后到的 Set 应当撤销先前的 Delete，实际 Delete = %v", rec.Delete)
	}
	if p.Len() != 1 {
		t.Errorf("Len = %d, want 1", p.Len())
	}
}

// TestPendingZeroValue 零值/nil Pending 不得 panic（宿主可能拿它当「无写入」的默认值）。
func TestPendingZeroValue(t *testing.T) {
	var nilPending *Pending
	if !nilPending.Empty() {
		t.Error("nil Pending 应当是空批")
	}
	if nilPending.Len() != 0 {
		t.Error("nil Pending 的长度应当是 0")
	}
	if rec := nilPending.Record(); len(rec.Set) != 0 || len(rec.Delete) != 0 {
		t.Errorf("nil Pending 的记录应当是空: %+v", rec)
	}
	var zero Pending // 零值：map 未初始化
	if !zero.Empty() || zero.Len() != 0 {
		t.Error("零值 Pending 应当是空批")
	}
	if rec := zero.Record(); len(rec.Set) != 0 || len(rec.Delete) != 0 {
		t.Errorf("零值 Pending 的记录应当是空: %+v", rec)
	}
	if err := NewPending().Set("k", json.RawMessage("1")); err != nil {
		t.Fatalf("Set: %v", err)
	}
}

func TestStoreValuesSnapshotIsCopy(t *testing.T) {
	s, _ := openTemp(t)
	commit(t, s, map[string]string{"k": `{"a":1}`})
	snap := s.Values()
	snap["k"][0] = 'X'
	snap["injected"] = json.RawMessage(`1`)
	delete(snap, "k")
	fresh := s.Values()
	if string(fresh["k"]) != `{"a":1}` {
		t.Errorf("Values() 返回的不是副本，内部状态被调用方改了: %v", fresh)
	}
	if _, ok := fresh["injected"]; ok {
		t.Error("Values() 返回的 map 与内部共用")
	}
}

func TestStoreMemoryOnly(t *testing.T) {
	s, err := OpenStore("")
	if err != nil {
		t.Fatalf("OpenStore(\"\"): %v", err)
	}
	if s.Path() != "" {
		t.Errorf("Path = %q, want 空（纯内存）", s.Path())
	}
	if err := s.Commit(pending(t, "k", json.RawMessage(`[1,2]`))); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := valuesOf(t, s); got["k"] != `[1,2]` {
		t.Errorf("内存 store 写入后 = %v", got)
	}
	if st := s.Stats(); st != (ReplayStats{}) {
		t.Errorf("内存 store 的 Stats = %+v, want 零值", st)
	}
}

func TestStoreMissingParentDir(t *testing.T) {
	_, err := OpenStore(filepath.Join(t.TempDir(), "nope", StoreFileName))
	if err == nil {
		t.Fatal("父目录不存在应当报错（store 不负责建目录）")
	}
	if !strings.Contains(err.Error(), "目录不存在") {
		t.Errorf("错误文案应当点明目录问题: %v", err)
	}
}

func TestStorePathHelper(t *testing.T) {
	if got := StorePath(""); got != "" {
		t.Errorf("StorePath(\"\") = %q, want 空", got)
	}
	if got := StorePath("/tmp/sess"); got != filepath.Join("/tmp/sess", StoreFileName) {
		t.Errorf("StorePath = %q", got)
	}
}

// TestStoreConcurrentCommits 并发追加不得互相撕行（-race 同时检查数据竞争）。
func TestStoreConcurrentCommits(t *testing.T) {
	s, path := openTemp(t)
	const writers, perWriter = 8, 20
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				p := NewPending()
				if err := p.Set(fmt.Sprintf("k%d_%d", w, i), json.RawMessage(`1`)); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
				if err := s.Commit(p); err != nil {
					t.Errorf("Commit: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	want := writers * perWriter
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	st := s2.Stats()
	if st.Skipped != 0 {
		t.Fatalf("并发追加产生了损坏行（Stats = %+v）:\n%s", st, readFile(t, path))
	}
	if got := len(s2.Values()); got != want {
		t.Errorf("键数 = %d, want %d", got, want)
	}
	if st.Applied != want {
		t.Errorf("Applied = %d, want %d（每一行都该是独立记录的完整一行）", st.Applied, want)
	}
}
