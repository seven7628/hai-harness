package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSessionManualTitlesRoundTrip(t *testing.T) {
	appDataDirOverride = t.TempDir()
	t.Cleanup(func() { appDataDirOverride = "" })
	// 全局缓存按 wsKey 分片：用独立的 key，避免与其他用例互踩
	const wsKey, sid = "test-ws-manual", "s-1"

	if got := sessionManualTitles.get(wsKey, sid); got != "" {
		t.Fatalf("初始应为空，得到 %q", got)
	}
	if !sessionManualTitles.set(wsKey, sid, "  修手机端  ") {
		t.Fatal("首次写入应返回 changed")
	}
	if got := sessionManualTitles.get(wsKey, sid); got != "修手机端" {
		t.Fatalf("get = %q（应 trim）", got)
	}
	// 幂等：同值不重复落盘
	if sessionManualTitles.set(wsKey, sid, "修手机端") {
		t.Fatal("同值写入不应标记 changed")
	}

	// 落盘格式：{ "titles": { sid: title } }
	b, err := os.ReadFile(manualTitlesFile(wsKey))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	var doc manualTitlesFileData
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if doc.Titles[sid] != "修手机端" {
		t.Fatalf("文件内容不对: %+v", doc.Titles)
	}

	// 模拟重启：清缓存后从磁盘读回
	sessionManualTitles = &manualTitlesStore{cache: map[string]map[string]string{}}
	if got := sessionManualTitles.get(wsKey, sid); got != "修手机端" {
		t.Fatalf("重启后 get = %q", got)
	}

	// 空标题 = 清除覆盖
	if !sessionManualTitles.set(wsKey, sid, "") {
		t.Fatal("清除应返回 changed")
	}
	if got := sessionManualTitles.get(wsKey, sid); got != "" {
		t.Fatalf("清除后应为空，得到 %q", got)
	}
	if sessionManualTitles.set(wsKey, sid, "") {
		t.Fatal("重复清除不应标记 changed")
	}
}
