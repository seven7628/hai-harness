package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// memDir 造一个记忆目录并返回 Dir + 其路径。
func memDir(t *testing.T) Dir {
	t.Helper()
	root := t.TempDir()
	d := Dir{ID: "test-id", Path: filepath.Join(root, "repos", "test-id")}
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func writeIndex(t *testing.T, d Dir, content string) string {
	t.Helper()
	p := filepath.Join(d.Path, IndexFileName)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// countingConfig 给 Config 装上计数的 I/O 出口：用来断言"关闭开关时一次读盘都没有"。
func countingConfig(t *testing.T, reads *int32, writes *int32) Config {
	t.Helper()
	c := Config{}.withDefaults()
	realRead, realWrite := c.ReadFile, c.WriteFile
	if reads != nil {
		c.ReadFile = func(p string) ([]byte, error) {
			atomic.AddInt32(reads, 1)
			return realRead(p)
		}
	}
	if writes != nil {
		c.WriteFile = func(p string, b []byte, m os.FileMode) error {
			atomic.AddInt32(writes, 1)
			return realWrite(p, b, m)
		}
	}
	return c
}

// —————————————————————— 基本形态 ——————————————————————

// 官方目录结构的最小复刻：MEMORY.md 是一行一个文件名的纯列表。
func TestBuildInjection_IndexOneLinePerMemory(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "# Project Memory\n\n- user_role.md\n- feedback_testing.md\n- build-and-verify.md\n")

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Empty() {
		t.Fatal("有索引时不得为空注入")
	}
	if !strings.HasPrefix(inj.Text, "# Project Memory") {
		t.Fatalf("注入段落应以标题开头：%q", inj.Text)
	}
	for _, want := range []string{"user_role.md", "feedback_testing.md", "build-and-verify.md"} {
		if !strings.Contains(inj.Text, want) {
			t.Fatalf("注入缺少 %q：\n%s", want, inj.Text)
		}
	}
	// 官方没有 description / title / 摘要：索引行就是文件名，注入里不该凭空多出描述。
	if strings.Contains(inj.Text, "description") {
		t.Fatalf("注入里出现 description（官方没有这个字段）：\n%s", inj.Text)
	}
	// 度量字段可断言（Step 5 评估要靠它看"到底加载了多少"）。
	if inj.Load == nil || inj.Load.LoadedLines != 5 || inj.Load.TruncatedBy != "" {
		// LoadedLines 是"从文件加载了几行"（官方量文件本身），标题的剥离不计入。
		t.Fatalf("Load = %+v", inj.Load)
	}
	if inj.Dir == nil || inj.Dir.Path != d.Path {
		t.Fatalf("Dir = %+v", inj.Dir)
	}
}

// —————————————————————— 官方规定的边界 ——————————————————————

// 索引不存在：官方 "If a file doesn't exist, no error" —— 不报错，且**必须仍然注入
// 目录说明**（2026-09-30 起）。
//
// 这一条是 P0-1 的核心断言：没有 MEMORY.md 就永远没有"第一条记忆"，而"第一条"
// 只可能靠这段说明写出来。不注入 = 冷启动死锁（guidance.go 顶部）。
func TestBuildInjection_NoIndexStillInjectsGuidance(t *testing.T) {
	d := memDir(t)
	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatalf("索引不存在不应报错：%v", err)
	}
	if inj.Reason != "no-index" {
		t.Fatalf("Reason = %q, want \"no-index\"", inj.Reason)
	}
	if inj.Empty() {
		t.Fatal("无索引时仍须注入目录说明——否则模型永远写不出第一条记忆")
	}
	if !strings.Contains(inj.Text, d.Path) {
		t.Fatalf("说明段必须含记忆目录绝对路径 %q：\n%s", d.Path, inj.Text)
	}
	if !strings.Contains(inj.Text, "当前尚无记忆") {
		t.Fatalf("无索引时须如实说明尚无记忆：\n%s", inj.Text)
	}
}

