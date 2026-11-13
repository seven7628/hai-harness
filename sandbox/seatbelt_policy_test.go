//go:build darwin

package sandbox

import (
	"strings"
	"testing"
)

// 预览产物缓存（~/.go-code/previews）在沙箱侧必须是「读开、写关」。
//
// 为什么审这条字符串：策略是拼出来的 SBPL，**顺序即语义**（后规则胜）。写错位置的
// 后果不是报错而是静默放行 —— 例如把只读子树也塞进 OpenSubpaths，读能过、写也能过，
// 一切看起来都正常，直到有人在缓存里换掉 index.html。
//
// 断言对象是 buildPolicy 的输出（纯函数），不起子进程、不依赖 sandbox-exec 可用。
func TestSeatbeltPolicyMakesPreviewsReadOnly(t *testing.T) {
	home := "/tmp/policy-home"
	previews := "/tmp/policy-home/.go-code/previews"
	s := &Seatbelt{
		Home:             home,
		Workspace:        "/tmp/policy-ws",
		Sensitive:        []string{"/tmp/policy-home/.go-code"},
		OpenSubpaths:     []string{"/tmp/policy-home/.go-code/plugins"},
		ReadOnlySubpaths: []string{previews},
	}
	pol := s.buildPolicy()

	readAllow := `(allow file-read* (subpath "` + previews + `"))`
	writeDeny := `(deny file-write* (subpath "` + previews + `"))`
	if !strings.Contains(pol, readAllow) {
		t.Fatalf("策略里没有 previews 的读放行（前端会读不回产物）:\n%s", pol)
	}
	if !strings.Contains(pol, writeDeny) {
		t.Fatalf("策略里没有 previews 的写拒绝（bash 可以改写缓存 = 伪造预览）:\n%s", pol)
	}
	// 顺序即语义：写拒绝必须在读放行之后，且之后不能再出现任何 allow file-write*。
	di := strings.Index(pol, writeDeny)
	if ri := strings.Index(pol, readAllow); ri > di {
		t.Fatalf("deny 写在 allow 之前，SBPL 后规则胜会让它失效")
	}
	if tail := pol[di:]; strings.Contains(tail, "(allow file-write*") {
		t.Fatalf("previews 的写拒绝之后还有 file-write 放行（后规则胜 → 写又被打开）:\n%s", tail)
	}
	// 不相交是前提：previews 若同时出现在读写放行里，"只读"就自相矛盾。
	for _, p := range s.ReadOnlySubpaths {
		if strings.Contains(pol, `(allow file-write* (subpath "`+p+`"))`) {
			t.Fatalf("只读子树 %s 同时被读写放行", p)
		}
	}
}

// 默认清单本身的自洽性：只读与读写两份清单不许交叉（交叉 = 一边写放行、
// 一边写拒绝，行为取决于拼接顺序 —— 这类 bug 只在生产某条路径上才显形）。
func TestDefaultOpenSubpathsDoNotOverlap(t *testing.T) {
	seen := map[string]string{}
	for _, p := range defaultOpenSubpaths {
		seen[p] = "read-write"
	}
	for _, p := range defaultReadOnlyOpenSubpaths {
		if prev, ok := seen[p]; ok {
			t.Fatalf("子树 %q 同时在 %s 与只读清单里", p, prev)
		}
	}
	if len(defaultReadOnlyOpenSubpaths) == 0 {
		t.Fatal("只读清单为空：previews 必须显式声明")
	}
}
