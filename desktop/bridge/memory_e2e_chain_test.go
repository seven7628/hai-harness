package main

// Project Memory 的端到端验收：**生产装配**（newSessionEngine）下的完整链。
//
// # 为什么需要这一份
//
// 上一轮"Step 3/4 接线齐全、单元测试全绿"，而整条链在真实会话里**恒不触发**——
// 因为每个测试都手工往 FileTools 上装钩子，"生产忘了装钩子"这类失效完全测不出来
// （memory_assembly_test.go 的注释记着这件事）。那次的教训是：
// **每个零件都在 ≠ 整条链会转**。
//
// 这次两项新能力（`/memory` 浏览、Saved/Recalled 活动事件）此前只有单测 + tsc，
// 同样没有端到端。工具调用 → 钩子 → 事件 → 浏览清单，这四段一环都没跑通过。
//
// # 为什么不联网
//
// 用真实的 memoryHost 钩子 + 真实的工具调用，但**不跑 LLM**：模型行为由别处验证
// （memory_p0_test.go 的真实 provider 跑批）。这里要验的是"宿主接线通不通"，
// 它是确定性的，不该烧配额、不该受模型波动影响——上一轮跑批 5 次里有 1 次 flake，
// 白等 6 分钟，而那种波动对"钩子装没装"毫无信息量。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools"
)

// memE2E 一次端到端演练的现场。
type memE2E struct {
	eng      *tools.Engine
	cfg      memory.Config
	dir      *memory.Dir
	ws       string
	m        *manager
	buf      *syncBuf
	home     string
	activity []*events.MemoryActivity
}

// newMemE2E 起一个**生产装配**的工具引擎，并解析出记忆目录。
func newMemE2E(t *testing.T) *memE2E {
	t.Helper()
	// manager **先**建：它会 t.Setenv("HOME", 自己的临时目录)。我们随后按这个 HOME
	// 显式指定记忆根，于是引擎侧（prodEngine 拿显式 cfg）与命令侧（browse 走
	// m.memoryConfig() → $HOME/.go-code/memory）落在**同一个**目录。
	//
	// 顺序反了就会两边解析到不同目录，而症状是「browse 返回空清单」——看起来像
	// 列举逻辑坏了，实际只是配置源不一致。（prodEngine 内部还会再建一次 manager、
	// 再改一次 HOME，但它拿到的是显式 cfg，不受影响。）
	m, buf := testManager(t)
	cfg := memory.Config{Directory: filepath.Join(os.Getenv("HOME"), ".go-code", "memory")}

	ws := t.TempDir()
	initGitRepo(t, ws)
	home := os.Getenv("HOME")
	e := &memE2E{eng: prodEngine(t, ws, cfg), cfg: cfg, ws: ws, m: m, buf: buf, home: home}

	res := memory.Bootstrap(context.Background(), ws, cfg)
	if !res.Resolved || res.Dir == nil {
		t.Skipf("本机无法派生记忆目录（%v），跳过", res.Err)
	}
	// memory 包不建目录（官方没有"启动即建目录"），首次写入时由 write_file 建。
	if err := os.MkdirAll(res.Dir.Path, 0o700); err != nil {
		t.Fatalf("建记忆目录: %v", err)
	}
	e.dir = res.Dir
	return e
}

// browse 通过**命令**读清单（而不是直接调 cfg.List）：这条命令走的是
// m.memoryConfig()，它**不带 Directory**，根目录取自 $HOME。所以派发前必须把 HOME
// 摆回构造时的值 —— prodEngine 内部的 manager 会 t.Setenv 改掉它。
func (e *memE2E) browse(t *testing.T) *memory.Listing {
	t.Helper()
	t.Setenv("HOME", e.home)
	e.m.memPrefs = &memoryPrefStore{path: filepath.Join(t.TempDir(), "memory.json")}
	e.m.dispatch(cmd(0, "memory_browse", map[string]any{"workspace": e.ws}))
	return browseListing(t, e.buf)
}

// ctx 返回一个带工具上下文的 ctx：钩子靠它拿 RunId 与 Handler 发活动事件。
func (e *memE2E) ctx(t *testing.T) context.Context {
	t.Helper()
	h := events.EventHandler(func(_ context.Context, ev events.Event) {
		if a, ok := ev.(*events.MemoryActivity); ok {
			e.activity = append(e.activity, a)
		}
	})
	return events.WithToolContextForRun(context.Background(), "run-e2e", "", 0, h, nil, nil)
}

func (e *memE2E) call(t *testing.T, ctx context.Context, tool, args string) string {
	t.Helper()
	tl, err := e.eng.GetTool(ctx, tool)
	if err != nil {
		t.Fatalf("%s 未注册: %v", tool, err)
	}
	if err := tl.ValidParams(ctx, tool, args); err != nil {
		t.Fatalf("%s 参数校验失败: %v", tool, err)
	}
	res, err := tl.Call(ctx, tool, args)
	if err != nil {
		t.Fatalf("%s 调用失败: %v", tool, err)
	}
	return res
}

