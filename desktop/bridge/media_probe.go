// media_probe.go — `media_probe` 命令：用 ffprobe 取时长/分辨率/编码/码率，并按
// `(绝对路径, size, mtime)` 缓存（R08c / 02 §PR-08c）。
//
// 三条硬口径：
//
//  1. **没有 ffprobe 不是错误**。返回 `ok:true, available:false, state:"unavailable"`，
//     并给出按容器默认的播放判断（mp4 照样能直放）。理由：ffprobe 是**可选**外部工具
//     （用户机器上没装是常态），把它当错误会让「能播的 mp4」被显示成「预览不可用」——
//     这违反 05 §4 D-L「没有 ffmpeg 就只给系统播放器」的降级约定。
//  2. 超时/异常 → `state:"failed"` + 原因（前端显示「无法读取媒体信息」但仍可播放）。
//     与 ① 的区别是「装了但坏了」：用户该看到一句原因，而不是静默降级。
//  3. 缓存键必须含 size+mtime：重新导出过的同名文件必须重探（否则面板显示旧时长，
//     而且用户拖动到旧时长之外会直接失败）。
//
// 编解码判断本身不在这个文件里：见 media_matrix.go（表 + 判定函数）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// mediaProbeMaxJSON ffprobe 输出上限（防病态文件把内存打满；正常输出 <100KB）。
	mediaProbeMaxJSON = 4 << 20
	// mediaProbeCacheMax 缓存条目上限（LRU 太重，这里只做「满了丢最早的一半」）。
	mediaProbeCacheMax = 256
)

// mediaProbeTimeout ffprobe 只在**打开媒体时**跑，结果被缓存；10s 对本地文件足够宽
// （4K mkv 也 <1s），又比主进程那条 bridge 请求的 30s 超时短得多 —— 超时原因落在面板上，
// 而不是"bridge 请求超时"。
// var 而不是 const：单测要把它缩到 200ms 才能在秒级内验证超时分支（超时是「装了但坏了」
// 唯一可复现的形态，见 TestMediaProbeTimeoutIsFailedNotError）。
var mediaProbeTimeout = 10 * time.Second

// ffprobe 的 JSON 形态（只取用到的字段；其余忽略）。
// 注意 ffprobe 把数值也输出成字符串（"bit_rate":"123456"），所以这里全是 string。
type ffprobeStream struct {
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
	BitRate    string `json:"bit_rate"`
	Duration   string `json:"duration"`
}

type ffprobeOutput struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []ffprobeStream `json:"streams"`
}