// 索引存在但为空（零字节 / 只有空白 / 只有标题）：**不注入索引正文**，但同样注入
// 目录说明。三种退化状态与"文件不存在"在模型看来是同一件事，给同一段文本。
func TestBuildInjection_EmptyIndex(t *testing.T) {
	d := memDir(t)
	for name, content := range map[string]string{
		"零字节":  "",
		"只有空白": "   \n\n\t\n",
		"只有标题": "# Project Memory\n",
	} {
		writeIndex(t, d, content)
		inj, err := (Config{}).BuildInjection(d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if inj.Empty() || !strings.Contains(inj.Text, d.Path) {
			t.Fatalf("%s: 期望注入目录说明，得到 %q", name, inj.Text)
		}
		if inj.Load != nil {
			t.Fatalf("%s: 索引无正文时 Load 应为 nil，得到 %+v", name, inj.Load)
		}
		if inj.Reason != "empty-index" {
			t.Fatalf("%s: Reason = %q", name, inj.Reason)
		}
	}
}

// 官方限额一：200 行。超出的部分"下次加载时被丢弃"。
func TestBuildInjection_LineLimit(t *testing.T) {
	d := memDir(t)
	var b strings.Builder
	b.WriteString("# Project Memory\n")
	for i := 1; i <= 300; i++ {
		b.WriteString("- topic")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(".md\n")
	}
	writeIndex(t, d, b.String())

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Load.TotalLines <= MaxIndexLines {
		t.Fatalf("前提失败：TotalLines = %d", inj.Load.TotalLines)
	}
	if inj.Load.LoadedLines != MaxIndexLines {
		t.Fatalf("LoadedLines = %d, want %d（官方 200 行硬限额）", inj.Load.LoadedLines, MaxIndexLines)
	}
	if inj.Load.TruncatedBy != "line" {
		t.Fatalf("TruncatedBy = %q, want \"line\"", inj.Load.TruncatedBy)
	}
	// 第 201 行起的内容必须**不在**注入里（官方：past the limit is dropped）。
	if strings.Contains(inj.Text, "topic202.md") {
		t.Fatal("第 201 行之后的内容不应进入注入")
	}
	if !strings.Contains(inj.Text, "topic199.md") {
		t.Fatal("第 199 行应保留（标题占第 1 行，恰好装满 200 行）")
	}
	// 结尾换行不算多一行：文件恰好 301 行（标题 1 + 条目 300），限额切在第 200 行，
	// 即最后一个被加载的条目是 topic199。
	if inj.Load.TotalLines != 301 {
		t.Fatalf("前提失败：TotalLines = %d, want 301", inj.Load.TotalLines)
	}
}

// 官方限额二：25KB。200 行先到时本测试不成立，故用短行大量堆字节。
func TestBuildInjection_ByteLimit(t *testing.T) {
	d := memDir(t)
	// 每行 200 字节（含换行）：200 行 = 40000 > 25000，所以 25KB 必然**先于** 200 行触发。
	// 反例警示：行短于 125 字节时 200 行会先触发，这个测试就变成在测行限额。
	line := "-" + strings.Repeat("a", 198) + "\n"
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString(line)
	}
	writeIndex(t, d, b.String())

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Load.TruncatedBy != "byte" {
		t.Fatalf("TruncatedBy = %q, want \"byte\"（TotalLines=%d TotalBytes=%d）",
			inj.Load.TruncatedBy, inj.Load.TotalLines, inj.Load.TotalBytes)
	}
	if inj.Load.LoadedBytes > MaxIndexBytes {
		t.Fatalf("LoadedBytes = %d > %d", inj.Load.LoadedBytes, MaxIndexBytes)
	}
	if inj.Load.LoadedLines >= MaxIndexLines {
		t.Fatalf("前提失败：字节限额应先于行限额触发，LoadedLines = %d", inj.Load.LoadedLines)
	}
}

// 截断不能产出非法 UTF-8：25KB 边界落在多字节字符中间时必须退到 rune 边界。
// 非法字节进 prompt 会让 provider 侧报错，那是"限额"之外的故障。
func TestBoundIndex_TruncationKeepsValidUTF8(t *testing.T) {
	d := memDir(t)
	// 一行 3 字节的 "中"：让第 MaxIndexBytes 字节正好落在字符中间。
	// 198 字节/行且含多字节字符：25KB 落点 25000 = 126×198 + 52，会切在某个三字节
	// 字符中间（52-2=50，50 mod 3 = 2，剩 1 字节），正好检验 rune 边界回退。
	line := "- " + strings.Repeat("中", 65) + "\n"
	var b strings.Builder
	for b.Len() < MaxIndexBytes+300 {
		b.WriteString(line)
	}
	load, text := boundIndex(d, []byte(b.String()))
	if load.TruncatedBy != "byte" {
		t.Fatalf("TruncatedBy = %q", load.TruncatedBy)
	}
	if !isValidUTF8(text) {
		t.Fatalf("截断后含非法 UTF-8：%q", text[len(text)-20:])
	}
}

