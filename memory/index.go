package memory

import (
	"errors"
	"io/fs"
	"strings"
	"unicode/utf8"
)

// 启动注入（技术方案 §5.2，Step 3）。
//
// 官方 memory#how-it-works 对这件事的全部规定：
//
//	"Claude doesn't load topic files such as user_role.md at startup.
//	 Claude reads them on demand using its standard file tools when it needs the information."
//	"…using MEMORY.md to keep track of what's stored where."
//
// 因此 Step 3 **只做一件事**：把 MEMORY.md 的前 200 行 / 25KB（取先到者）作为一段
// 系统提示词交给装配层。之后模型自己用标准 read_file 去读 topic 正文。
//
// 这里**不做**（官方没有，《技术方案》§4.5/§5.5）：
//   - 不逐条校验索引里的文件是否存在（官方无校验）；
//   - 不为索引行补 description / 摘要（官方索引是纯文件名列表）；
//   - 不排序索引（官方未规定；上一版的"近期优先"在 `modified` 不进索引行的前提下无法实现）；
//   - 不注入任何"记忆协议"文本（§2：上一版 6 条协议含两条自相矛盾，是 12%~62% 采纳率的来源）。
const (
	// MaxIndexLines 索引加载行数上限（官方 200 行）。
	MaxIndexLines = 200
	// MaxIndexBytes 索引加载字节上限（官方 25KB）。
	//
	// 取 25000 而不是 25*1024：官方原文写 "25KB"，Claude Code 自己按 25,000 计，
	// 取大了会在"官方会拦、我们不拦"的边界上与官方行为不一致。
	MaxIndexBytes = 25000
)

// ErrIndexTooLarge 索引超过限额，调用点应让模型重写索引。
//
// 与 §4.3 的语义对齐：**超限时写入本身不失败**（文件已由标准 write_file 落盘），
// 这里返回的是一条"请重写索引"的错误信息，用于附在工具结果里回给模型。
var ErrIndexTooLarge = errors.New("memory: MEMORY.md 超过 200 行 / 25KB 限额，本次加载已截断；请重写索引：每条一行、细节移到 topic 文件、合并或丢弃过期条目")

// IndexLoad 是「读到了索引」的度量结果（Step 3 的可观测面，便于评估与调试）。
type IndexLoad struct {
	// Dir 本次加载所属的记忆目录（<root>/repos/<id>）。
	Dir Dir
	// Path 索引文件绝对路径。
	Path string
	// Text 截断后的索引正文，**逐字节等于**注入段落去掉标题后的内容。
	Text string
	// TotalLines 索引总行数（截断前）。
	TotalLines int
	// TotalBytes 索引总字节数（截断前）。
	TotalBytes int
	// LoadedLines / LoadedBytes 实际进入提示词的量。
	LoadedLines int
	LoadedBytes int
	// TruncatedBy "line" / "byte" / "" —— 哪个限额先到。
	TruncatedBy string
}

// Injection 是 Step 3 的产物：一段可直接拼进系统提示词的文本。
//
// 结构体而非 string 是为了让装配层不必重新解析自己的输出判断"有没有内容"
// （Empty 字段即答案），也便于测试断言边界。
type Injection struct {
	// Text 注入文本。Enabled=false / 无索引 / 索引为空时**恒为 ""**——
	// "无记忆时 prompt 逐字节不变"（§5.2）是硬要求。
	Text string
	// Enabled 解析后的开关值。false 时 Text 必为 ""。
	Enabled bool
	// Dir 解析出的记忆目录（Enabled=false 或解析失败时为 nil）。
	Dir *Dir
	// Load 索引加载度量（未读到索引时为 nil）。
	Load *IndexLoad
	// Reason 未注入的原因标识（无注入时非空）："disabled" / "no-identity" / "no-index" / "empty-index"。
	Reason string
}

// Empty 无注入。装配层据此**不改动系统提示词的任何字节**。
func (i Injection) Empty() bool { return strings.TrimSpace(i.Text) == "" }

// indexHeading 注入段落的标题行。
//
// 标题是必需的：段落拼进系统提示词后，模型需要知道"这一段是记忆索引"而不是文档片段。
// 但**只有这一行**——官方不向模型追加记忆协议（§2），索引本身就是全部引导，
// 剩下的引导由「索引行 + 模型判断」承担。
const indexHeading = "# Project Memory (MEMORY.md)"

