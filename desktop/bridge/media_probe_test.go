// media_probe_test.go — 编解码矩阵 + `media_probe` 命令（R08c）。
//
// 两类测试，刻意分开：
//
//	① 矩阵（纯函数 decideMediaPlayback）：它是**产品行为**的表格（「mkv 要提示转码」
//	   「hevc 不直放」），散在 if 里就没人能一眼读出全貌 —— 所以这里用表驱动把每种
//	   结局钉死，改表必红。
//
//	② 命令（走 dispatch 生产入口，00 §4.7）：三种结局都必须是 **ok:true**，
//	   只有「路径不合法」才是 ok:false ——
//	     · 没有 ffprobe → available:false（降级不报错）；
//	     · ffprobe 坏了/超时 → state:"failed" + 原因；
//	     · 正常 → state:"ok" + 时长/编码。
//	   第一种最容易在重构里被写成错误（「找不到工具」看起来就像出错），而它一旦是
//	   ok:false，前端就会把「能播的 mp4」显示成「预览不可用」。
//
// 外部工具用**假 ffprobe 脚本 + 改 PATH** 来造，不 mock 函数：被测的 LookPath/exec/
// 解析/超时全是真的（这是 00 §4.7 要的那种证据强度）。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// —— ① 矩阵 ——

func TestMediaMatrixTableIsCoherent(t *testing.T) {
	seen := map[string]string{}
	for _, r := range mediaContainerRules {
		if len(r.Exts) == 0 {
			t.Fatalf("规则没有扩展名: %+v", r)
		}
		if r.Kind != "video" && r.Kind != "audio" {
			t.Fatalf("%v 的 kind 非法: %q", r.Exts, r.Kind)
		}
		if r.Mode != playDirect && r.Mode != playTranscode {
			t.Fatalf("%v 的 mode 非法: %q（规则表里只允许 direct/transcode，unknown 是判定结果）", r.Exts, r.Mode)
		}
		if r.Mode == playTranscode && r.Reason == "" {
			// 需转码必须给用户一句理由：面板上「需要转码」后面跟的就是它。
			t.Fatalf("%v 是 transcode 但没写 Reason（用户会看到没有原因的红字）", r.Exts)
		}
		if r.Mode == playDirect && r.Kind == "video" && len(r.VideoCodecs) == 0 {
			t.Fatalf("%v 是直放视频容器但没写 VideoCodecs（等于「任何编码都直放」，hevc 也会被放行）", r.Exts)
		}
		for _, e := range r.Exts {
			if e != mediaExtOf("x."+e) {
				t.Fatalf("扩展名必须是小写不含点: %q", e)
			}
			if prev, ok := seen[e]; ok {
				t.Fatalf("扩展名 %q 同时出现在两条规则里（%q 与这条）—— 判定结果会取决于表的顺序", e, prev)
			}
			seen[e] = r.Reason
		}
	}
	// 05 §4 D-L 的三种口径必须都在表里（少了任何一种，PR-08c 的验收就不成立）。
	for _, must := range []string{"mkv", "avi", "ts"} {
		r := mediaRuleForExt(must)
		if r == nil || r.Mode != playTranscode {
			t.Fatalf("%s 必须是 transcode（05 §4 D-L：容器不支持）", must)
		}
	}
	for _, must := range []string{"mp4", "webm", "mp3", "wav", "ogg", "flac"} {
		r := mediaRuleForExt(must)
		if r == nil || r.Mode != playDirect {
			t.Fatalf("%s 必须是 direct（05 §4 D-L：直放名单）", must)
		}
	}
	if !isMediaExt("mp4") || isMediaExt("txt") || isMediaExt("") {
		t.Fatal("isMediaExt 判据不对")
	}
}