// 索引里混有非 topic 的行（说明文字、目录、坏行）：**如实呈现，不校验、不丢弃**。
//
// 理由（重要，写在这里以免后来者"顺手加校验"）：官方对 MEMORY.md 的全部规定是
// "one line per memory" 与两个读限额，**没有任何一致性校验**，也没有"索引行对应
// 的文件必须存在"的要求。任何额外校验都是《技术方案》§4.5 禁止的自创机制，而且会
// 产生一个危险副作用：模型把 MEMORY.md 改坏（例如临时写成散文）时，宿主擅自"修复"
// 或丢弃内容，等于把模型的写入权限夺走，而记忆系统本该只做"给模型看清单"。
//
// 真正会出事的是**文件不存在**（见下一个测试）——那是内容陈旧，不是内容非法。
func TestBuildInjection_IndexWithNonTopicLinesIsPreservedVerbatim(t *testing.T) {
	d := memDir(t)
	raw := "# Project Memory\n" +
		"\n" +
		"Notes live in topic files; keep this list short.\n" +
		"- user_role.md\n" +
		"- deleted_topic.md\n" +
		"sub/nested.md\n" +
		"- notes.txt\n" +
		"MEMORY.md\n"
	writeIndex(t, d, raw)

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Load.TruncatedBy != "" {
		t.Fatalf("不得截断：%+v", inj.Load)
	}
	// 逐字节等于「宿主标题 + 原文去掉重复标题行 + 目录说明」：官方没规定索引行格式，
	// 宿主不得改写模型的写入（唯一例外是那段重复的标题，见 stripIndexHeading）。
	want := indexHeading + "\n\n" +
		"Notes live in topic files; keep this list short.\n" +
		"- user_role.md\n- deleted_topic.md\nsub/nested.md\n- notes.txt\nMEMORY.md" + // 结尾换行由宿主补齐
		"\n\n" + guidance(d, true)
	if inj.Text != want {
		t.Fatalf("注入与原文不一致：\n%q\nwant\n%q", inj.Text, want)
	}
	for _, want := range []string{"deleted_topic.md", "notes.txt", "sub/nested.md"} {
		if !strings.Contains(inj.Text, want) {
			t.Fatalf("索引行 %q 被丢弃了（不应丢弃）", want)
		}
	}
}

// topic 文件已被删除但索引仍引用：**如实呈现，且一次额外的 stat 都不做**。
//
// 官方明确"官方无校验"（§4.5 不得自行发明），所以本包**不去核对**每个索引行对应的
// 文件是否存在。理由有三：
//
//  1. 核对 = 启动时对 200 个文件做 200 次 stat，且模型随时可能删/建，
//     校验结果在注入那一刻就已经过期——它既不能阻止陈旧，也挡不住新的不一致。
//  2. 隐藏不存在的条目是**替模型改写事实**：模型下一次读文件会 404，然后它自己改索引；
//     而如果宿主悄悄删掉那一行，模型永远不知道自己记错了什么。
//  3. 官方把索引当"清单"而不是"真相"：真相在 topic 文件里，模型用标准 read_file
//     读不到就会知道。
//
// 因此陈旧条目的正确处理者永远是模型自己，宿主的职责只是把清单原样给它。
func TestBuildInjection_DanglingIndexEntriesAreNotValidated(t *testing.T) {
	d := memDir(t)
	raw := "- exists.md\n- gone.md\n"
	writeIndex(t, d, raw)
	// 故意**不**创建 exists.md / gone.md 两个文件。
	if _, err := os.Stat(filepath.Join(d.Path, "gone.md")); !os.IsNotExist(err) {
		t.Fatal("前提失败：gone.md 不应存在")
	}
	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inj.Text, "gone.md") {
		t.Fatalf("指向已删除文件的条目被静默丢弃了（应为如实呈现）：\n%s", inj.Text)
	}
	if !strings.Contains(inj.Text, "exists.md") {
		t.Fatalf("注入丢失了存在的条目：\n%s", inj.Text)
	}
}

// —————————————————————— 开关（用户可见） ——————————————————————

