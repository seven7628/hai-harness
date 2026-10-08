package codemode

// protocol_test.go：宿主 ↔ scaffold 的**跨语言契约**测试。
//
// 为什么值得单列：桥接协议靠两侧约定、没有类型约束（设计文档 §1.10 那条架构债），
// 一旦漂移，症状是「脚本调用静默失效」而不是报错。本文件把三处耦合钉住：
//
//	1. hostCall 的保留名 —— scaffold 里的 JS 常量 vs Go 的 op* 常量（真跑一遍，取实到名）；
//	2. scaffold 装的全局 —— 恰好是教学正文承诺的那几个，且不越界去覆盖 exit/tools；
//	3. init 载荷与各回执的字段名 —— Go 结构体的 tag vs JS 里读的键。
//
// 另外这里放了保留操作的宿主侧单测（store/image/search/describe 的判据与错误面），
// 它们不需要 node：保留操作是纯宿主逻辑，只有「谁来发这条消息」属于沙箱。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/execproc"
	"github.com/seven7628/hai-harness/tools"
)

// ---- 1. 保留名：真跑一遍，看宿主实际收到哪些名字 ----

func TestScaffoldHostCallNamesMatchGoConstants(t *testing.T) {
	requireNode(t)
	pngPath := filepath.Join(t.TempDir(), "pic.png")
	writeTinyPNG(t, pngPath)

	var mu = make(chan string, 16)
	e := execproc.New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := e.ExecuteScript(ctx, execproc.ScriptOpts{
		Timeout:  20 * time.Second,
		Scaffold: scaffoldSource,
		Init:     json.RawMessage(`{"tools":[]}`),
		Script: fmt.Sprintf(`
await searchTools("x");
await describeTool("y");
await store("k", 1);
await store("k", undefined);
await image(%q);
return "done";
`, pngPath),
		OnCall: func(_ context.Context, c execproc.ScriptCall) execproc.ScriptResult {
			mu <- c.Name
			// 回执形状按各保留操作的契约给最小合法值（scaffold 会读 res.tools / res.path）。
			switch c.Name {
			case opSearch:
				return execproc.ScriptResult{Value: json.RawMessage(`{"tools":[]}`)}
			case opDescribe:
				return execproc.ScriptResult{Value: json.RawMessage(`{"name":"y"}`)}
			case opStore:
				return execproc.ScriptResult{Value: json.RawMessage(`{"key":"k","bytes":1}`)}
			case opImage:
				return execproc.ScriptResult{Value: json.RawMessage(`{"path":"x","mime":"image/png","bytes":1}`)}
			}
			return execproc.ScriptResult{IsError: true, Value: jsonText("unexpected host call: " + c.Name)}
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("脚本没跑完: exit=%d text=%q", res.ExitCode, res.Text)
	}
	close(mu)
	var seen []string
	for n := range mu {
		seen = append(seen, n)
	}
	// PATH 用的是真实存在的 png（image 那条会被宿主读文件，故这里给真图片）。
	want := []string{opSearch, opDescribe, opStore, opStore, opImage}
	if len(seen) != len(want) {
		t.Fatalf("宿主收到的 hostCall = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("第 %d 条 hostCall = %q, want %q（scaffold 里的常量与 Go 常量漂移了）",
				i, seen[i], want[i])
		}
	}
	// 每个名字都必须能被 reservedOp 认出来（否则会掉进「未知工具」分支）。
	for _, n := range seen {
		if _, ok := reservedOp(n); !ok {
			t.Fatalf("reservedOp 不认识 %q —— scaffold 用了 Go 侧没定义的保留名", n)
		}
	}
}

// ---- 2. scaffold 装的全局（纯文本解析，不需要 node）----

func TestScaffoldDefinesExactlyPromisedGlobals(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(`globalThis\.([A-Za-z_$][A-Za-z0-9_$]*)\s*=`)
	got := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(scaffoldSource, -1) {
		got[m[1]] = true
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	// 教学正文（DESCRIPTION_INTRO）承诺的就是这几个；新增/删除必须同步改正文与这里。
	want := []string{"describeTool", "image", "load", "searchTools", "store", "text"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("scaffold 装的全局 = %v, want %v", names, want)
	}
	// 越界红线：exit / tools 属于传输层，scaffold 不许覆盖（覆盖会让 exit 语义与
	// 保活曲线各说各话，且 tools 的 Proxy 消失后未知工具报错形态也会变）。
	for _, forbidden := range []string{"exit", "tools", "__codemode", "__codemodeBridge"} {
		if got[forbidden] {
			t.Fatalf("scaffold 覆盖了传输层的全局 %q", forbidden)
		}
		if strings.Contains(scaffoldSource, "globalThis."+forbidden+" =") {
			t.Fatalf("scaffold 覆盖了传输层的全局 %q", forbidden)
		}
	}
	// 正文里提到的全局都得真装上（正文是模型唯一的说明书）。
	for _, g := range want {
		if !strings.Contains(DESCRIPTION_INTRO, g+"(") {
			t.Errorf("教学正文没提 %s()，但 scaffold 装上了它 —— 模型不会用", g)
		}
	}
}

// ---- 3. 保留名不可能与脚本可调用名撞 ----

func TestReservedOpsNeverCollideWithScriptNames(t *testing.T) {
	t.Parallel()
	names := []string{
		ToolName, "read_file", "mcp__dev-radius__get", "$codemode:store", "$codemode_store",
		"$codemode.image", "$codemode:image", "store", "text", "exit", "searchTools",
	}
	for _, n := range names {
		got := normalize(n)
		if strings.Contains(got, ":") {
			t.Fatalf("normalize(%q) = %q 仍含冒号 —— 保留名与脚本名的隔离前提没了", n, got)
		}
		if _, ok := reservedOp(got); ok {
			t.Fatalf("normalize(%q) = %q 撞上保留名", n, got)
		}
		// 目录表里存的是归一化名（可能带 fnv 后缀），它同样不得等于保留名。
		if _, ok := reservedOp(scriptNameFor(n)); ok {
			t.Fatalf("scriptNameFor(%q) = %q 撞上保留名", n, scriptNameFor(n))
		}
	}
}

// ---- 4. init 载荷与回执字段：Go 的 tag 必须与 JS 读的键一致 ----

func TestProtocolFieldNamesMatchScaffoldAndTransport(t *testing.T) {
	t.Parallel()
	// init 载荷：传输层（prelude_bridge.js）读 init.tools，项里读 name / rawName。
	raw, err := json.Marshal(initPayload{
		Tools: []initTool{{Name: "read_file", RawName: "mcp__dev__read-file"}},
		Store: map[string]json.RawMessage{"k": json.RawMessage(`1`)},
	})
	if err != nil {
		t.Fatalf("marshal initPayload: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("initPayload 不是 JSON 对象: %v", err)
	}
	for _, key := range []string{"tools", "store"} {
		if _, ok := generic[key]; !ok {
			t.Fatalf("init 载荷缺键 %q: %s", key, raw)
		}
	}
	item := generic["tools"].([]any)[0].(map[string]any)
	if item["name"] != "read_file" || item["rawName"] != "mcp__dev__read-file" {
		t.Fatalf("init 工具项字段名不对（传输层读 name/rawName）: %s", raw)
	}

	// searchTools 的回执：scaffold 明确读 `res.tools`（唯一由 JS 解结构的保留操作），
	// 故这一对必须对齐；image/store/describe 的回执是**原样透给脚本**的，脚本按
	// 教学正文/文档消费，没有 JS 侧的解构点。
	searchJSON := mustJSONOf(t, searchReply{Tools: []searchHit{{Name: "read_file", Summary: "s", Namespace: "fs"}}})
	if !strings.Contains(scaffoldSource, "res.tools") {
		t.Fatal("scaffold 里看不到 res.tools —— 契约的另一半不见了")
	}
	for _, key := range []string{"tools", "name", "summary", "namespace"} {
		if !strings.Contains(searchJSON, `"`+key+`"`) {
			t.Fatalf("searchTools 回执缺字段 %q: %s", key, searchJSON)
		}
	}
	// 图片回执的三个字段是宿主/UI 侧取数口（前端按 mime 判定是否可展示）。
	if img := mustJSONOf(t, imageReply{Path: "/p", Mime: "image/png"}); !strings.Contains(img, `"mime"`) {
		t.Fatalf("image 回执缺 mime: %s", img)
	}

	// 沙箱读 init 里的限额（store() 的快速失败用），且不自己写常数。
	if !strings.Contains(scaffoldSource, "init.store_limits") {
		t.Fatal("scaffold 没读 init.store_limits（限额数值必须由宿主给）")
	}

	// 保留名常量在 JS 里逐字出现（三处同源：Go 常量、JS 常量、这里的断言）。
	for _, op := range []string{opStore, opImage, opSearch, opDescribe} {
		if !strings.Contains(scaffoldSource, "'"+op+"'") {
			t.Fatalf("scaffold 里没有保留名字面量 %q", op)
		}
	}
}

func mustJSONOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ---- 5. 保留操作的宿主侧行为（无 node）----

// newOpBridge 造一个只用于保留操作单测的 bridge。
func newOpBridge(t *testing.T, list ...tools.Tool) (*bridge, *Store) {
	t.Helper()
	st, err := OpenStore("")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	tool := New(Options{Tools: func() []tools.Tool { return list }})
	snap := tool.snapshot()
	cat, err := BuildCatalog(DescribeOptions{Tools: snap.catalog})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	return &bridge{
		tool:    tool,
		cat:     cat,
		byRaw:   snap.byRaw,
		ns:      snap.ns,
		store:   st,
		pending: NewPending(),
		cancels: map[int64]context.CancelFunc{},
	}, st
}

func TestHostOpStore(t *testing.T) {
	t.Parallel()
	b, _ := newOpBridge(t)
	call := func(args string) execproc.ScriptResult {
		return b.hostOp(context.Background(), opStore, json.RawMessage(args))
	}

	ok := call(`{"key":"a","value":{"n":1}}`)
	if ok.IsError {
		t.Fatalf("正常写入被判失败: %s", ok.Value)
	}
	var rep storeReply
	if err := json.Unmarshal(ok.Value, &rep); err != nil || rep.Key != "a" || rep.Bytes == 0 {
		t.Fatalf("回执不对: %s (err=%v)", ok.Value, err)
	}
	// 未 await 的写入也必须真到宿主（脚本靠这条承诺「写入会被保留」）。
	if rec := b.pending.Record(); len(rec.Set) != 1 {
		t.Fatalf("写入没进 Pending: %+v", rec)
	}

	del := call(`{"key":"a","delete":true}`)
	if del.IsError {
		t.Fatalf("删除被判失败: %s", del.Value)
	}
	if rec := b.pending.Record(); len(rec.Delete) != 1 {
		t.Fatalf("删除没进 Pending: %+v", rec)
	}

	// 超限值：**当场**报错（脚本可 catch），而不是等脚本跑完才失败。
	big := strings.Repeat("x", MaxStoreValueBytes+10)
	over := call(`{"key":"big","value":"` + big + `"}`)
	if !over.IsError {
		t.Fatal("超过单值上限的写入必须当场失败")
	}
	if !strings.Contains(string(over.Value), "256 KiB") {
		t.Fatalf("限额报错文案不可读: %s", over.Value)
	}
	if rec := b.pending.Record(); len(rec.Set) != 0 {
		t.Fatalf("被拒的写入不该留在 Pending: %+v", rec)
	}

	// 非法请求体。
	if bad := call(`not json`); !bad.IsError {
		t.Fatal("畸形请求必须判失败")
	}
}

// TestScaffoldStoreRollsBackRejectedWritesAndLoadKeepsProtoKey 两条跨语言修正的钉子
// （对抗复核 F7）：① load() 无参不许用普通字面量累加 —— 键 __proto__ 会走原型 setter
// 而静默消失（单查 load("__proto__") 却拿得到，症状极难查）；② 宿主拒绝的写入必须回滚
// 本地视图 —— 否则同一段脚本里的 load() 会读到「根本没落盘的值」。
func TestScaffoldStoreRollsBackRejectedWritesAndLoadKeepsProtoKey(t *testing.T) {
	requireNode(t)
	b, _ := newOpBridge(t)
	e := execproc.New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := e.ExecuteScript(ctx, execproc.ScriptOpts{
		Timeout:  20 * time.Second,
		Scaffold: scaffoldSource,
		Init:     json.RawMessage(`{"tools":[],"store":{}}`),
		Script: `
await store("__proto__", 41);
await store("n", 1);
const all = load();
const longKey = "k".repeat(2048);
let caught = "";
try { await store(longKey, 1); } catch (e) { caught = String((e && e.message) || e); }
text(JSON.stringify({
  keys: Object.keys(all).sort().join(","),
  proto: String(all["__proto__"]),
  caught: caught.slice(0, 80),
  keysAfter: Object.keys(load()).length,
}));
return "";
`,
		OnCall: func(cctx context.Context, c execproc.ScriptCall) execproc.ScriptResult {
			if op, ok := reservedOp(c.Name); ok {
				return b.hostOp(cctx, op, c.Args)
			}
			return execproc.ScriptResult{IsError: true, Value: jsonText("unexpected tool call: " + c.Name)}
		},
	})
	if err != nil {
		t.Fatalf("ExecuteScript: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("脚本没跑完: exit=%d text=%q", res.ExitCode, res.Text)
	}
	// 取最后一行非空输出：text() 与 console.* 汇在同一条通道里，前面可能还有别的行。
	lines := strings.Split(strings.TrimSpace(res.Text), "\n")
	payload := lines[len(lines)-1]
	var got struct {
		Keys      string `json:"keys"`
		Proto     string `json:"proto"`
		Caught    string `json:"caught"`
		KeysAfter int    `json:"keysAfter"`
	}
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("脚本输出不是预期 JSON: %q (err=%v)", res.Text, err)
	}
	if got.Keys != "__proto__,n" {
		t.Errorf("load() 列出的键 = %q, want \"__proto__,n\"（普通字面量会让 __proto__ 静默消失）", got.Keys)
	}
	if got.Proto != "41" {
		t.Errorf("load()[\"__proto__\"] = %q, want \"41\"", got.Proto)
	}
	if !strings.Contains(got.Caught, "1 KiB") {
		t.Errorf("超长键被宿主拒绝时，脚本应看到可读原因，实际 %q", got.Caught)
	}
	if got.KeysAfter != 2 {
		t.Errorf("被拒的写入没有回滚本地视图：load() 里有 %d 个键，want 2（__proto__ 与 n）", got.KeysAfter)
	}
	// 宿主侧同样只该有两个键（长键那次压根没进 Pending）。
	if rec := b.pending.Record(); len(rec.Set) != 2 {
		t.Errorf("宿主 Pending 里的键 = %d, want 2: %+v", len(rec.Set), rec.Set)
	}
}

func TestHostOpImage(t *testing.T) {
	t.Parallel()
	b, _ := newOpBridge(t)
	dir := t.TempDir()
	png := filepath.Join(dir, "ok.png")
	writeTinyPNG(t, png)
	bogus := filepath.Join(dir, "no.txt")
	if err := os.WriteFile(bogus, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}

	sink := &tools.ImageSink{}
	ctx := tools.WithImageSink(context.Background(), sink)

	good := b.hostOp(ctx, opImage, json.RawMessage(`{"path":`+string(mustJSONBytes(t, png))+`}`))
	if good.IsError {
		t.Fatalf("合法图片被判失败: %s", good.Value)
	}
	var rep imageReply
	if err := json.Unmarshal(good.Value, &rep); err != nil || rep.Mime != "image/png" || rep.Bytes == 0 {
		t.Fatalf("回执不对: %s (err=%v)", good.Value, err)
	}
	if imgs := sink.Images(); len(imgs) != 1 || imgs[0].Type != "image" ||
		!strings.HasPrefix(imgs[0].Content, "data:image/png;base64,") {
		t.Fatalf("图片没进槽: %+v", imgs)
	}

	// 坏图片：必须当场拒绝且**不**进槽（pi #10215 的不变量）。
	bad := b.hostOp(ctx, opImage, json.RawMessage(`{"path":`+string(mustJSONBytes(t, bogus))+`}`))
	if !bad.IsError || !strings.Contains(string(bad.Value), "not a supported image") {
		t.Fatalf("坏图片必须被拒且说明原因: %s", bad.Value)
	}
	if imgs := sink.Images(); len(imgs) != 1 {
		t.Fatalf("坏图片进了槽: %+v", imgs)
	}

	for _, args := range []string{`{"path":""}`, `{"path":"/nonexistent/x.png"}`, `{"path":"` + dir + `"}`} {
		if r := b.hostOp(ctx, opImage, json.RawMessage(args)); !r.IsError {
			t.Fatalf("%s 应当判失败", args)
		}
	}
}

func TestHostOpSearchAndDescribe(t *testing.T) {
	t.Parallel()
	nsTool := &nsReadFileStub{readFileStub: *newReadFileStub()}
	b, _ := newOpBridge(t, nsTool, newTierStub("gone", tools.ExposureHidden))

	hit := b.hostOp(context.Background(), opSearch, json.RawMessage(`{"query":"read","limit":3}`))
	if hit.IsError {
		t.Fatalf("searchTools 失败: %s", hit.Value)
	}
	var srep searchReply
	if err := json.Unmarshal(hit.Value, &srep); err != nil || len(srep.Tools) == 0 {
		t.Fatalf("回执不对: %s (err=%v)", hit.Value, err)
	}
	for _, h := range srep.Tools {
		if h.Name == "gone" {
			t.Fatalf("检索泄漏 hidden 工具: %+v", srep.Tools)
		}
	}

	d := b.hostOp(context.Background(), opDescribe, json.RawMessage(`{"name":"read_file"}`))
	if d.IsError {
		t.Fatalf("describeTool 失败: %s", d.Value)
	}
	var drep describeReply
	if err := json.Unmarshal(d.Value, &drep); err != nil {
		t.Fatalf("回执不对: %s (err=%v)", d.Value, err)
	}
	if drep.Name != "read_file" || drep.ScriptName != "read_file" || len(drep.Params) != 1 {
		t.Fatalf("describeTool 回执不全: %+v", drep)
	}
	if drep.NamespaceInstructions == "" {
		t.Fatalf("namespace 的 instructions 必须经 describeTool 给: %+v", drep)
	}
	// 不可编排/未知的工具：走同一条「不在工具表里」的拒绝。
	if r := b.hostOp(context.Background(), opDescribe, json.RawMessage(`{"name":"gone"}`)); !r.IsError {
		t.Fatal("describeTool 不该能描述 hidden 工具")
	}
	if r := b.hostOp(context.Background(), opDescribe, json.RawMessage(`{"name":"nope"}`)); !r.IsError {
		t.Fatal("describeTool 对未知工具应报错")
	}
	// 未知保留操作。
	if r := b.hostOp(context.Background(), "$codemode:nope", json.RawMessage(`{}`)); !r.IsError {
		t.Fatal("未知保留操作应报错")
	}
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
