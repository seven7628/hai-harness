package agents

// refextract.go —— @引用「本层读不出内容的格式」抽取钩子（当前仅 PDF）。
//
// 背景（2026-09 问题）：@引用 .pdf 时模型拿不到内容 —— PDF 结构层（%PDF-1.4 / 1 0 obj…）
// 是纯 ASCII，readRef 的二进制判定（NUL 字节）判不出来，于是把压缩流 PDF 报成
// 「二进制文件，不读内容」、把未压缩 PDF 的源码当正文注入。两种都不是用户要的：
// 用户要的是**文档里的文字**。
//
// 为什么用函数钩子而不是在 agents 层直接抽：抽取要真解析格式（pypdf + 受管 Python
// 运行时 + 沙箱），那是宿主的能力面；agents 包不认识 Python/办公格式，也不该
// import tools/runtime/sandbox（分层：库给事实，能力由宿主提供）。风格同
// tools/builtin 的 binaryHint / BashTool.BeforeExec / LoadSkillTool.WithOnLoad：
// 库定义协议与调用时机，实现由装配点（desktop/bridge）注入。
//
// 线程安全：抽取器由宿主实现，本层只按调用点顺序调用（同 @引用展开本身）。

import "context"

// RefExtractor 可选的引用内容抽取钩子：把「本层读不出内容的格式」（当前仅 PDF）
// 交给宿主抽取为文本。
//
// 三态协议（**为什么是三态**：调用方要对三种情形给出不同措辞，静默回退会让
// 「扫描件抽不到」与「运行时没装配」混成同一句，用户无法判断该重试还是该换个文件）：
//
//	(content, true,  nil)  → 抽到文本；content 作为 FileContent 正文（截断由调用方做）；
//	("",       false, nil) → 抽取器**可用**，但该文件没有文本层（扫描件/纯图 PDF）：
//	                         调用方须显式告知用户，不能静默；
//	("",       false, err) → 抽取器**不可用**（运行时未装配 / bootstrap 失败 / 解析报错）：
//	                         调用方回退到既有行为（"二进制文件，不读内容"），
//	                         避免为环境问题给出"这个 PDF 没文字"的错误结论。
//
// 用 (ok, err) 而不是单一哨兵值：哨兵只能编码第三种；用 err 承载环境失败是 Go 惯例
// （且 err 文本可供宿主日志），ok 专心表达「内容层有没有文字」这一业务事实。
//
// ctx 用于限时（首次调用可能触发受管运行时 bootstrap，慢；实现方自行设超时）。
// abs = 文件绝对路径。实现方不应假设写权限（只读抽取）。
type RefExtractor func(ctx context.Context, abs string) (content string, ok bool, err error)