// 关闭开关：完全不注入，且**一次读盘都不发生**。
//
// 这条是开关的全部价值所在——关闭必须是"真的不注入"，不是"注入一段让模型别用记忆"
// （后者仍占 context 且模型未必遵守，§2 判定的上一版失败根因）。
func TestBuildInjection_DisabledProducesNoInjectionAndNoRead(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- user_role.md\n")

	var reads, writes int32
	base := countingConfig(t, &reads, &writes)
	cfg := base
	cfg.Enabled = Bool(false) // 用户在设置面板关掉开关
	inj, err := cfg.BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Text != "" || !inj.Empty() {
		t.Fatalf("关闭开关时 Text 必须为空，得到 %q", inj.Text)
	}
	if inj.Enabled {
		t.Fatal("Enabled 应为 false")
	}
	if inj.Reason != "disabled" {
		t.Fatalf("Reason = %q, want \"disabled\"", inj.Reason)
	}
	if n := atomic.LoadInt32(&reads); n != 0 {
		t.Fatalf("关闭开关时发生了 %d 次读盘（必须为 0）", n)
	}
	if n := atomic.LoadInt32(&writes); n != 0 {
		t.Fatalf("关闭开关时发生了 %d 次写盘（必须为 0）", n)
	}
	// 关闭不删除已存在的记忆文件（官方：开关只控制加载，删数据是用户的显式操作）。
	if _, err := os.Stat(filepath.Join(d.Path, IndexFileName)); err != nil {
		t.Fatalf("关闭开关不得删除已有记忆文件：%v", err)
	}
}

// 开关关闭时连身份解析都不该发生（Bootstrap 层，见 bootstrap_test.go）；
// 这一层只保证 BuildInjection 自己也不偷跑。

// —————————————————————— 写后测量（§4.3） ——————————————————————

// 超限时"写入仍成功、返回错误"——本包不写正文，只报告。
func TestCheckIndex(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- a.md\n")
	if err := (Config{}).CheckIndex(d); err != nil {
		t.Fatalf("限额内应为 nil：%v", err)
	}
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("- ")
		b.WriteString(strings.Repeat("z", 20))
		b.WriteString(strconv.Itoa(i))
		b.WriteString(".md\n")
	}
	over := b.String()
	writeIndex(t, d, over)
	err := (Config{}).CheckIndex(d)
	if err == nil {
		t.Fatal("超限应返回错误要求模型重写索引")
	}
	if !isErr(err, ErrIndexTooLarge) {
		t.Fatalf("err = %v, 期望包装 ErrIndexTooLarge", err)
	}
	// 关键：报告超限**不得**改动文件（官方：写入已经成功了）。
	after := mustRead(t, filepath.Join(d.Path, IndexFileName))
	if after != over {
		t.Fatal("CheckIndex 改动了索引文件（它只测量，不写）")
	}
}

// 接近限额时给出一条精简提醒（非错误）。官方只说 "near a limit"，没给数字，
// 这里把取值写出来是为了让它可测、可争论。
func TestNearIndexNotice(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- a.md\n")
	if got := (Config{}).NearIndexNotice(d); got != "" {
		t.Fatalf("离限额还远时不该有提醒：%q", got)
	}
	var b strings.Builder
	for i := 0; i < 180; i++ {
		b.WriteString("- t")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(".md\n")
	}
	writeIndex(t, d, b.String())
	got := (Config{}).NearIndexNotice(d)
	if got == "" {
		t.Fatal("接近限额时应有精简提醒")
	}
	for _, want := range []string{"200", "每条一行", "topic"} {
		if !strings.Contains(got, want) {
			t.Fatalf("提醒缺少 %q：%q", want, got)
		}
	}
}

// —————————————————————— 截断提示（读侧不得静默丢数据） ——————————————————————

// truncationNoticeMarker 截断提示的固定开头。测试用它断言"有/没有提示"，
// 而不是断言整段文案——文案会改，不变的是**该不该出现**这件事。
const truncationNoticeMarker = "注意：以上索引已按 200 行 / 25KB 限额截断"

// buildLinesIndex 造一个 n 行的短行索引（每行 ~10 字节，保证 200 行先于 25KB 触发）。
func buildLinesIndex(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("- t")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(".md\n")
	}
	return b.String()
}

