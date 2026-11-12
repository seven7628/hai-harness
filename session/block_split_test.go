package session

// 切分器的正确性只有一个硬要求：**拼回去必须与原字节逐字节相同**。
// 因为 sidecar 的完整性校验是「读出的字节摘要 vs 记录里的摘要」，
// 重组后哪怕只差一个字节（哪怕语义等价、字段顺序变了），校验就会失败。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// joinFrags 拼回片段。
func joinFrags(frags [][]byte) []byte {
	var out bytes.Buffer
	for _, f := range frags {
		out.Write(f)
	}
	return out.Bytes()
}

func TestSplitTopLevelArrayRoundTrip(t *testing.T) {
	cases := []string{
		`{"kind":"agent","id":5,"status":"running","items":[{"k":"a"},{"k":"b"}],"items_trimmed":0}`,
		`{"items":[1,2,3]}`,
		`{"a":"x","items":[{"x":"},{"},{"y":1}],"z":1}`,
		`{"items":["有中文","和\"引号\"","还有逗号,在里面"],"tail":true}`,
		`{"nested":{"items":[1,2]},"items":[{"a":1},{"b":2}]}`,
		`{"items":[{"deep":{"a":[1,2,3]}},{"b":"x"}],"after":"y"}`,
	}
	for _, c := range cases {
		frags := splitTopLevelArray([]byte(c), 1)
		if frags == nil {
			t.Fatalf("没有切分：%s", c)
		}
		got := joinFrags(frags)
		if !bytes.Equal(got, []byte(c)) {
			t.Fatalf("拼接不逐字节相同：\n原: %s\n拼: %s", c, got)
		}
		// 拼回去必须还是合法 JSON 且语义相同
		var a, b any
		if err := json.Unmarshal([]byte(c), &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got, &b); err != nil {
			t.Fatalf("拼回去不是合法 JSON: %v (%s)", err, got)
		}
	}
}

// TestSplitTopLevelArrayPicksLongest 应当切**最长**的顶层数组。
func TestSplitTopLevelArrayPicksLongest(t *testing.T) {
	payload := []byte(`{"small":[1,2],"big":["aaaa","bbbb","cccc","dddd"],"tail":1}`)
	frags := splitTopLevelArray(payload, 1)
	if frags == nil {
		t.Fatal("没有切分")
	}
	if !bytes.Equal(joinFrags(frags), payload) {
		t.Fatal("拼接不逐字节相同")
	}
	// 切点必须在 "big" 数组里（而不是 "small"）
	if !bytes.Contains(frags[0], []byte(`"big":[`)) {
		t.Fatalf("切错了数组：首片 = %s", frags[0])
	}
}

// TestSplitSkipsShortArrays 太短的数组不切（碎片化不划算）。
func TestSplitSkipsShortArrays(t *testing.T) {
	payload := []byte(`{"items":[1,2],"x":1}`)
	if frags := splitTopLevelArray(payload, 1024); frags != nil {
		t.Fatalf("短数组不该切：%v", frags)
	}
	// 空数组 → 没东西可切
	if frags := splitTopLevelArray([]byte(`{"items":[]}`), 1); frags != nil {
		t.Fatalf("空数组不该切：%v", frags)
	}
	// 没有数组 → 不切
	if frags := splitTopLevelArray([]byte(`{"a":1,"b":"x"}`), 1); frags != nil {
		t.Fatalf("无数组不该切：%v", frags)
	}
	// 不是对象 → 不切
	if frags := splitTopLevelArray([]byte(`[1,2,3]`), 1); frags != nil {
		t.Fatalf("非对象不该切：%v", frags)
	}
}

// TestSplitRealAgentCard 用真实形态的 agent 卡片验证（字段顺序与 items 在中间）。
func TestSplitRealAgentCard(t *testing.T) {
	var items []string
	for i := 0; i < 50; i++ {
		items = append(items, fmt.Sprintf(`{"kind":"tool","tool_id":"call-%d","name":"bash","args":"{\"cmd\":\"echo %d\"}","status":"done"}`, i, i))
	}
	payload := []byte(`{"id":9001,"kind":"agent","run_id":"r1","status":"running","label":"干活",` +
		`"items":[` + strings.Join(items, ",") + `],"items_trimmed":10,"artifacts":[]}`)
	frags := splitTopLevelArray(payload, 1024)
	if frags == nil {
		t.Fatal("没有切分")
	}
	if len(frags) < 10 {
		t.Fatalf("片段太少：%d", len(frags))
	}
	got := joinFrags(frags)
	if !bytes.Equal(got, payload) {
		t.Fatal("拼接不逐字节相同")
	}
	// 第一片是前缀（含 "items":["），最后一片是后缀
	if !bytes.Contains(frags[0], []byte(`"items":[`)) {
		t.Fatalf("首片应为前缀：%s", frags[0])
	}
	if !bytes.Contains(frags[len(frags)-1], []byte(`"items_trimmed":10`)) {
		t.Fatalf("末片应为后缀：%s", frags[len(frags)-1])
	}
}

// TestSplitIsDeterministic 同样的输入必须切出同样的片段（否则摘要每次都变，
// 每��提交都会重写整个块）。
func TestSplitIsDeterministic(t *testing.T) {
	payload := []byte(`{"kind":"agent","items":[{"a":1},{"b":2},{"c":3}],"z":1}`)
	a := splitTopLevelArray(payload, 1)
	b := splitTopLevelArray(payload, 1)
	if len(a) != len(b) {
		t.Fatalf("片段数不一致：%d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("第 %d 片不一致", i)
		}
	}
}