func TestDecideMediaPlayback(t *testing.T) {
	type c struct {
		name   string
		ext    string
		info   mediaProbeInfo
		probed bool
		mode   string
		kind   string
	}
	cases := []c{
		{"mp4 h264+aac 直放", "mp4", mediaProbeInfo{FormatName: "mov,mp4,m4a,3gp,3g2,mj2", VideoCodec: "h264", AudioCodec: "aac", HasVideo: true, HasAudio: true}, true, playDirect, "video"},
		{"mp4 hevc → 需转码（05：hevc 视构建，一律不承诺）", "mp4", mediaProbeInfo{VideoCodec: "hevc", AudioCodec: "aac", HasVideo: true, HasAudio: true}, true, playTranscode, "video"},
		{"mp4 ac3 音轨 → 需转码", "mp4", mediaProbeInfo{VideoCodec: "h264", AudioCodec: "ac3", HasVideo: true, HasAudio: true}, true, playTranscode, "video"},
		{"mp4 无 ffprobe → 按容器默认直放", "mp4", mediaProbeInfo{}, false, playDirect, "video"},
		{"mkv 容器否定一切编码", "mkv", mediaProbeInfo{VideoCodec: "h264", AudioCodec: "aac", HasVideo: true, HasAudio: true}, true, playTranscode, "video"},
		{"mkv 无探测也照样要转码", "mkv", mediaProbeInfo{}, false, playTranscode, "video"},
		{"avi → 转码", "avi", mediaProbeInfo{VideoCodec: "mpeg4", HasVideo: true}, true, playTranscode, "video"},
		{"m2ts → 转码（与 ts 同族）", "m2ts", mediaProbeInfo{}, false, playTranscode, "video"},
		{"webm vp9+opus 直放", "webm", mediaProbeInfo{FormatName: "matroska,webm", VideoCodec: "vp9", AudioCodec: "opus", HasVideo: true, HasAudio: true}, true, playDirect, "video"},
		{"webm vp9 无 ffprobe 直放", "webm", mediaProbeInfo{}, false, playDirect, "video"},
		{"webm + h264 不在白名单 → 转码", "webm", mediaProbeInfo{VideoCodec: "h264", HasVideo: true}, true, playTranscode, "video"},
		{"mp3 是 audio 而不是 video", "mp3", mediaProbeInfo{FormatName: "mp3", AudioCodec: "mp3", HasAudio: true}, true, playDirect, "audio"},
		{"wav 的 pcm_* 前缀匹配", "wav", mediaProbeInfo{AudioCodec: "pcm_s24le", HasAudio: true}, true, playDirect, "audio"},
		{"flac 直放", "flac", mediaProbeInfo{AudioCodec: "flac", HasAudio: true}, true, playDirect, "audio"},
		{"ogg vorbis 直放", "ogg", mediaProbeInfo{AudioCodec: "vorbis", HasAudio: true}, true, playDirect, "audio"},
		{"未知扩展名 + h264 → unknown（先试播）", "xyz", mediaProbeInfo{FormatName: "some-format", VideoCodec: "h264", HasVideo: true}, true, playUnknown, "video"},
		{"未知扩展名 + mpeg4 → 转码", "xyz", mediaProbeInfo{VideoCodec: "mpeg4", HasVideo: true}, true, playTranscode, "video"},
		{"未知扩展名 + 无探测 → unknown", "xyz", mediaProbeInfo{}, false, playUnknown, "video"},
		{"扩展名未知但 format_name 认得出是 mp4 族", "m4v2", mediaProbeInfo{FormatName: "mov,mp4,m4a,3gp,3g2,mj2", VideoCodec: "h264", HasVideo: true}, true, playDirect, "video"},
		{"扩展名未知但 format_name 是 matroska → 转码", "backup", mediaProbeInfo{FormatName: "matroska,webm", VideoCodec: "h264", HasVideo: true}, true, playTranscode, "video"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pb := decideMediaPlayback(tc.ext, tc.info, tc.probed)
			if pb.Mode != tc.mode {
				t.Fatalf("mode = %q, want %q（reason=%q）", pb.Mode, tc.mode, pb.Reason)
			}
			if pb.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", pb.Kind, tc.kind)
			}
			if pb.Mode != playDirect && pb.Reason == "" {
				t.Fatal("非直放的结论必须带原因（面板要写给人看）")
			}
		})
	}
}

// —— ② 命令（dispatch 生产入口）——

// fakeFFprobe 在临时目录里放一个假 `ffprobe` 脚本并把 PATH 指向它（只有这一个目录：
// 真正的 ffprobe 一定找不到 —— 这是「真的把工具藏起来」，不是 mock 掉一次调用）。
func fakeFFprobe(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 假脚本自己还要用 cat/sleep：把假目录**前置**到原 PATH（LookPath 命中我们这份，
	// 真实的 ffprobe 被影子掉）。「完全没有 ffprobe」的那条用例另用空目录（见降级测试）。
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return p
}

// countLines 数假 ffprobe 被调用了几次（每个调用写一行 x）。
func countLines(t *testing.T, p string) int {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "x")
}