// 提示只陈述"本次加载被截断"这一个既成事实，不发明任何新机制。
//
// 回归动机：截断一直存在（boundIndex 一直在丢），但**只有写侧** CheckIndex 知道这件事。
// 读侧 BuildInjection 丢掉 TruncatedBy 后，模型看到的是一份语法完全正常、却少了
// 50 条的清单——它据此判定"这条我没记过"并重记，索引更长，下一轮又消失一批。
// 这个循环模型自己发现不了，所以必须由宿主如实说一声。
func TestBuildInjection_TruncationNoticeIsAppendedAtEnd(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, buildLinesIndex(250))

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Load.TruncatedBy != "line" {
		t.Fatalf("前提失败：TruncatedBy = %q", inj.Load.TruncatedBy)
	}

	notice := truncationNotice("line") + "\n\n" + guidance(d, true)
	if !strings.Contains(inj.Text, notice) {
		t.Fatalf("250 行索引应带截断提示，得到：\n%q", inj.Text)
	}
	// 提示必须在索引正文之后、说明段之前：标题 → 条目 → 截断提示 → 目录说明。
	// 官方规定的阅读结构（清单在前）不能被打断。
	if !strings.HasSuffix(inj.Text, notice) {
		t.Fatalf("提示必须紧接在目录说明之前：\n%q", inj.Text)
	}
	// 提示本身含 "line"（哪个限额先到），且落在全文最后一段。
	idx := strings.Index(inj.Text, truncationNoticeMarker)
	if idx < 0 || !strings.Contains(inj.Text[idx:], "line") {
		t.Fatalf("提示里应出现 \"line\"：\n%q", inj.Text)
	}

	// 条目本身**一字未改**：清单部分与"没有提示时的同样 200 行"逐字节相同。
	// 即：只追加，不删行、不加行、不改排序。
	body := strings.TrimSuffix(inj.Text, notice)
	body = strings.TrimSuffix(body, "\n\n"+guidance(d, true))
	_, wantBody := boundIndex(d, []byte(buildLinesIndex(250)))
	if body != indexHeading+"\n\n"+wantBody {
		t.Fatalf("提示之外的内容被改动了：\n%q", body)
	}
	if inj.Load.LoadedLines != MaxIndexLines {
		t.Fatalf("前提失败：LoadedLines = %d", inj.Load.LoadedLines)
	}
	// 恰好装满 200 行（t0..t199），下一条 t200 被丢弃。
	if !strings.HasSuffix(body, "- t199.md") {
		t.Fatalf("清单应以最后一个装下的条目结尾：\n%q", body[len(body)-40:])
	}
	if strings.Contains(body, "- t200.md") {
		t.Fatalf("第 201 行的条目不应进入清单：\n%q", body[len(body)-40:])
	}
}

// 未截断时**不得**追加任何提示（截断提示）。
//
// 2026-09-30 起注入末尾固定多一段目录说明，所以"字节不变"的不变量从**整段**收窄为
// **索引部分**——这是有意为之的收窄，不是退化：说明段是冷启动的唯一入口（见
// TestBuildInjection_NoIndexStillInjectsGuidance）。真正要守住的是"宿主的呈现层
// 不碰模型写的每一个字节"，而 boundIndex 的等式正是对这一点的证明。
func TestBuildInjection_NoNoticeWhenWithinBothLimits(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"单行", buildLinesIndex(1)},
		{"典型规模", buildLinesIndex(50)},
		{"恰好 200 行（边界）", buildLinesIndex(MaxIndexLines)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := memDir(t)
			writeIndex(t, d, tc.content)

			inj, err := (Config{}).BuildInjection(d)
			if err != nil {
				t.Fatal(err)
			}
			if inj.Load.TruncatedBy != "" {
				t.Fatalf("前提失败：不得截断，TruncatedBy = %q", inj.Load.TruncatedBy)
			}
			if strings.Contains(inj.Text, truncationNoticeMarker) {
				t.Fatalf("未截断时不得出现提示：\n%q", inj.Text)
			}
			// 索引部分逐字节等于「宿主标题 + boundIndex 产出的索引正文」——与 2026-09-30
			// 之前完全相同。这里刻意用 boundIndex 而不是"手写一个想当然的原文"：它是那次
			// 改动**之前**注入文本的逐字节来源，所以这个等式就是"索引字节没变"的证明。
			_, wantBody := boundIndex(d, []byte(tc.content))
			want := indexHeading + "\n\n" + wantBody + "\n\n" + guidance(d, true)
			if inj.Text != want {
				t.Fatalf("注入文本被改动了：\n%q\nwant\n%q", inj.Text, want)
			}
		})
	}
}

