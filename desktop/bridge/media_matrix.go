// media_matrix.go — 「这个媒体能不能直放」的编解码矩阵：**一张表 + 一个通用判定函数**。
//
// 为什么必须是数据而不是散在 if 里（02 §PR-08c 的原话）：能不能直放取决于
// 「容器 × 视频编码 × 音频编码」两三百种组合里的一小部分，写成 if/else 之后：
//
//	· 新加一个容器（比如 vp9-in-mp4）要在三处改，漏一处就变成「有时能播有时不能」；
//	· 没有任何一行代码能回答「现在到底直放哪些组合」——评审只能靠读分支；
//	· 前端与后端容易各判一次，两边不一致时用户看到的是「能点播放但黑屏」。
//
// 所以：容器规则是表（mediaContainerRules），编码白名单也是表里的一列，
// 判定函数 decideMediaPlayback 只做「查表 + 比较」，一行业务判断都不含。
//
// 矩阵口径来自 05 §4 D-L（Electron 自带 ffmpeg，但只含常见组合）：
//
//	容器/编码            能否直接播   策略
//	mp4 / h264 + aac     ✅          直接播
//	webm / vp9 + opus    ✅          直接播
//	mp3 / wav / ogg/flac ✅          <audio>
//	mov (h264)           ✅（多数）   直接播
//	mkv / avi / ts/m2ts  ❌（容器）   提示 + 系统播放器（转码是 PR-08e）
//	hevc / av1 / prores  ⚠️ 视构建    探测到不在表里 → 判为需转码
//	ac3 / dts 音轨       ❌          需转码
//
// 「需转码」在本 PR 里**不真的转**（转码是 PR-08e）：前端只显示「需要转码（可打开系统
// 播放器）」。判定的价值在于：用户不用点播放、等黑屏、再猜为什么。
package main

import "strings"

// 播放模式（前端按字符串分支，所以取值是协议的一部分，不是内部常量）。
const (
	playDirect    = "direct"    // 可以直接塞进 <video>/<audio>
	playTranscode = "transcode" // 容器或编码不在直放名单：需要转码（本轮只提示）
	playUnknown   = "unknown"   // 信息不足/容器未知：试播，失败再系统播放器
)

// mediaContainerRule 一个容器族（一组扩展名共享同一套判断）。
type mediaContainerRule struct {
	// Exts 扩展名（小写、不含点）。同一族共享规则，但判定结果里的 container 保留用户
	// 文件真实的扩展名（.m4v 不该被显示成 .mp4）。
	Exts []string
	// Kind video / audio。audio 只喂 <audio>，不给 <video> 挂一个 0 字节的画面。
	Kind string
	// Mode 容器本身给出的结论（direct / transcode）。
	Mode string
	// VideoCodecs / AudioCodecs 直放允许的编码（ffprobe 的 codec_name）。
	// 空 = 这一维不设限（例如 mp3 文件里只可能有音频轨）。
	VideoCodecs []string
	AudioCodecs []string
	// Reason 触发 transcode 时给用户看的一句话（写清是容器的锅还是编码的锅）。
	Reason string
}

