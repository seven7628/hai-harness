package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// PreviewScriptFingerprint 三个预览转换脚本（编译期常量）的内容指纹。
//
// 唯一用途：转换服务的**缓存键**里「转换器版本」这一位
// （desktop/bridge/conversion.CacheKey）。产物长什么样只由脚本决定，所以「脚本变了」
// 就必须换键；否则升级后用户仍命中上一版脚本产出的 HTML，表现为「更新了却什么都没变」，
// 且没有任何报错。
//
// 为什么不用 app 版本号 / vcs revision 顶替：那要靠人记得 bump，而本地 `go run`、
// `buildvcs=false`、改了脚本还没提交这三种情况下 revision 根本不变 —— 正好漏掉要防的场景。
// 脚本是常量，算一次 sha256 成本为零，语义精确。
var previewScriptFingerprint = scriptFingerprint(xlsxToHTMLScript, docxToHTMLScript, pptxToHTMLScript)

// PreviewScriptFingerprint 见 previewScriptFingerprint。
func PreviewScriptFingerprint() string { return previewScriptFingerprint }

// scriptFingerprint 对脚本内容算短指纹（前 16 个十六进制字符 = 64 bit，够防碰撞，
// 又能让缓存目录名短一点）。
//
// 分隔符不是装饰：直接把几个脚本拼起来，删掉脚本 A 的末行 + 加上脚本 B 的首行可能
// 得到同一个拼接串；逐个写 0 字节就不会有跨脚本的字节级歧义。
func scriptFingerprint(scripts ...string) string {
	h := sha256.New()
	for _, s := range scripts {
		_, _ = io.WriteString(h, s)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
