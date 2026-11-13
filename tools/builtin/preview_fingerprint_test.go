package builtin

import "testing"

// 脚本指纹是缓存键里「转换器版本」的唯一来源：它必须
//  1. 稳定（同进程内多次调用一致）且非空；
//  2. **覆盖全部三个脚本**（漏一个 = 改那个脚本后仍命中旧产物）；
//  3. 对内容敏感（改一个字就换指纹）。
//
// 为什么值得单独测：它是「改了脚本，缓存静默命中旧产物」这条故障的唯一防线，
// 而它自己没有任何运行时反馈 —— 算错了不会报错，只会让用户看到过期的预览。
func TestPreviewScriptFingerprint(t *testing.T) {
	fp := PreviewScriptFingerprint()
	if fp == "" || len(fp) != 16 {
		t.Fatalf("指纹形状不对: %q", fp)
	}
	if again := PreviewScriptFingerprint(); again != fp {
		t.Fatalf("同一进程内两次取值不一致: %q vs %q", fp, again)
	}
	// 自证闭合：指纹必须**等于**三个脚本现算的结果。没有这一行，把
	// previewScriptFingerprint 换成任何硬编码常量，上面两条断言照样全绿 ——
	// 而那正是「改了脚本仍命中旧产物」的原始故障。
	if want := scriptFingerprint(xlsxToHTMLScript, docxToHTMLScript, pptxToHTMLScript); fp != want {
		t.Fatalf("指纹与脚本现值不符: got %q want %q（要么漏了脚本，要么被写死了）", fp, want)
	}

	// 覆盖性：任取两个脚本的组合都必须算出**不同**的指纹 —— 等价于「三个都进了哈希」。
	pairs := [][2]string{
		{xlsxToHTMLScript, docxToHTMLScript},
		{xlsxToHTMLScript, pptxToHTMLScript},
		{docxToHTMLScript, pptxToHTMLScript},
	}
	for i, p := range pairs {
		if scriptFingerprint(p[0], p[1]) == fp {
			t.Fatalf("第 %d 组（缺一个脚本）指纹与全量相同 —— 有脚本没进哈希", i+1)
		}
	}

	// 内容敏感：改一个字必须换指纹。
	changed := "x" + xlsxToHTMLScript
	if scriptFingerprint(changed, docxToHTMLScript, pptxToHTMLScript) == fp {
		t.Fatal("脚本内容变了指纹没变 —— 缓存不会失效")
	}
	// 顺序敏感（分隔符的意义）：交换两个脚本不该得到同一个指纹。
	if scriptFingerprint(xlsxToHTMLScript, docxToHTMLScript, pptxToHTMLScript) !=
		scriptFingerprint(xlsxToHTMLScript, pptxToHTMLScript, docxToHTMLScript) {
		return
	}
	t.Fatal("脚本顺序不影响指纹：分隔符没起作用（跨脚本边界可被脚本内容搬运）")
}