// mediaContainerRules 直放矩阵本体。**改这里就是改产品行为**，所以有
// TestMediaMatrixTableIsCoherent 把「扩展名全局唯一」「transcode 的规则必须写明理由」
// 这些不变量钉住，避免后人加一行表就悄悄改了口径。
var mediaContainerRules = []mediaContainerRule{
	{
		Exts: []string{"mp4", "m4v"}, Kind: "video", Mode: playDirect,
		VideoCodecs: []string{"h264", "avc1"},
		AudioCodecs: []string{"aac", "mp3"},
	},
	{
		// mov 与 mp4 同族（QuickTime 容器 + H.264/AAC 是 Electron 的日常组合）。
		Exts: []string{"mov"}, Kind: "video", Mode: playDirect,
		VideoCodecs: []string{"h264", "avc1"},
		AudioCodecs: []string{"aac", "mp3"},
	},
	{
		Exts: []string{"webm"}, Kind: "video", Mode: playDirect,
		VideoCodecs: []string{"vp8", "vp9", "av1"},
		AudioCodecs: []string{"opus", "vorbis"},
	},
	{
		Exts: []string{"ogv"}, Kind: "video", Mode: playDirect,
		VideoCodecs: []string{"theora", "vp8", "vp9"},
		AudioCodecs: []string{"vorbis", "opus"},
	},
	{
		Exts: []string{"3gp"}, Kind: "video", Mode: playDirect,
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
	},
	{
		// 三个必须转码的容器：Electron 的 demuxer 不认（不是编码问题）。
		Exts: []string{"mkv"}, Kind: "video", Mode: playTranscode,
		Reason: "Matroska（mkv）容器不在 Electron 直放名单",
	},
	{
		Exts: []string{"avi"}, Kind: "video", Mode: playTranscode,
		Reason: "AVI 容器不在 Electron 直放名单",
	},
	{
		Exts: []string{"ts", "m2ts", "mts"}, Kind: "video", Mode: playTranscode,
		Reason: "MPEG-TS 容器不在 Electron 直放名单",
	},
	{
		Exts: []string{"mp3"}, Kind: "audio", Mode: playDirect,
		AudioCodecs: []string{"mp3"},
	},
	{
		Exts: []string{"m4a"}, Kind: "audio", Mode: playDirect,
		AudioCodecs: []string{"aac", "alac"},
	},
	{
		Exts: []string{"aac"}, Kind: "audio", Mode: playDirect,
		AudioCodecs: []string{"aac"},
	},
	{
		// wav 的 codec_name 是一串 pcm_* 变体（pcm_s16le/pcm_s24le/pcm_f32le…），
		// 逐个列会漏；前缀匹配由 codecAllowed 处理（见其注释）。
		Exts: []string{"wav"}, Kind: "audio", Mode: playDirect,
		AudioCodecs: []string{"pcm_"},
	},
	{
		Exts: []string{"ogg", "oga"}, Kind: "audio", Mode: playDirect,
		AudioCodecs: []string{"vorbis", "opus", "flac"},
	},
	{
		Exts: []string{"opus"}, Kind: "audio", Mode: playDirect,
		AudioCodecs: []string{"opus"},
	},
	{
		Exts: []string{"flac"}, Kind: "audio", Mode: playDirect,
		AudioCodecs: []string{"flac"},
	},
	{
		Exts: []string{"wma"}, Kind: "audio", Mode: playTranscode,
		Reason: "WMA 不在 Electron 直放名单",
	},
}

// mediaPlayback 判定结果（进 JSON，前端只渲染不重算）。
type mediaPlayback struct {
	Mode       string `json:"mode"`        // direct / transcode / unknown
	Kind       string `json:"kind"`        // video / audio
	Container  string `json:"container"`   // 用户文件的扩展名（小写）
	VideoCodec string `json:"video_codec"` // 来自 ffprobe（可能为空）
	AudioCodec string `json:"audio_codec"` //
	Reason     string `json:"reason"`      // 给用户看的一句（transcode/unknown 必填）
	Probed     bool   `json:"probed"`      // 结论是否用上了 ffprobe（false = 只看容器）
}

// mediaProbeInfo 判定需要的探测事实（ffprobe 不在线时为零值）。
type mediaProbeInfo struct {
	FormatName string // ffprobe format.format_name（可能是逗号列表）
	VideoCodec string
	AudioCodec string
	HasVideo   bool
	HasAudio   bool
}

// isMediaExt 是否媒体扩展名 —— 判据就是「矩阵里有没有它的规则」，
// 所以**不存在第二份扩展名清单**：加一个容器只改 mediaContainerRules 一处。
// （协议白名单在 electron/media-protocol.ts，两边由 media-protocol.test.ts 的
// 跨文件断言互相钉住。）
func isMediaExt(ext string) bool { return mediaRuleForExt(ext) != nil }

// mediaExtOf 文件扩展名的小写形式（不含点）。
func mediaExtOf(p string) string {
	i := strings.LastIndexByte(p, '.')
	if i <= 0 || i == len(p)-1 {
		return ""
	}
	return strings.ToLower(p[i+1:])
}

// mediaRuleForExt 扩展名 → 规则（nil = 未知容器）。
func mediaRuleForExt(ext string) *mediaContainerRule {
	for i := range mediaContainerRules {
		for _, e := range mediaContainerRules[i].Exts {
			if e == ext {
				return &mediaContainerRules[i]
			}
		}
	}
	return nil
}

// mediaFamilyOfFormat ffprobe 的 format_name → 规则族。format_name 是逗号分隔的别名列表
// （`mov,mp4,m4a,3gp,3g2,mj2`），任一别名命中即算这一族。**只在扩展名未知时兜底**：
// 扩展名是我们自己维护的口径，比 demuxer 自报的别名列表更稳定。
func mediaFamilyOfFormat(formatName string) string {
	for _, alias := range strings.Split(strings.ToLower(formatName), ",") {
		alias = strings.TrimSpace(alias)
		switch alias {
		case "":
			continue
		case "mov", "mp4", "m4a", "3gp", "3g2", "mj2":
			return "mp4"
		case "matroska":
			return "mkv"
		case "webm":
			return "webm"
		case "avi":
			return "avi"
		case "mpegts":
			return "ts"
		case "mp3":
			return "mp3"
		case "wav":
			return "wav"
		case "flac":
			return "flac"
		case "ogg":
			return "ogg"
		}
	}
	return ""
}