func writeMediaFixture(t *testing.T, ws, name string, size int) string {
	t.Helper()
	p := filepath.Join(ws, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func probeViaDispatch(t *testing.T, m *manager, buf *syncBuf, ws, path string) map[string]any {
	t.Helper()
	m.dispatch(cmd(0, "media_probe", map[string]any{"workspace": ws, "path": path}))
	lines := readLines(buf)
	if len(lines) == 0 {
		t.Fatal("media_probe 没有响应")
	}
	if lines[0]["ok"] != true {
		t.Fatalf("media_probe 必须 ok:true（降级不是错误）: %v", lines[0])
	}
	data, _ := lines[0]["data"].(map[string]any)
	if data == nil {
		t.Fatalf("media_probe 响应没有 data: %v", lines[0])
	}
	return data
}

const fakeFFprobeOK = `cat <<'JSON'
{"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"3600.000000","bit_rate":"120000"},
 "streams":[{"codec_type":"video","codec_name":"h264","width":160,"height":120,"bit_rate":"100000"},
            {"codec_type":"audio","codec_name":"aac","channels":2,"sample_rate":"48000","bit_rate":"20000"}]}
JSON`

func TestMediaProbeDegradesWhenFFprobeMissing(t *testing.T) {
	resetMediaProbeCache()
	ws := t.TempDir()
	writeMediaFixture(t, ws, "a.mp4", 32)
	// PATH 指向一个空目录：LookPath("ffprobe") 必失败（本机装没装 ffprobe 都不影响这条）。
	t.Setenv("PATH", t.TempDir())
	m, buf := testManager(t)

	d := probeViaDispatch(t, m, buf, ws, "a.mp4")
	if d["available"] != false {
		t.Fatalf("没有 ffprobe 时 available 必须是 false: %v", d["available"])
	}
	if d["state"] != "unavailable" {
		t.Fatalf("state = %v, want unavailable", d["state"])
	}
	if d["path"] == "" || d["reason"] == "" {
		t.Fatalf("降级也要给出路径与原因: %v", d)
	}
	pb, _ := d["playback"].(map[string]any)
	if pb == nil || pb["mode"] != playDirect {
		t.Fatalf("mp4 在没有 ffprobe 时仍须判为可直放（否则用户看到「不可播」）: %v", d["playback"])
	}
	if pb["probed"] != false {
		t.Fatalf("probed 必须是 false: %v", pb["probed"])
	}
}

func TestMediaProbeParsesFFprobeOutput(t *testing.T) {
	resetMediaProbeCache()
	ws := t.TempDir()
	writeMediaFixture(t, ws, "a.mp4", 64)
	fakeFFprobe(t, fakeFFprobeOK)
	m, buf := testManager(t)

	d := probeViaDispatch(t, m, buf, ws, "a.mp4")
	if d["available"] != true || d["state"] != "ok" {
		t.Fatalf("state = %v available = %v reason = %v", d["state"], d["available"], d["reason"])
	}
	if d["duration_ms"] != float64(3_600_000) {
		t.Fatalf("duration_ms = %v", d["duration_ms"])
	}
	if d["bit_rate"] != float64(120000) {
		t.Fatalf("bit_rate = %v", d["bit_rate"])
	}
	pb, _ := d["playback"].(map[string]any)
	if pb["mode"] != playDirect || pb["kind"] != "video" || pb["video_codec"] != "h264" || pb["audio_codec"] != "aac" {
		t.Fatalf("playback = %v", pb)
	}
	// 响应是 JSON 往返过的：切片的静态类型是 []any（不是 []map[string]any）
	streams, _ := d["streams"].([]any)
	if len(streams) != 2 {
		t.Fatalf("streams = %v", d["streams"])
	}
	v, _ := streams[0].(map[string]any)
	a, _ := streams[1].(map[string]any)
	if v["width"] != float64(160) || v["codec"] != "h264" || a["channels"] != float64(2) {
		t.Fatalf("streams = %v", d["streams"])
	}
}

func TestMediaProbeFailedIsNotAnError(t *testing.T) {
	resetMediaProbeCache()
	ws := t.TempDir()
	writeMediaFixture(t, ws, "a.mp4", 16)
	fakeFFprobe(t, `echo "boom: invalid data found" >&2; exit 1`)
	m, buf := testManager(t)

	d := probeViaDispatch(t, m, buf, ws, "a.mp4")
	if d["state"] != "failed" {
		t.Fatalf("state = %v, want failed", d["state"])
	}
	if s, _ := d["reason"].(string); s == "" {
		t.Fatalf("failed 必须带原因: %v", d)
	}
	pb, _ := d["playback"].(map[string]any)
	if pb["mode"] != playDirect {
		t.Fatalf("ffprobe 坏了不等于文件不能播：mp4 仍按容器判直放，got %v", pb["mode"])
	}
}

func TestMediaProbeTimeoutIsFailedNotError(t *testing.T) {
	resetMediaProbeCache()
	ws := t.TempDir()
	writeMediaFixture(t, ws, "a.mkv", 16)
	fakeFFprobe(t, `exec sleep 5`) // exec：不 fork 子 shell —— 杀掉的是它自己，不会被 WaitDelay 拖成 3s
	old := mediaProbeTimeout
	mediaProbeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { mediaProbeTimeout = old })
	m, buf := testManager(t)

	start := time.Now()
	d := probeViaDispatch(t, m, buf, ws, "a.mkv")
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("超时没有生效（用了 %s）", el)
	}
	if d["state"] != "failed" {
		t.Fatalf("state = %v, want failed", d["state"])
	}
	if s, _ := d["reason"].(string); s == "" {
		t.Fatal("超时必须带原因")
	}
	pb, _ := d["playback"].(map[string]any)
	if pb["mode"] != playTranscode {
		t.Fatalf("mkv 仍然是转码（与 ffprobe 无关），got %v", pb["mode"])
	}
}

