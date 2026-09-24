package agents

// fileref.go —— @引用展开（Client 保留原文 → AgentHarness 层在拼接 LLM Messages 前展开）。
//
// 背景（2026-08 优化项）：客户端发送时不再把 @./docs/text.txt 展开成文档内容，而是原文发送；
// 历史回显保持 "@./docs/text.txt"。展开动作下沉到本层：每次 LLM 请求前对消息副本展开，
// 不改 ac.Messages（持久化/回显/恢复永远是原文）。
//
// 语义（与旧客户端 Composer.expandRefs 对齐，含测试用例定稿）：
//  ① 词边界：@ 前必须是行首或空白（否则为普通文本，如 email foo@bar、日志 @mention）
//  ② 转义：  \@  → 字面 @，不展开（去反斜杠）
//  ③ 路径形态：./  ../  /  盘符 开头（否则 = 普通文本，不展开）
//  ④ 相对路径以工作区为基准解析；%20 解码为空格；URL 中 ?# 视为文件名一部分（不截断）
//  ⑤ 合法性：仅敏感路径拒绝（~/.go-code、.ssh、.aws 等）。工作区外**已放开**（2026-09-17 决策：
//     用户上传/拖拽的文件常在 ~/Desktop、~/Downloads，按工作区 containment 拒绝会让「添加文件」
//     功能直接失效）；工作区内的相对引用语义不变。
//  ⑥ 兜底：  文件/目录不存在 → 保留原文（用户可看到原文修正，不注入失败占位）
//  ⑦ 成功：  文件 → FilePath 块（截断/二进制检测）；目录 → Dir 块（递归清单 cap 300）
//  ⑧ PDF：   扩展名 .pdf 且注入了 RefExtractor 时走抽取（见 extractRefBody）——PDF 结构层
//     是纯 ASCII，②的 NUL 判定对「压缩流 PDF」判成二进制、对「未压缩 PDF」判成文本源码，
//     两种都读不到正文；抽取失败/未注入一律回退既有行为。
//
// 注：历史消息（已展开成 FilePath:/Dir: 文本）不做兼容处理——按原文发送，与本次改动无关。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// 与旧客户端 desktop/app/electron/main.ts 对齐的引用常量。
const (
	maxRefFileChars  = 64 * 1024 // 单文件引用读取上限（字符；超限截断并标注）
	maxRefDirEntries = 300       // 单目录引用文件清单上限（递归；超限截断并标注）
)