// 字节限额先到时提示里的维度名是 "byte"（不是 "line"）。
//
// 维度名必须区分：成因不同，模型该做的事不同（行数超了合并条目、字节超了缩短行）。
func TestBuildInjection_ByteLimitNoticeSaysByte(t *testing.T) {
	d := memDir(t)
	// 40 行 × 1000 字节：TotalLines=40 < 200 行，但 TotalBytes=40000 > 25000。
	line := "- " + strings.Repeat("a", 998) + "\n"
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString(line)
	}
	writeIndex(t, d, b.String())

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if inj.Load.TotalLines >= MaxIndexLines {
		t.Fatalf("前提失败：字节限额应先触发，TotalLines = %d", inj.Load.TotalLines)
	}
	if inj.Load.TruncatedBy != "byte" {
		t.Fatalf("TruncatedBy = %q, want \"byte\"", inj.Load.TruncatedBy)
	}
	notice := truncationNotice("byte")
	if !strings.HasSuffix(inj.Text, notice+"\n\n"+guidance(d, true)) {
		t.Fatalf("应带 \"byte\" 提示且紧接目录说明之前：\n%q", inj.Text)
	}
	if !strings.Contains(inj.Text, notice) {
		t.Fatalf("应带 \"byte\" 提示：\n%q", inj.Text)
	}
	// 字节限额触发的索引没被行限额再切一刀，40 行里只装下了 ~25 行。
	if inj.Load.LoadedLines >= 40 {
		t.Fatalf("前提失败：字节截断应切在行中间，LoadedLines = %d", inj.Load.LoadedLines)
	}
}

// 提示不计入 200 行 / 25KB 限额——官方量的是 MEMORY.md 本身，提示不是文件内容。
//
// 若提示被计入，就得"为提示腾额度"（少载一条记忆）或者递归地自我截断，两者都是错的。
// 本测试从**度量**一侧断言：Load.LoadedLines / LoadedBytes 仍是"从文件里加载了多少"，
// 且 Load.Text（索引正文本身）里没有提示——宿主加的话不进正文。
func TestTruncationNoticeIsNotCountedAgainstTheLimits(t *testing.T) {
	d := memDir(t)
	raw := buildLinesIndex(250)
	writeIndex(t, d, raw)

	load, text := boundIndex(d, []byte(raw))
	if load.TruncatedBy != "line" {
		t.Fatalf("前提失败：TruncatedBy = %q", load.TruncatedBy)
	}
	// boundIndex 自身**完全不知道**提示的存在：它返回的仍然是纯净的索引正文。
	if strings.Contains(text, truncationNoticeMarker) {
		t.Fatalf("boundIndex 的输出不得含提示（提示在它之后才拼）：\n%q", text)
	}
	// 度量仍然是"从文件加载了多少"，没有被提示抬高。
	if load.LoadedLines != MaxIndexLines {
		t.Fatalf("LoadedLines = %d, want %d", load.LoadedLines, MaxIndexLines)
	}
	if load.LoadedBytes > MaxIndexBytes {
		t.Fatalf("LoadedBytes = %d > %d", load.LoadedBytes, MaxIndexBytes)
	}
	joined := stripIndexHeading(strings.Join(
		strings.Split(text, "\n"), "\n"))
	if load.LoadedBytes != len(joined) {
		t.Fatalf("LoadedBytes 应等于索引正文长度：%d vs %d", load.LoadedBytes, len(joined))
	}

	// 拼上提示后注入文本变长，但**限额判定不因它改变**：重跑 boundIndex 结论相同。
	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inj.Text, truncationNoticeMarker) {
		t.Fatalf("前提失败：应带提示\n%q", inj.Text)
	}
	reload, _ := boundIndex(d, []byte(raw))
	if reload.TruncatedBy != load.TruncatedBy ||
		reload.LoadedLines != load.LoadedLines ||
		reload.LoadedBytes != load.LoadedBytes {
		t.Fatalf("提示改变了度量：%+v vs %+v", load, reload)
	}
	if inj.Load.LoadedBytes != load.LoadedBytes {
		t.Fatalf("提示计入了限额：%d vs %d", inj.Load.LoadedBytes, load.LoadedBytes)
	}
	// 反面：提示若被当成索引内容，下一次加载会因为它而更早触发字节限额。
	// 这里直接钉住这个性质——提示长度 + 索引正文 > MaxIndexBytes 时，
	// CheckIndex 依然只看文件本身。
	if err := (Config{}).CheckIndex(d); !isErr(err, ErrIndexTooLarge) {
		t.Fatalf("前提失败：CheckIndex 应报超限，err = %v", err)
	}
}