// TestMemoryEndToEndWriteThenBrowse 是主链的端到端验收：
// 写 topic → 补 modified + saved 事件 → 写索引 → 读 topic → recalled 事件
// → browse 命令把两个文件都列出来。
//
// 一次跑完五段，是因为它们本来就是一个连续过程；拆成五个测试反而看不出
// 「前一段坏了后一段照样绿」。
func TestMemoryEndToEndWriteThenBrowse(t *testing.T) {
	e := newMemE2E(t)
	ctx := e.ctx(t)

	// ① 写 topic（带 frontmatter，官方语义）
	topic := filepath.Join(e.dir.Path, "relay_tunnel_id.md")
	body := "---\ntype: project\n---\n\nThe identifier is `q7-tunnel-corp`.\n"
	e.call(t, ctx, "write_file",
		`{"path":`+strconv.Quote(topic)+`,"content":`+strconv.Quote(body)+`}`)

	got, err := os.ReadFile(topic)
	if err != nil {
		t.Fatalf("读回 topic: %v", err)
	}
	if !strings.Contains(string(got), "modified:") {
		t.Fatalf("生产装配未补 modified —— Step 4 钩子没接上\n%s", got)
	}
	if !strings.Contains(string(got), "q7-tunnel-corp") {
		t.Fatalf("正文被改写\n%s", got)
	}

	// ② saved 活动事件
	if len(e.activity) != 1 || e.activity[0].Action != events.MemoryActionSaved {
		t.Fatalf("写 topic 应发 1 条 saved 事件，得到 %+v", e.activity)
	}
	if e.activity[0].File != "relay_tunnel_id.md" || e.activity[0].RunId != "run-e2e" ||
		e.activity[0].Scope != events.MemoryScopeMain {
		t.Fatalf("saved 事件读数不对: %+v", e.activity[0])
	}

	// ③ 写索引
	idx := filepath.Join(e.dir.Path, memory.IndexFileName)
	e.call(t, ctx, "write_file",
		`{"path":`+strconv.Quote(idx)+`,"content":`+strconv.Quote("- relay_tunnel_id.md\n")+`}`)

	// ④ 读 topic → recalled 事件（读索引不算）
	e.activity = nil
	e.call(t, ctx, "read_file", `{"path":`+strconv.Quote(topic)+`}`)
	if len(e.activity) != 1 || e.activity[0].Action != events.MemoryActionRecalled {
		t.Fatalf("读 topic 应发 1 条 recalled 事件，得到 %+v", e.activity)
	}

	// ⑤ browse 命令列出两个文件，且带上前端要显示的字段
	listing := e.browse(t)
	if listing == nil {
		t.Fatalf("browse 响应里没有 available=true：%v", readLines(e.buf))
	}
	byName := map[string]memory.ListedFile{}
	for _, f := range listing.Files {
		byName[f.Name] = f
	}
	tf, ok := byName["relay_tunnel_id.md"]
	if !ok {
		t.Fatalf("browse 未列出 topic：%+v", listing.Files)
	}
	if tf.Type != "project" || tf.Modified == "" || !tf.Indexed || tf.Missing {
		t.Fatalf("topic 的浏览读数不对（type/modified/indexed）：%+v", tf)
	}
	if !listing.IndexPresent {
		t.Fatal("browse 未认出 MEMORY.md 存在")
	}
}

// TestMemoryEndToEndSubagentDirectoryIsSeparate：带 memory: true 的子 agent
// 写到**自己**那份目录，主目录不受影响。
//
// 走的是生产引擎（主/子共用同一个 eng），所以它同时验证了"归属按路径判定"这个
// 设计前提在生产装配下成立——钩子拿不到调用者身份，只能靠两个目录不相交。
func TestMemoryEndToEndSubagentDirectoryIsSeparate(t *testing.T) {
	e := newMemE2E(t)
	ctx := e.ctx(t)

	sub, ok := e.cfg.SubagentDir(*e.dir, "docs-writer")
	if !ok {
		t.Fatal("SubagentDir 应当成功")
	}

	// 主目录与子目录各放一个同名 topic：归属判错的话补戳会落到另一个文件上。
	body := "---\ntype: project\n---\n\nbody\n"
	for _, d := range []string{e.dir.Path, sub.Path} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "same-name.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	subTopic := filepath.Join(sub.Path, "same-name.md")
	e.call(t, ctx, "write_file",
		`{"path":`+strconv.Quote(subTopic)+`,"content":`+strconv.Quote(body)+`}`)

	subGot, _ := os.ReadFile(subTopic)
	if !strings.Contains(string(subGot), "modified:") {
		t.Fatalf("子 agent 的 topic 未被补 modified\n%s", subGot)
	}
	mainGot, _ := os.ReadFile(filepath.Join(e.dir.Path, "same-name.md"))
	if strings.Contains(string(mainGot), "modified:") {
		t.Fatalf("主目录的同名 topic 被这次写入影响了（归属判错）\n%s", mainGot)
	}

	// 事件 Scope 也必须分开。
	subEv := false
	for _, a := range e.activity {
		if a.Scope == events.MemoryScopeSubagent {
			subEv = true
		}
	}
	if !subEv {
		t.Fatalf("子 agent 的写入应标 subagent scope，得到 %+v", e.activity)
	}
	// 子 agent 目录不在主目录下 → 主会话的 browse 不该列出它。
	listing := e.browse(t)
	if listing != nil && strings.Contains(listing.Dir, "agents") {
		t.Fatalf("browse 返回的应是主目录，却指向 %q", listing.Dir)
	}
}

// browseListing 从同步缓冲里取 browse 响应的 Listing（available=false 时返回 nil）。
func browseListing(t *testing.T, buf *syncBuf) *memory.Listing {
	t.Helper()
	for _, l := range readLines(buf) {
		if l["ok"] != true {
			continue
		}
		d, _ := l["data"].(map[string]any)
		if d == nil || d["available"] != true {
			continue
		}
		raw, err := json.Marshal(d["listing"])
		if err != nil {
			t.Fatalf("序列化 listing 失败: %v", err)
		}
		var out memory.Listing
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("反序列化 listing 失败: %v", err)
		}
		return &out
	}
	return nil
}
