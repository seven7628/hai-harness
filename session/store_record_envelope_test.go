package session

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// marshalRecordEnvelope 必须与 json.Marshal 逐字节相同。
//
// 这条是「提交路径手工拼装 view_checkpoint」（329ms → 9ms）的安全网：
// 一旦拼装逻辑与标准库出现任何字节差异，checksum 口径就变了，
// 已落盘的 record 会全部读不出来。用真实 86~91MB record 对拍,
// 路径不可用时自动跳过（CI 无这些文件）。
func TestMarshalRecordEnvelopeEquivalence(t *testing.T) {
	paths := []string{
		"/Users/admin/.go-code/sessions/Users_admin_workspace_hai-studio-4e93d080/s-1791771198670823000-2.record.json",
		"/Users/admin/.go-code/sessions/Users_admin_cybertron-gogroup_agent-server-g-89d56925/s-1792132279027217000-21.record.json",
	}
	any := false
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil { continue }
		any = true
		var rec SessionRecord
		if err := json.Unmarshal(raw, &rec); err != nil { t.Fatal(err) }

		rec.Checksum = ""
		want, err := json.Marshal(&rec)
		if err != nil { t.Fatal(err) }

		s := time.Now()
		got, err := marshalRecordEnvelope(&rec)
		dEnv := time.Since(s)
		if err != nil { t.Fatal(err) }

		s = time.Now()
		_, _ = json.Marshal(&rec)
		dStd := time.Since(s)

		if !bytes.Equal(got, want) {
			n := len(got); if len(want) < n { n = len(want) }
			for i := 0; i < n; i++ {
				if got[i] != want[i] {
					lo := i-60; if lo < 0 { lo = 0 }
					hi := i+80; if hi > n { hi = n }
					t.Fatalf("%s: 字节不一致 @%d (len %d vs %d)\n got: %s\nwant: %s",
						p, i, len(got), len(want), got[lo:hi], want[lo:hi])
				}
			}
			t.Fatalf("%s: 长度不一致 %d vs %d", p, len(got), len(want))
		}
		t.Logf("%.1f MB: 逐字节一致 ✓  封装 %v vs 标准 %v (%.1fx)",
			float64(len(raw))/1e6, dEnv, dStd, float64(dStd)/float64(dEnv))
	}
	if !any { t.Skip("无真实 record") }
}

// 边界：空/ nil view_checkpoint。
func TestMarshalRecordEnvelopeEdgeCases(t *testing.T) {
	// 注意 nil 与 []byte{} ：json.Marshal 会把空 RawMessage 编码成 null。
	for _, view := range [][]byte{nil, []byte(`{}`), []byte(`{"a":1}`), []byte(`[1,2]`), []byte(`"str"`), []byte(`123`)} {
		rec := testSessionRecord(t, "ws", "s1", 1)
		rec.UpdatedAt = time.Date(2026,1,1,0,0,0,0,time.UTC)
		rec.ViewCheckpoint = view
		rec.Checksum = ""
		want, _ := json.Marshal(rec)
		got, err := marshalRecordEnvelope(rec)
		if err != nil { t.Fatalf("view=%q: %v", view, err) }
		if !bytes.Equal(got, want) {
			t.Fatalf("view=%q 不一致:\n got: %s\nwant: %s", view, got, want)
		}
	}
}
