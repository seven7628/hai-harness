package stdjson

// stdjson_test.go：NDJSON 解码 + 握手版本校验。
//
// 纯内存测试（不 spawn 进程）—— 本包只负责「一行 JSON 怎么读、版本怎么判」，
// 连接管理不在这一层（见包注释的取舍说明），所以不需要真子进程。

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestNextDecodesFrames 逐行解码。
func TestNextDecodesFrames(t *testing.T) {
	r := NewReader(strings.NewReader(`{"notify":"hello","version":{"version_major":1,"version_minor":0}}
{"notify":"result","out":"hello","bytes":5}
{"notify":"result"}
`))
	var first HelloResult
	if err := r.Next(&first); err != nil {
		t.Fatalf("第一帧: %v", err)
	}
	if first.Version.Major != 1 || first.Version.Minor != 0 {
		t.Errorf("第一帧版本 = %+v, want 1.0", first.Version)
	}
	var res struct {
		Notify string `json:"notify"`
		Out    string `json:"out"`
		Bytes  int    `json:"bytes"`
	}
	if err := r.Next(&res); err != nil {
		t.Fatalf("第二帧: %v", err)
	}
	if res.Out != "hello" || res.Bytes != 5 {
		t.Errorf("第二帧 = %+v", res)
	}
	// 第三帧（无 out 字段）：解出来，**已存在的字段保持不变**（json.Unmarshal
	// 的标准语义：缺席的键不动原值）。故用新变量验证「不报错」。
	var third struct {
		Notify string `json:"notify"`
	}
	if err := r.Next(&third); err != nil {
		t.Fatalf("第三帧: %v", err)
	}
	if third.Notify != "result" {
		t.Errorf("第三帧 notify = %q, want result", third.Notify)
	}
	// 读尽后返回 io.EOF（**不是**故障：子进程正常写完并关闭 stdout）。
	if err := r.Next(&res); err != io.EOF {
		t.Errorf("第四帧 err = %v, want io.EOF", err)
	}
	if r.Err() != io.EOF {
		t.Errorf("Err() = %v, want io.EOF", r.Err())
	}
}

// TestEmptyLinesSkipped 子进程在握手前打启动 banner 是常见的，不该让整次执行失败。
func TestEmptyLinesSkipped(t *testing.T) {
	r := NewReader(strings.NewReader("\n\n{\"notify\":\"hello\"}\n"))
	var f struct {
		Notify string `json:"notify"`
	}
	if err := r.Next(&f); err != nil {
		t.Fatalf("空行未被跳过: %v", err)
	}
	if f.Notify != "hello" {
		t.Errorf("解出 %q, want hello", f.Notify)
	}
}

// TestLineTooLongIsDetectable 超长行必须**可判别**（不是「读不出来」这种
// 无信息故障）—— 调用方据此给模型不同的文案。
func TestLineTooLongIsDetectable(t *testing.T) {
	huge := `{"notify":"result","out":"` + strings.Repeat("x", MaxLine+16) + `"}`
	r := NewReader(strings.NewReader(huge + "\n"))
	var f struct {
		Notify string `json:"notify"`
	}
	err := r.Next(&f)
	if err == nil {
		t.Fatal("超长行竟被接受了")
	}
	if !LineTooLongErr(err) {
		t.Errorf("LineTooLongErr(%v) = false, want true", err)
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("超长行被误报为 EOF（调用方会当成「子进程正常结束」）")
	}
}

// TestBadJSONIsTransportError 非 JSON 行 = 传输级错误（流已不同步，不可继续读）。
func TestBadJSONIsTransportError(t *testing.T) {
	r := NewReader(strings.NewReader("not json at all\n{\"notify\":\"hello\"}\n"))
	var f struct {
		Notify string `json:"notify"`
	}
	if err := r.Next(&f); err == nil {
		t.Fatal("非 JSON 行竟被接受")
	}
	// 第二次调用必须返回**同一条**错误（sticky），不能继续往下读。
	var g struct {
		Notify string `json:"notify"`
	}
	if err2 := r.Next(&g); err2 == nil || !strings.Contains(err2.Error(), "decode") {
		t.Errorf("第二次 Next = %v, want 同一条 decode 错误", err2)
	}
}

// TestCheckHello 版本判定：major 相同即通过（minor 差异向后兼容）。
func TestCheckHello(t *testing.T) {
	want := Version{Major: 1, Minor: 0}
	ok := []Version{{Major: 1, Minor: 0}, {Major: 1, Minor: 7}}
	for _, got := range ok {
		if err := CheckHello(HelloResult{Version: got}, want); err != nil {
			t.Errorf("CheckHello(%+v) = %v, want nil", got, err)
		}
	}
	bad := Version{Major: 2, Minor: 0, Runtime: "node"}
	err := CheckHello(HelloResult{Version: bad}, want)
	if err == nil {
		t.Fatal("major 不兼容却通过了")
	}
	// 错误必须同时说明**双方**版本 + 明确「别重试」（模型自我修正的依据）。
	for _, want := range []string{"1.0", "2.0", "node", "do not retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误缺 %q: %v", want, err)
		}
	}
}

// TestCheckHelloNoRuntimeName 未自报运行时名时文案仍成立（不得出现空槽）。
func TestCheckHelloNoRuntimeName(t *testing.T) {
	err := CheckHello(HelloResult{Version: Version{Major: 3}}, Version{Major: 1})
	if err == nil {
		t.Fatal("major 不兼容却通过了")
	}
	if !strings.Contains(err.Error(), "sandbox runtime") {
		t.Errorf("未自报运行时名时文案退化: %v", err)
	}
}
