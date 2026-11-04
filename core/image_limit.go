package core

// MaxImageSidePx 单张图片单边像素的硬上限（视觉上游/网关的运输约束，与体积无关）。
//
// 实测依据（2026-11-04，opendesign 渠道 + deepseek-v4.1-flash，同一 key 同一端点）：
//
//	390x8192  → 200            390x8193 → 400
//	390x8200  → 400            9000x390 → 400
//	8192x8192 → 200（6700 万像素、5.4MB）
//	1440x4151 → 200（1.0MB）   390x9697 → 400（837KB，合法 PNG）
//
// ⇒ 判据只有「单边边长」，与字节数、像素总数、长宽比都无关。而超限的报错文案却写成
// 格式错误（"You have uploaded an unsupported image … webp, png, jpeg, and gif"），
// 极易被误诊成「图片损坏 / 格式不支持」——真实的 390x9697 PNG 完全合法（CRC、解码
// 都验过）。取值 8192：不砍掉本来就合格的常规长截图（1440x4151 / 2560x5644 都收）。
//
// 两个使用点必须同源：
//   - tools/builtin/image.go：read_file 读图时就切片/降采样（保住原生分辨率）；
//   - provider/imageguard.go：请求组装前最后一道闸（覆盖 MCP / 截图 / IM 附件等
//     其它图片来源），超限图降级为文本说明而不是让整轮 run 400 死掉。
const MaxImageSidePx = 8192