func TestMediaProbeCacheByPathSizeMtime(t *testing.T) {
	resetMediaProbeCache()
	ws := t.TempDir()
	p := writeMediaFixture(t, ws, "a.mp4", 32)
	// 假 ffprobe 每次都往计数文件里追加一行：缓存命中时它不该被执行。
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "ffprobe")
	body := "echo x >> " + counter + "\n" + fakeFFprobeOK
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	m, buf := testManager(t)

	d1 := probeViaDispatch(t, m, buf, ws, "a.mp4")
	if d1["cached"] != false {
		t.Fatalf("第一次不该是缓存命中: %v", d1["cached"])
	}
	d2 := probeViaDispatch(t, m, buf, ws, "a.mp4")
	if d2["cached"] != true {
		t.Fatalf("同 (path,size,mtime) 必须命中缓存: %v", d2["cached"])
	}
	if n := countLines(t, counter); n != 1 {
		t.Fatalf("缓存命中时不该再执行 ffprobe（调用次数 = %d）", n)
	}

	// size 变了 → 必须重探
	if err := os.WriteFile(p, make([]byte, 48), 0o644); err != nil {
		t.Fatal(err)
	}
	d3 := probeViaDispatch(t, m, buf, ws, "a.mp4")
	if d3["cached"] != false || d3["size"] != float64(48) {
		t.Fatalf("size 变化后必须重探: cached=%v size=%v", d3["cached"], d3["size"])
	}
	if n := countLines(t, counter); n != 2 {
		t.Fatalf("size 变化后应再执行一次 ffprobe（调用次数 = %d）", n)
	}
}

func TestMediaProbeRejectsBadPaths(t *testing.T) {
	resetMediaProbeCache()
	ws := t.TempDir()
	writeMediaFixture(t, ws, "a.mp4", 8)
	writeMediaFixture(t, ws, "notes.txt", 8)
	m, buf := testManager(t)

	// 越界（相对路径上跳）
	m.dispatch(cmd(0, "media_probe", map[string]any{"workspace": ws, "path": "../evil.mp4"}))
	if l := readLines(buf); len(l) == 0 || l[0]["ok"] != false {
		t.Fatalf("越界路径必须 ok:false: %v", l)
	}
	// 非媒体扩展名（协议同样拒绝；这里提前给明确说法，而不是去跑 ffprobe）
	m.dispatch(cmd(0, "media_probe", map[string]any{"workspace": ws, "path": "notes.txt"}))
	if l := readLines(buf); len(l) == 0 || l[0]["ok"] != false {
		t.Fatalf("非媒体文件必须 ok:false: %v", l)
	}
	// 目录
	m.dispatch(cmd(0, "media_probe", map[string]any{"workspace": ws, "path": "."}))
	if l := readLines(buf); len(l) == 0 || l[0]["ok"] != false {
		t.Fatalf("目录必须 ok:false: %v", l)
	}
	// 不存在
	m.dispatch(cmd(0, "media_probe", map[string]any{"workspace": ws, "path": "nope.mp4"}))
	if l := readLines(buf); len(l) == 0 || l[0]["ok"] != false {
		t.Fatalf("不存在的文件必须 ok:false: %v", l)
	}
	// 空 path
	m.dispatch(cmd(0, "media_probe", map[string]any{"workspace": ws}))
	if l := readLines(buf); len(l) == 0 || l[0]["ok"] != false {
		t.Fatalf("空 path 必须 ok:false: %v", l)
	}
}