// BuildInjection 读该记忆目录的索引并组装注入段落（Dir 已知时的入口）。
//
// 返回 Injection：不存在 / 空 → Empty()==true、Reason 非空、**err 为 nil**
// （官方 "If a file doesn't exist, no error" —— 绝大多数会话都没有记忆，不该报错）。
// 真正的读盘错误（权限、IO）才返回 err，交给装配层决定是否记日志。
func (c Config) BuildInjection(d Dir) (Injection, error) {
	c = c.withDefaults()
	if !c.enabled() {
		// 开关前置判定发生在任何 I/O 之前：关闭时**一次读盘都不发生**。
		return Injection{Enabled: false, Reason: "disabled"}, nil
	}
	return c.buildInjection(d)
}

func (c Config) buildInjection(d Dir) (Injection, error) {
	path := joinIndex(d)
	raw, err := c.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			// 官方：索引不存在就不注入、不报错（§5.2 明文）。
			return Injection{Enabled: true, Dir: &d, Reason: "no-index"}, nil
		}
		return Injection{Enabled: true, Dir: &d}, err
	}
	load, text := boundIndex(d, raw)
	if strings.TrimSpace(text) == "" {
		// 空索引（如模型建了 MEMORY.md 但还没写条目）：同样不注入。
		// 注入一个空标题只会占 context 并给模型一个"记忆系统已就绪"的错觉。
		return Injection{Enabled: true, Dir: &d, Reason: "empty-index"}, nil
	}
	return Injection{
		Text:    indexHeading + "\n\n" + text,
		Enabled: true,
		Dir:     &d,
		Load:    load,
	}, nil
}

// boundIndex 把索引正文截到 MaxIndexLines 行 / MaxIndexBytes 字节，取先到者。
//
// 官方（memory#how-it-works）：
//
//	"Claude Code measures the file against the 200-line and 25KB read limits.
//	 If the file is near a limit… If the file is over a limit, the write still
//	 succeeds, but Claude Code returns an error telling Claude to rewrite the index,
//	 because everything past the limit is dropped on the next load."
//
// 即**超限部分下次加载时被直接丢弃**，所以截断必须是"按行切 + 按字节切"两遍独立进行，
// 取先到者，而不是先按行再按字节串行（那会因行截断后变短而错过字节限额）。
func boundIndex(d Dir, raw []byte) (*IndexLoad, string) {
	load := &IndexLoad{
		Dir:        d,
		Path:       joinIndex(d),
		TotalBytes: len(raw),
		TotalLines: countLines(string(raw)),
	}

	// — 字节限额 —（先切字节：25KB 常比 200 行先到）
	b := raw
	if len(b) > MaxIndexBytes {
		b = b[:MaxIndexBytes]
		// 官方说"everything past the limit is dropped"，即硬切。硬切可能落在多字节
		// rune 中间，产出的字符串会带 U+FFFD；这里退到最近的完整 rune 边界，
		// 保证注入文本永远 UTF-8 合法（非法字节进 prompt 会让 provider 报错）。
		b = trimPartialRune(b)
		load.TruncatedBy = "byte"
	}

	text := string(b)
	// — 行限额 —
	// 按 "\n" 切、去掉末尾空段：官方示例的索引以单个换行结尾，若按 Split 的末段
	// 算行数会把"文件结尾换行"误判成多一行，白白少载一条记忆。
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > MaxIndexLines {
		lines = lines[:MaxIndexLines]
		load.TruncatedBy = "line"
	}
	// 截断完成后再做字节计数：报告的必须是**真正进了 prompt 的量**，
	// 而不是"截断前那一坨有多少字节"——后者在行限额先触发时会偏大。
	joined := strings.Join(lines, "\n")
	load.LoadedLines = len(lines)
	load.LoadedBytes = len(joined)

	// 索引自带的标题行与宿主加的标题重复，注入前去掉（见 stripIndexHeading）。
	// LoadedLines/LoadedBytes 保持为"从文件里加载了多少"——官方量的是文件本身，
	// 标题是注入这一步的呈现处理，不该改写这个数字。
	return load, stripIndexHeading(joined)
}