// codecAllowed 编码是否在允许列表里。**前缀匹配**而不是全等：
// wav 的 codec_name 是 pcm_s16le/pcm_s24le/pcm_f32le…（列全必漏），
// 表里写 `pcm_` 就覆盖整族；其余条目全是精确名字，前缀匹配对它们等价于全等
// （`h264` 不会误配 `h264_qsv`？会 —— 但那也确实是 h264，可直放，故可接受）。
func codecAllowed(codec string, allowed []string) bool {
	if codec == "" || len(allowed) == 0 {
		return true // 没有这一维 / 探不到：不因为「不知道」就判死
	}
	c := strings.ToLower(codec)
	for _, a := range allowed {
		if strings.HasPrefix(c, a) {
			return true
		}
	}
	return false
}

// decideMediaPlayback 矩阵的**唯一**判定入口：查表 → 比较 → 给一句理由。
// 顺序刻意是「扩展名优先、ffprobe 兜底」：
//  1. 扩展名命中且规则是 transcode → 直接 transcode（容器不支持，编码再好也没用）；
//  2. 扩展名命中但没探测到 → 按容器默认（mp4 默认能播；这是「没有 ffprobe 也能放 mp4」）；
//  3. 扩展名命中且探测到 → 用编码白名单收紧（hevc/ac3 这类会被降级为 transcode）；
//  4. 扩展名未知 → 用 ffprobe 的 format_name 找族；再不行按 codec_name 判；
//  5. 什么都判不出来 → unknown（前端提示「先试播」而不是「不能播」）。
func decideMediaPlayback(ext string, info mediaProbeInfo, probed bool) mediaPlayback {
	pb := mediaPlayback{Container: ext, VideoCodec: info.VideoCodec, AudioCodec: info.AudioCodec, Probed: probed}
	rule := mediaRuleForExt(ext)
	if rule == nil && probed {
		if fam := mediaFamilyOfFormat(info.FormatName); fam != "" {
			rule = mediaRuleForExt(fam)
		}
	}
	if rule == nil {
		// 未知容器：按编码说话。有视频编码且是 Electron 的常见直放编码 → 值得一试。
		pb.Kind = "video"
		if !info.HasVideo && info.HasAudio {
			pb.Kind = "audio"
		}
		switch {
		case !probed:
			pb.Mode = playUnknown
			pb.Reason = "未知容器，且没有 ffprobe 信息：先试播，失败用系统播放器"
		case codecAllowed(info.VideoCodec, []string{"h264", "avc1", "vp8", "vp9", "av1"}) && info.HasVideo:
			pb.Mode = playUnknown
			pb.Reason = "容器未在直放名单，编码 " + info.VideoCodec + " 本身可直放：先试播，失败用系统播放器"
		case info.HasAudio && !info.HasVideo && codecAllowed(info.AudioCodec, []string{"aac", "mp3", "vorbis", "opus", "flac", "pcm_"}):
			pb.Kind = "audio"
			pb.Mode = playUnknown
			pb.Reason = "容器未在直放名单，音频编码 " + info.AudioCodec + " 可直放：先试播，失败用系统播放器"
		default:
			pb.Mode = playTranscode
			pb.Reason = "容器与编码都不在直放名单"
		}
		return pb
	}
	pb.Kind = rule.Kind
	if rule.Mode == playTranscode {
		pb.Mode = playTranscode
		pb.Reason = rule.Reason
		return pb
	}
	if !probed {
		// 容器在直放名单、但探测不可用：按容器默认放行（这正是「藏掉 ffprobe 仍能播mp4」）。
		pb.Mode = playDirect
		pb.Reason = "没有 ffprobe：按容器默认判断（" + ext + "）"
		return pb
	}
	pb.Mode = playDirect
	if info.HasVideo && !codecAllowed(info.VideoCodec, rule.VideoCodecs) {
		pb.Mode = playTranscode
		pb.Reason = "视频编码 " + info.VideoCodec + " 不在 " + ext + " 的直放名单（需要转码）"
		return pb
	}
	if info.HasAudio && !codecAllowed(info.AudioCodec, rule.AudioCodecs) {
		pb.Mode = playTranscode
		pb.Reason = "音轨编码 " + info.AudioCodec + " 不在 " + ext + " 的直放名单（需要转码）"
		return pb
	}
	return pb
}