// parseRefs 从输入文本解析出所有需要展开的 @token（去重，按出现顺序）。
// 判定逻辑（①词边界 ②转义 ③路径形态）与客户端完全一致。
// 返回的 token 不含前导 @；\@ 转义 token 不在返回中（消费者自行去转义）。
func parseRefs(input string) []string {
	var out []string
	seen := make(map[string]bool)
	// 以 @ 分段扫描：每段检查前导（词边界/转义），段内取首个空白前的 token。
	segments := strings.Split(input, "@")
	for i, seg := range segments {
		if i == 0 {
			continue // 首个 @ 之前无前缀可判断
		}
		// 前缀：上一个 segment 的最后一个字符（词边界判断）；特殊处理行首。
		var prev byte
		if i == 1 && len(segments[0]) == 0 {
			prev = 0 // 行首
		} else if len(segments[i-1]) > 0 {
			prev = segments[i-1][len(segments[i-1])-1]
		}
		if prev != 0 && prev != ' ' && prev != '\t' && prev != '\n' && prev != '\r' {
			continue // ① 非词边界：email / @mention 等
		}
		// ② 转义：\@（前一个字符是反斜杠）
		if prev == '\\' {
			continue
		}
		// 取 token：首个空白（含 \t \n \r）前
		end := len(seg)
		for j := 0; j < len(seg); j++ {
			if seg[j] == ' ' || seg[j] == '\t' || seg[j] == '\n' || seg[j] == '\r' {
				end = j
				break
			}
		}
		tok := seg[:end]
		if tok == "" {
			continue
		}
		// ③ 路径形态：./ ../ / 盘符 开头（与客户端 /^(\.\.?\/|\/|[A-Za-z]:[\\/])/ 一致）
		if !isPathShape(tok) {
			continue
		}
		if seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// isPathShape 判断 token 是否为「显式路径形态」。
// 与客户端 Composer.expandRefs 的正则 /^(\.\.?\/|\/|[A-Za-z]:[\\/])/ 对齐：
// ./  ../  /  盘符 开头。%20 空格不参与判定（token 以空白为界，%20 解码在后）。
func isPathShape(tok string) bool {
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") || strings.HasPrefix(tok, "/") {
		return true
	}
	if len(tok) >= 3 && isDriveLetter(tok[0]) && (tok[1] == ':') && (tok[2] == '\\' || tok[2] == '/') {
		return true
	}
	return false
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// unescapeToken \@ → @（转义展开处使用）。
func unescapeToken(tok string) string {
	return strings.ReplaceAll(tok, "\\@", "@")
}

// resolveRef 把 @token 解析为绝对路径（④：相对以工作区为基准；%20 解码为空格）。
// 非绝对路径（./ ../ 相对形态）在无工作区时无法解析 → 返回 ("", false)（保留原文）。
func resolveRef(tok, workspace string) (string, bool) {
	t := strings.ReplaceAll(tok, "%20", " ") // %20 解码为空格
	if filepath.IsAbs(t) || isAbsWin(t) {
		return filepath.Clean(t), true
	}
	if workspace == "" {
		return "", false
	}
	base := filepath.Clean(workspace)
	// 去前导 ./ （../ 保留向上语义，后续 containment 校验兜底）
	rel := strings.TrimPrefix(t, "./")
	return filepath.Join(base, rel), true
}

func isAbsWin(p string) bool {
	return len(p) >= 3 && isDriveLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// sensitivePath 敏感路径判定（⑤）：home 下常见密钥/配置目录 + go-code 自身配置目录。
// 对齐 Electron main.ts isSensitiveFsPath + tools/builtin denyHarnessConfigPath 语义。
func sensitivePath(abs string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	clean := filepath.Clean(abs)
	// go-code 自身配置/密钥目录（~/.go-code；plugins/runtime 子树放行）
	cfgDir := filepath.Join(home, ".go-code")
	if clean == cfgDir || strings.HasPrefix(clean, cfgDir+string(os.PathSeparator)) {
		// 受管子树放行（与 tools/builtin.denyHarnessConfigPath 同步）：plugins 为受管
		// 组件目录、runtime 为受管 Python 运行时（~/.go-code/runtime/python），
		// 两者都只含公开内容、无密钥。
		for _, sub := range []string{"plugins", "runtime"} {
			dir := filepath.Join(cfgDir, sub)
			if clean == dir || strings.HasPrefix(clean, dir+string(os.PathSeparator)) {
				return false
			}
		}
		return true
	}
	// 常见敏感凭据目录（与 Electron isSensitiveFsPath 对齐）
	for _, name := range []string{".ssh", ".aws", ".gnupg", ".netrc", ".kube"} {
		base := filepath.Join(home, name)
		if clean == base || strings.HasPrefix(clean, base+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// expandUserRefs 展开输入文本中的 @引用（核心入口）。
// 逐 token 判定 ⑤⑥⑦；失败一律保留原文（⑥ 兜底）。
// 返回展开后的文本与逐引用结果（供 refs_loaded 事件批量上报；调用方可忽略）。
// ext = 可选内容抽取钩子（见 refextract.go）；nil = 不抽取（PDF 回退到既有二进制行为）。
func expandUserRefs(input, workspace string, ext RefExtractor) string {
	expanded, _ := expandUserRefsWithItems(input, workspace, ext)
	return expanded
}

// refResult 单个引用的展开过程结果（内部结构，事件层再映射为 events.RefLoadedItem）。
type refResult struct {
	tok    string // 原始 token（不含 @）
	abs    string
	status string // "loaded" / "missing" / "blocked"
	kind   string // "file" / "dir"（仅 loaded）
}

// expandUserRefsWithItems 展开并返回逐引用结果。ext 见 expandUserRefs。
func expandUserRefsWithItems(input, workspace string, ext RefExtractor) (string, []refResult) {
	if !strings.Contains(input, "@") {
		return input, nil
	}
	toks := parseRefs(input)
	if len(toks) == 0 {
		return input, nil
	}
	repl := make(map[string]string, len(toks))
	var items []refResult
	for _, tok := range toks {
		abs, ok := resolveRef(tok, workspace)
		if !ok {
			items = append(items, refResult{tok: tok, status: "blocked"}) // 无工作区相对路径：保留原文
			continue
		}
		if sensitivePath(abs) {
			items = append(items, refResult{tok: tok, abs: abs, status: "blocked"}) // ⑤ 敏感路径（安全边界，不放）
			continue
		}
		// ⑤ 工作区外不再拒绝（2026-09-17 决策）：用户上传的文件常在 ~/Desktop、~/Downloads，
		// 按 containment 拒绝会让「添加文件/拖拽文件」这条通路整体失效。敏感路径闸门仍在上面。
		content, ok := readRef(abs, workspace, ext)
		if !ok {
			items = append(items, refResult{tok: tok, abs: abs, status: "missing"}) // ⑥ 不存在
			continue
		}
		kind := "file"
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			kind = "dir"
		}
		items = append(items, refResult{tok: tok, abs: abs, status: "loaded", kind: kind})
		repl["@"+tok] = content
	}
	if len(repl) == 0 {
		return input, items
	}
	// 原文重建：@ 分段替换（保留未展开 token 与 \@ 转义处理）。
	var b strings.Builder
	segments := strings.Split(input, "@")
	for i, seg := range segments {
		if i > 0 {
			// 判定此 @ 是否展开：与 parseRefs 相同的前缀/形态判定
			var prev byte
			if i == 1 && len(segments[0]) == 0 {
				prev = 0
			} else if len(segments[i-1]) > 0 {
				prev = segments[i-1][len(segments[i-1])-1]
			}
			expandable := prev == 0 || prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r'
			if expandable && prev != '\\' {
				end := len(seg)
				for j := 0; j < len(seg); j++ {
					if seg[j] == ' ' || seg[j] == '\t' || seg[j] == '\n' || seg[j] == '\r' {
						end = j
						break
					}
				}
				tok := seg[:end]
				if tok != "" && isPathShape(tok) {
					if v, ok := repl["@"+tok]; ok {
						b.WriteString(v)
						b.WriteString(seg[end:])
						continue
					}
				}
			}
			b.WriteByte('@')
		}
		b.WriteString(seg)
	}
	return b.String(), items
}

// withinWorkspace 路径 containment 判定：workspace 非空且 target 在其（含子目录）内 → true。
//
// 注意：此判定**已不再作为 @引用的准入闸门**（2026-09-17 决策放开工作区外引用；见文件头 ⑤）。
// 仅保留为几何工具函数——relToWorkspace 等展示/统计逻辑与既有测试仍按其语义使用。
// 安全边界由 sensitivePath 单独把守。
func withinWorkspace(target, workspace string) bool {
	if workspace == "" {
		return true // 无工作区：不限制（绝对路径原样）
	}
	ws := filepath.Clean(workspace)
	t := filepath.Clean(target)
	rel, err := filepath.Rel(ws, t)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// readRef 读取引用目标：文件 → FilePath 块；目录 → Dir 块（递归清单）。
// ok=false 表示失败（保留原文）。对齐 Electron readFsRef 输出格式。
// ext = 可选的格式抽取钩子（当前仅 PDF 用）；nil = 全部走既有文本/二进制判定。
func readRef(abs, workspace string, ext RefExtractor) (string, bool) {
	st, err := os.Stat(abs)
	if err != nil {
		return "", false
	}
	if st.IsDir() {
		var lines []string
		count := 0
		var walk func(dir string)
		walk = func(dir string) {
			if count >= maxRefDirEntries {
				return
			}
			names, err := os.ReadDir(dir)
			if err != nil {
				return
			}
			// 排序（os.ReadDir 已按文件名排序）
			for _, n := range names {
				if count >= maxRefDirEntries {
					return
				}
				full := filepath.Join(dir, n.Name())
				ist, err := os.Stat(full)
				if err != nil {
					continue
				}
				if ist.IsDir() {
					if n.Name() == ".git" || n.Name() == "node_modules" {
						continue
					}
					walk(full)
				} else if ist.Mode().IsRegular() {
					lines = append(lines, "  "+relToWorkspace(workspace, full))
					count++
				}
			}
		}
		walk(abs)
		if count >= maxRefDirEntries {
			lines = append(lines, fmt.Sprintf("  [目录清单已截断: >%d 个文件]", maxRefDirEntries))
		}
		head := "Dir: " + relToWorkspace(workspace, abs) + "\nFiles:\n"
		if len(lines) == 0 {
			return head + "  (空目录)", true
		}
		return head + strings.Join(lines, "\n"), true
	}
	if st.Mode().IsRegular() {
		// ⑧ PDF 抽取：在二进制判定**之前**按扩展名分流 —— PDF 结构层（%PDF-1.4 / 1 0 obj）
		// 是纯 ASCII，NUL 判定对压缩流 PDF 判成「二进制，不读内容」、对未压缩 PDF 判成
		// 「文本」把源码注入正文，两条都读不到文档正文（本次修复的起因）。
		//
		// 三种结果（见 RefExtractor 的三态协议）分别落到：
		//   ok=true         → FileContent 用抽出的正文（refFileBlock，截断口径共用）；
		//   !ok && err==nil → 抽取器可用但**没有文本层**（扫描件/纯图 PDF）：显式告知，
		//                     不静默给空正文（模型会以为文件为空，转而去怀疑读取坏了）；
		//   err!=nil        → 抽取器不可用（运行时 bootstrap 失败/解析报错/沙箱拦截）：
		//                     给既有二进制占位文案 —— **不能**落到下面的旧文本路径，
		//                     否则未压缩 PDF 会把 %PDF-1.4 / 1 0 obj 源码当正文注入
		//                     （本次修复的另一个起因）。
		// ext == nil（宿主未装配）时不进本分支：完全沿用旧行为（「本层不认识该格式」），
		// 这是宿主回退到改动前语义的确定性保证。
		if ext != nil && strings.EqualFold(filepath.Ext(abs), ".pdf") {
			body, ok, err := extractRefBody(abs, ext)
			switch {
			case err == nil && ok:
				return refFileBlock(workspace, abs, body), true
			case err == nil && !ok:
				return refFileBlock(workspace, abs, pdfNoTextLayer), true
			default:
				return refBinaryBlock(workspace, abs), true
			}
		}
		buf, err := os.ReadFile(abs)
		if err != nil {
			return "", false
		}
		if bytes.IndexByte(buf, 0) >= 0 {
			return refBinaryBlock(workspace, abs), true
		}
		return refFileBlock(workspace, abs, string(buf)), true
	}
	return "", false // 既不是普通文件也不是目录
}

// PDF 抽取相关的文案与时限。
const (
	// pdfNoTextLayer 抽取器可用但文件没有文本层、**且渲染也不可用**时的正文。
	//
	// 措辞对齐库内既有「不静默丢弃」口径（对应用户已拍板的「抽不到要告知」）。
	//
	// 2026-09 修订两处（原措辞已与实际能力不符）：
	//   - 删掉「请用 OCR 工具」：本产品**没有** OCR 工具（Work persona 无 shell、无 OCR 技能），
	//     这句话会把模型指向一个不存在的能力 —— 比不给建议更糟（模型会去 load_skill 找一个
	//     永远找不到的东西，然后卡在"工具不可用"上）。
	//   - 补「也未能渲染为图片」：宿主装配了渲染器时，扫描件走的是**渲染成图片**这条路
	//     （bridge 侧 pypdfium2）；能走到本文案说明渲染也没成功（运行时缺 pypdfium2 /
	//     渲染报错 / 无可用输出目录）。不写清楚，模型会以为"文件只有图片"而不知道
	//     我们还试过另一条路并失败了 —— 那样它可能反复重试同一个引用。
	pdfNoTextLayer = "[该 PDF 无文本层（可能是扫描件或整页图片），无法提取文字内容；本次也未能把它渲染为图片。如需内容，请让用户提供文本版本，或把关键页的文字贴出来。]"
	// refBinaryPlaceholder 二进制占位文案（与改动前逐字一致，勿改：模型与前端提示词
	// 都按这句组织后续动作）。
	refBinaryPlaceholder = "[二进制文件，不读内容]"
	// extractRefBackstop 抽取调用的兜底时限（**不是**性能目标）：真正的时间预算是
	// 抽取器内部的两段 —— 受管运行时 bootstrap（建 venv + 装依赖，慢网分钟级）
	// 与一次抽取（秒级）；本值只防「子进程挂死不返回」。理由同 run_python 的
	// bootstrapHeadroom：首次引用 PDF 会顺带把运行时装起来，按「抽取耗时」掐 ctx
	// 会让首次调用在 bootstrap 中途被取消、报成看不懂的错误。
	extractRefBackstop = 12 * time.Minute
)

// extractRefBody 调一次抽取钩子（带兜底时限）。
// ctx 取自 context.Background() + 兜底时限：readRef 是纯函数式的读路径（无 ctx 参数，
// 被 4 层展开函数与 2 处生产调用点共用），为它透传 ctx 要改 6 个签名、6 处测试调用点，
// 而调用点手上也没有更合适的 ctx（消息展开不属任何可取消的运行阶段）；限时的真实
// 责任在抽取器实现侧（它知道自己哪一段慢），这里只是防挂死的兜底。
func extractRefBody(abs string, ext RefExtractor) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), extractRefBackstop)
	defer cancel()
	return ext(ctx, abs)
}

// refFileBlock 组装 FilePath/FileContent 块（含 maxRefFileChars 截断与标注）。
// 文本分支与 PDF 抽取分支共用同一份截断口径 —— 改这里两处同时生效，避免两套阈值。
func refFileBlock(workspace, abs, content string) string {
	if len(content) > maxRefFileChars {
		content = content[:maxRefFileChars] + fmt.Sprintf("\n[文件内容已截断: >%d 字符]", maxRefFileChars)
	}
	return "FilePath: " + relToWorkspace(workspace, abs) + "\nFileContent:\n" + content
}

// refBinaryBlock 二进制占位块（两条通路共用：NUL 判定与「PDF 抽取器不可用」回退）。
func refBinaryBlock(workspace, abs string) string {
	return "FilePath: " + relToWorkspace(workspace, abs) + "\nFileContent: " + refBinaryPlaceholder
}

// expandUserRefsInMessages 对消息列表里的 user 文本块做 @引用展开（请求副本用）。
// 只展开 role=user 且为纯文本（text 块）的消息；image 等其他块不碰。
// 全部引用展开结果合并成一次 refs_loaded 事件批量发出（空结果不发）。
// emitEvent=false 时（压缩输入展开）只展开、不发事件——refs_loaded 只应由
// 真正发给 LLM 的请求（streamOnce）发，避免同一输入每轮重复通知。
// ext = 内容抽取钩子（从 a.cfg.RefExtractor 透传；nil = 不抽取）。**不放进 AgentContext**：
// 它是 loop 级配置（Config）而非 per-Run 运行态，且 ac 无法在测试里构造出 cfg ——
// 加参数（2 处生产调用点显式传 a.cfg.RefExtractor）比给 ac 挂一个 cfg 后门更诚实。
func expandUserRefsInMessages(ac *AgentContext, msgs []core.Message, workspace string, ext RefExtractor) {
	expandUserRefsInMessagesEmit(ac, msgs, workspace, ext, true)
}

func expandUserRefsInMessagesEmit(ac *AgentContext, msgs []core.Message, workspace string, ext RefExtractor, emitEvent bool) {
	if ac == nil || len(msgs) == 0 {
		return
	}
	var allItems []events.RefLoadedItem
	for i := range msgs {
		if msgs[i].Role != core.User {
			continue
		}
		// 跳过内部注入消息（task_result / summary / command 等非 text 块）——
		// 只对「用户输入」的 text 块展开，避免误展开系统注入内容。
		if len(msgs[i].Content) != 1 || msgs[i].Content[0].Type != core.ContentTypeText {
			continue
		}
		text := msgs[i].Content[0].Content
		expanded, items := expandUserRefsWithItems(text, workspace, ext)
		if expanded == text {
			continue
		}
		msgs[i].Content = []core.Content{{Type: core.ContentTypeText, Content: expanded}}
		for _, it := range items {
			allItems = append(allItems, events.RefLoadedItem{
				Path:     "@" + it.tok,
				Status:   events.RefStatus(it.status),
				Resolved: it.abs,
				Kind:     it.kind,
			})
		}
	}
	if emitEvent && len(allItems) > 0 {
		ac.Handler(ac.ctx, &events.RefsLoaded{
			RunId:     ac.RunId,
			Items:     allItems,
			Timestamp: time.Now(),
			EventType: events.RefsLoadedType,
		})
	}
}

// relToWorkspace 绝对路径 → 工作区相对展示（./xxx）；工作区外保持绝对路径。
// 对齐 Electron relToWorkspace。
func relToWorkspace(ws, abs string) string {
	if ws != "" {
		if r, err := filepath.Rel(ws, abs); err == nil {
			if r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator)) && !filepath.IsAbs(r) {
				return "./" + strings.ReplaceAll(r, string(os.PathSeparator), "/")
			}
		}
	}
	return abs
}