// stripIndexHeading 去掉索引正文**首个** markdown 标题行及其后的空行。
//
// 为什么做这一件事（它看起来像自创，所以把理由写全）：
//
//  1. 宿主要注入的段落本来就会带一行标题（模型需要知道这段是记忆清单）。官方示例
//     的 MEMORY.md 第一行也是 `# Project Memory`——两份标题叠在一起纯属浪费 context。
//  2. 它让"索引只有标题、没有条目"这种退化状态能被判为**空**：否则宿主会注入一段
//     只有标题的空清单，既占 context 又让模型误以为"记忆系统已就绪"。
//     §5.2 的硬要求是「无记忆时 prompt 逐字节不变」，这个退化状态必须算"无记忆"。
//  3. 顺序上它发生在**限额判定之后**：官方量的是文件本身，所以 TotalLines/TotalBytes
//     与截断都按原文件算，标题只影响最终进入 prompt 的那一份文本。
//
// 只剥**第一个非空行**的标题，且只认 ATX 标题（`#`…）；正文里的 `#` 标题不动
// （模型可能把索引写成按主题分组的多级列表）。
func stripIndexHeading(s string) string {
	lines := strings.Split(s, "\n")
	i := 0
	for ; i < len(lines); i++ {
		t := strings.TrimSpace(strings.TrimRight(lines[i], " \t\r"))
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			rest := t[1:]
			if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
				i++ // 跳过标题行本身
			}
		}
		break
	}
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			break
		}
	}
	return strings.Join(lines[i:], "\n")
}

// CheckIndex 测量**已落盘**的索引是否超限，供 write_file 钩子在写后调用（§4.3）。
//
// 官方（memory#how-it-works）只定义了超限这一种返回：
//
//	"If the file is over a limit, the write still succeeds, but Claude Code
//	 returns an error telling Claude to rewrite the index, because everything
//	 past the limit is dropped on the next load."
//
// 所以本函数**只判超限**：在限额内一律返回 nil，接近限额时的精简提醒由
// NearIndexNotice 单独给出（那是非错误提示，宿主自行决定要不要附在工具结果后）。
//
// 官方那条 "near a limit → reminds Claude to shorten it" 的提醒由谁在何时发出，
// 官方文档没有规定触发点，本轮**不接**（§4.5：不得自行发明）；NearIndexNotice 作为
// 可调用入口留着，接线时挂到 write_file 钩子即可。
//
// 索引不存在返回 nil：官方对索引缺失的规定是"不注入不报错"，写后测量同理。
func (c Config) CheckIndex(d Dir) error {
	raw, err := c.withDefaults().ReadFile(joinIndex(d))
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	load, _ := boundIndex(d, raw)
	if load.TruncatedBy != "" {
		return ErrIndexTooLarge
	}
	return nil
}

// NearIndexNotice 接近限额时给模型的精简提醒（官方 memory#how-it-works 的原话口径）。
//
// 阈值取「用量 ≥ 限额的四分之三」。官方没有给数字（只说 "near a limit"），
// 这里是产品侧的一个具体取值，写出来是为了让它可测、可争论，而不是藏在代码里。
func NearIndexNotice(d Dir) string {
	raw, err := (Config{}).withDefaults().ReadFile(joinIndex(d))
	if err != nil {
		return ""
	}
	load, _ := boundIndex(d, raw)
	nearLines := MaxIndexLines*3/4 <= load.TotalLines
	nearBytes := MaxIndexBytes*3/4 <= load.TotalBytes
	if !nearLines && !nearBytes {
		return ""
	}
	return "MEMORY.md 接近 200 行 / 25KB 加载上限：请精简索引——每条一行、细节移到 topic 文件、合并或丢弃过期条目。"
}

// joinIndex 索引文件路径（d.Path 已是 Clean 过的绝对路径，直接用 "/" 拼即可）。
func joinIndex(d Dir) string { return d.Path + "/" + IndexFileName }

// countLines 统计行数（等价 wc -l：结尾有换行时不算多出空行；无换行结尾也计 1 行）。
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// trimPartialRune 把可能被切断的多字节 rune 退掉。
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i > len(b)-utf8.UTFMax-1; i-- {
		if utf8.RuneStart(b[i]) {
			r, size := utf8.DecodeLastRune(b[:i+1])
			if r == utf8.RuneError && size <= 1 {
				// i 之后是不完整的尾部字节，退到 i。
				return b[:i]
			}
			return b
		}
	}
	return b
}

// isNotExist 判定「文件不存在」。fs.ErrNotExist 覆盖 os / iofs 两层的 ENOENT。
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