// mediaStream 回给前端的一条流（字段名走 snake_case，与 command_response 的其余 data 一致）。
type mediaStream struct {
	Type       string `json:"type"`
	Codec      string `json:"codec"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	BitRate    int64  `json:"bit_rate,omitempty"`
}

// mediaProbeData 是 `media_probe` 的完整 data。
type mediaProbeData struct {
	Path       string        `json:"path"`        // 绝对路径（前端据此要 hai-media URL）
	Size       int64         `json:"size"`        // 字节
	ModifiedMs int64         `json:"modified_ms"` // mtime（毫秒）：前端用它判「文件换过了」
	Available  bool          `json:"available"`   // ffprobe 是否可用
	State      string        `json:"state"`       // ok | unavailable | failed
	Reason     string        `json:"reason"`      // 不可用/失败的原因（用户可见）
	Format     string        `json:"format"`      // ffprobe 的 format_name
	DurationMs int64         `json:"duration_ms"`
	BitRate    int64         `json:"bit_rate"`
	Streams    []mediaStream `json:"streams"`
	Playback   mediaPlayback `json:"playback"`
	Cached     bool          `json:"cached"`
}

// payload 摊成 map[string]any（resp() 的 data 是 map；这里显式映射，字段少且类型固定，
// 比 JSON 往返少一次分配，也避免 omitempty 在 map 里失效）。
func (d mediaProbeData) payload() map[string]any {
	streams := make([]map[string]any, 0, len(d.Streams))
	for _, s := range d.Streams {
		streams = append(streams, map[string]any{
			"type": s.Type, "codec": s.Codec, "width": s.Width, "height": s.Height,
			"channels": s.Channels, "sample_rate": s.SampleRate, "bit_rate": s.BitRate,
		})
	}
	return map[string]any{
		"path": d.Path, "size": d.Size, "modified_ms": d.ModifiedMs,
		"available": d.Available, "state": d.State, "reason": d.Reason,
		"format": d.Format, "duration_ms": d.DurationMs, "bit_rate": d.BitRate,
		"streams": streams,
		"playback": map[string]any{
			"mode": d.Playback.Mode, "kind": d.Playback.Kind, "container": d.Playback.Container,
			"video_codec": d.Playback.VideoCodec, "audio_codec": d.Playback.AudioCodec,
			"reason": d.Playback.Reason, "probed": d.Playback.Probed,
		},
		"cached": d.Cached,
	}
}

// —— 缓存 ——

type mediaProbeCacheEntry struct {
	size       int64
	modifiedMs int64
	data       mediaProbeData
}

var mediaProbeCache = struct {
	sync.Mutex
	m map[string]mediaProbeCacheEntry
}{m: make(map[string]mediaProbeCacheEntry)}

func resetMediaProbeCache() {
	mediaProbeCache.Lock()
	defer mediaProbeCache.Unlock()
	mediaProbeCache.m = make(map[string]mediaProbeCacheEntry)
}

func mediaProbeCacheGet(abs string, size, modifiedMs int64) (mediaProbeData, bool) {
	mediaProbeCache.Lock()
	defer mediaProbeCache.Unlock()
	e, ok := mediaProbeCache.m[abs]
	if !ok || e.size != size || e.modifiedMs != modifiedMs {
		return mediaProbeData{}, false
	}
	d := e.data
	d.Cached = true
	return d, true
}

func mediaProbeCachePut(abs string, d mediaProbeData) {
	mediaProbeCache.Lock()
	defer mediaProbeCache.Unlock()
	if len(mediaProbeCache.m) >= mediaProbeCacheMax {
		// 满了丢一半：比实现 LRU 便宜，且媒体缓存本来就是「最近打开的那几个才热」。
		i := 0
		for k := range mediaProbeCache.m {
			delete(mediaProbeCache.m, k)
			i++
			if i >= mediaProbeCacheMax/2 {
				break
			}
		}
	}
	e := d
	e.Cached = false
	mediaProbeCache.m[abs] = mediaProbeCacheEntry{size: d.Size, modifiedMs: d.ModifiedMs, data: e}
}

// —— ffprobe 执行 ——

// ffprobePath 找 ffprobe：**只查 PATH**（不扫 /Applications、不内置一份二进制）。
// 找不到 → ("", false)，由调用方降级；这里不报错。
func ffprobePath() (string, bool) {
	p, err := exec.LookPath("ffprobe")
	if err != nil || p == "" {
		return "", false
	}
	return p, true
}

// runFFprobe 跑一次 ffprobe。ctx 超时 → 明确的"超时"错误（调用方转成 failed + 原因）。
// WaitDelay 与 gitRun 同一条经验：即使 ffprobe 拉起了继承人管道的东西，Wait 也不会永久挂住。
func runFFprobe(ctx context.Context, bin, abs string) (ffprobeOutput, error) {
	var out ffprobeOutput
	c, cancel := context.WithTimeout(ctx, mediaProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, bin, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", abs)
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.Output()
	if len(stdout) > mediaProbeMaxJSON {
		return out, fmt.Errorf("ffprobe 输出过大（>%d 字节）", mediaProbeMaxJSON)
	}
	// WaitDelay 到点时 exec 会回 ErrWaitDelay，但**进程可能已经正常退出**（只是有后代占着
	// 管道）——与 gitRun 同一条口径：这种情况输出可信，不判失败。
	if err != nil && errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	if err != nil {
		if c.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("ffprobe 超时（%s）", mediaProbeTimeout)
		}
		// exec.ExitError 带 stderr 里的最后一小段（ffprobe 的报错就在那里）
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("ffprobe 失败: %s", firstNonEmptyLine(string(ee.Stderr)))
		}
		return out, fmt.Errorf("ffprobe 失败: %v", err)
	}
	if err := json.Unmarshal(stdout, &out); err != nil {
		return out, fmt.Errorf("ffprobe 输出无法解析: %v", err)
	}
	return out, nil
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}

func parseMediaNumber(s string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f)
}

// parseMediaSeconds ffprobe 的时长字段是**秒**（"3600.000000"），面板要毫秒。
// 单独一个函数是因为「秒 → 毫秒」漏乘 1000 时，界面上只是显示 3.6 秒 —— 看起来像
// 文件坏了，而不像单位错了（本 PR 的测试当场抓到过一次）。
func parseMediaSeconds(s string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f * 1000)
}

// probeInfoFrom 把 ffprobe 的输出压成判定需要的几个事实（第一条视频流/音频流）。
func probeInfoFrom(out ffprobeOutput) (mediaProbeInfo, []mediaStream, int64, int64) {
	info := mediaProbeInfo{FormatName: out.Format.FormatName}
	streams := make([]mediaStream, 0, len(out.Streams))
	var durMs int64
	for _, s := range out.Streams {
		ms := mediaStream{Type: s.CodecType, Codec: s.CodecName, Width: s.Width, Height: s.Height,
			Channels: s.Channels, SampleRate: int(parseMediaNumber(s.SampleRate)), BitRate: parseMediaNumber(s.BitRate)}
		streams = append(streams, ms)
		switch s.CodecType {
		case "video":
			if !info.HasVideo {
				info.HasVideo = true
				info.VideoCodec = s.CodecName
			}
		case "audio":
			if !info.HasAudio {
				info.HasAudio = true
				info.AudioCodec = s.CodecName
			}
		}
		if d := parseMediaSeconds(s.Duration); d > durMs {
			durMs = d
		}
	}
	if d := parseMediaSeconds(out.Format.Duration); d > 0 {
		durMs = d
	}
	return info, streams, durMs, parseMediaNumber(out.Format.BitRate)
}

// probeMediaFile 探测一个已经 stat 过的普通文件：命中缓存直接回，否则跑 ffprobe。
// 三种结局都回 **正常 data**（ok 由调用方写死 true）：可用 / 不可用 / 失败。
func probeMediaFile(abs string, size, modifiedMs int64) mediaProbeData {
	ext := mediaExtOf(abs)
	if d, ok := mediaProbeCacheGet(abs, size, modifiedMs); ok {
		return d
	}
	d := mediaProbeData{Path: abs, Size: size, ModifiedMs: modifiedMs, State: "ok"}
	bin, ok := ffprobePath()
	if !ok {
		d.Available = false
		d.State = "unavailable"
		d.Reason = "本机没有 ffprobe（ffmpeg）——无法读取时长/编码，按容器默认判断"
		d.Playback = decideMediaPlayback(ext, mediaProbeInfo{}, false)
		mediaProbeCachePut(abs, d)
		return d
	}
	out, err := runFFprobe(context.Background(), bin, abs)
	if err != nil {
		d.Available = true
		d.State = "failed"
		d.Reason = err.Error()
		d.Playback = decideMediaPlayback(ext, mediaProbeInfo{}, false)
		// 失败**不进缓存**（与转换服务同一条纪律：一次失败不该永久失败）。
		return d
	}
	d.Available = true
	info, streams, durMs, bitRate := probeInfoFrom(out)
	d.Format = out.Format.FormatName
	d.DurationMs = durMs
	d.BitRate = bitRate
	d.Streams = streams
	d.Playback = decideMediaPlayback(ext, info, true)
	mediaProbeCachePut(abs, d)
	return d
}

// handleMediaProbe `media_probe` 的 dispatch 分支实现。
// 路径口径与 file_preview 完全一致（同一个 resolveWorkspacePath）：越界/受保护目录的
// 拒绝理由不该因为「看的是视频」而变。
func (m *manager) handleMediaProbe(c command, ws string) {
	raw := str(c.Payload, "path")
	if raw == "" {
		m.resp(c, false, "path 为空", nil)
		return
	}
	if ws == "" && !filepath.IsAbs(raw) {
		m.resp(c, false, "workspace 为空", nil)
		return
	}
	abs, err := resolveWorkspacePath(ws, raw)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	st, err := os.Stat(abs)
	if err != nil {
		m.resp(c, false, "无法读取文件: "+err.Error(), nil)
		return
	}
	if st.IsDir() {
		m.resp(c, false, "path 是目录，不是媒体文件: "+abs, nil)
		return
	}
	if !isMediaExt(mediaExtOf(abs)) {
		m.resp(c, false, "不是媒体文件（扩展名不在编解码矩阵里）: "+abs, nil)
		return
	}
	m.resp(c, true, "", probeMediaFile(abs, st.Size(), st.ModTime().UnixMilli()).payload())
}
