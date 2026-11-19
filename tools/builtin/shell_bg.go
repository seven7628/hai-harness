package builtin

// 命令末尾「后台运算符 &」的识别与改写（2026-11-18 资源有界化）。
//
// 为什么需要：模型表达「我要一个后台进程」最自然的写法是**命令末尾加 `&`**
// （shell 惯例，Claude Code 也支持 `cmd &`）。而我们的执行纪律是
// 「命令结束 ⇒ 收掉它留下的进程组」⇒ 不做转换的话，`cmd &` 里的 cmd 会被杀掉 ⇒
//
//	**模型以为服务起来了、其实已经死了** —— 命令返回 0、输出为空、端口没了。
//	这是最难查的一类静默失败（工具层看不出任何异常）。
//
// 改写：**末尾的 `&` ⇒ 等价于 `run_in_background: true`**（起独立进程组、
// 立即返回 pid + 日志路径）。语义上比裸 `&` 更好：
//   - stdout 写日志文件而不是占住管道（裸 `&` 会让执行层等 3s 的 WaitDelay）；
//   - 模型拿到 pid 与日志路径（能查状态、能停它）。
//
// 保守判定（**宁可漏改，不要改错** —— 改错 = 执行了用户没批准的语义）：
//   - 引号内（' ' " `）的 & 不算；
//   - 反斜杠转义的 & 不算；
//   - `&>` / `>&` 里的 & 是重定向，不算；
//   - `&&` 不是后台运算符（末尾的 && 是语法错误，不该由我们"顺手修"）；
//   - 只认**末尾**那个（中间的 `a & b` 是「a 后台、b 前景」，不归我们管）。

import "strings"

// splitTrailingBackground 若命令末尾是「后台运算符 &」，返回去掉它之后的命令
// 与 true；否则返回原命令与 false。
func splitTrailingBackground(cmd string) (string, bool) {
	// ① 去掉末尾的空白与 `;`（`cmd & ;` 也是合法的后台写法）。
	trimmed := strings.TrimRight(cmd, " \t\n;")
	if trimmed == "" {
		return cmd, false
	}
	// ② 找最后一个**顶层**的 &（跳过引号与转义）。
	pos := -1
	var inSingle, inDouble, inBacktick, escaped bool
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
			continue
		case c == '\'' && !inDouble && !inBacktick:
			inSingle = !inSingle
			continue
		case c == '"' && !inSingle && !inBacktick:
			inDouble = !inDouble
			continue
		case c == '`' && !inSingle && !inDouble:
			inBacktick = !inBacktick
			continue
		}
		if inSingle || inDouble || inBacktick {
			continue
		}
		if c == '&' {
			pos = i
		}
	}
	// ③ 判定：必须是**末尾**那个、且不是 `&&`、且去掉后还剩东西。
	if pos < 0 || pos != len(trimmed)-1 {
		return cmd, false
	}
	if pos > 0 && trimmed[pos-1] == '&' {
		return cmd, false // `&&` ⇒ 不是后台运算符
	}
	rest := strings.TrimRight(trimmed[:pos], " \t\n")
	if rest == "" {
		return cmd, false // 只有 `&` ⇒ 空命令，不动
	}
	return rest, true
}