// 提示不得泄露任何被截掉的内容，也不暗示具体条目。
//
// 官方没有"报出丢了什么"的机制；点名会把**索引内容**塞进提示，
// 那既泄露记忆也让模型开始猜"还有哪几条"，同时会让提示长度随索引长度漂移。
func TestTruncationNoticeLeaksNoDroppedEntries(t *testing.T) {
	d := memDir(t)
	// 前 200 行是会被显示的，200 行之后是会消失的——名字分得干干净净。
	raw := buildLinesIndex(250)
	writeIndex(t, d, raw)

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	// 泄露检查的对象是**截断提示本身**，不包含其后的目录说明——说明段里出现
	// "25000"（官方字节限额）是规格的一部分，与"泄露丢了多少条"无关。
	notice := truncationNotice("line")
	// 截断提示与目录说明一起构成注入的末尾（说明在最后，见 guidance.go）。
	if !strings.HasSuffix(inj.Text, notice+"\n\n"+guidance(d, true)) {
		t.Fatalf("注入末尾不是常量提示：\n%q", inj.Text)
	}
	if !strings.Contains(inj.Text, notice) {
		t.Fatalf("注入缺少截断提示：\n%q", inj.Text)
	}
	// 提示正文是**常量**拼接（只随 truncatedBy 变化），不含任何索引派生的字符串。
	// 逐条核对：所有被丢掉的条目名都不得出现在提示里。
	for i := 0; i < 250; i++ {
		name := "- t" + strconv.Itoa(i) + ".md"
		if strings.Contains(notice, name) || strings.Contains(notice, "t"+strconv.Itoa(i)+".md") {
			t.Fatalf("提示泄露了条目名 %q：%q", name, notice)
		}
	}
	// 也不得给出条目数（那会暗示丢了多少条）。
	for _, n := range []string{"50", "49", "51"} {
		if strings.Contains(notice, n) {
			t.Fatalf("提示里出现条目数 %q：%q", n, notice)
		}
	}
	// 被显示的那些同样不该被复述一遍。
	if strings.Contains(notice, "t0.md") || strings.Contains(notice, "t198.md") {
		t.Fatalf("提示复述了清单内容：%q", notice)
	}
	// 提示只陈述三件事：被截断 / 哪个限额 / 可以自己读。不含祈使性的协议条款。
	for _, forbidden := range []string{"必须", "禁止", "不要", "你应该", "步骤", "首先", "然后"} {
		if strings.Contains(notice, forbidden) {
			t.Fatalf("提示含协议式措辞 %q：%q", forbidden, notice)
		}
	}
}

// 「无记忆」的各个退化状态一律不产生提示（哪怕文件超限）。
func TestTruncationNoticeNotAddedWhenIndexIsEmpty(t *testing.T) {
	d := memDir(t)
	for name, content := range map[string]string{
		"零字节":     "",
		"只有空白":    "   \n\n\t\n",
		"只有标题":    "# Project Memory\n",
		"超限但全是空行": strings.Repeat("\n", MaxIndexLines+50),
	} {
		writeIndex(t, d, content)
		inj, err := (Config{}).BuildInjection(d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if inj.Empty() || !strings.Contains(inj.Text, d.Path) {
			t.Fatalf("%s: 期望注入目录说明，得到 %q", name, inj.Text)
		}
		if inj.Reason != "empty-index" {
			t.Fatalf("%s: Reason = %q", name, inj.Reason)
		}
		// 索引无正文时**不得**出现截断提示：官方量的是文件，而"超限但全是空行"
		// 这类退化状态里没有任何条目可丢，提示会凭空造出一个不存在的截断。
		if strings.Contains(inj.Text, truncationNoticeMarker) {
			t.Fatalf("%s: 无记忆时不得有提示：%q", name, inj.Text)
		}
	}
}

// —————————————————————— 小工具 ——————————————————————

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func isErr(err, target error) bool { return errors.Is(err, target) }

func isValidUTF8(s string) bool { return utf8.ValidString(s) }
