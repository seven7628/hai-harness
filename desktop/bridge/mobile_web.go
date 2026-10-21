// mobile_web.go —— 把手机端 PWA 打进 bridge 二进制。
//
// 无构建步骤（P0 取舍）：一个 index.html + app.js + style.css，go:embed 直接托管，
// 换工作区/装到别的机器都不用 npm。等交互复杂到需要组件化再迁 Vite。
package main

import "embed"

//go:embed mobile_web
var mobileWebFS embed.FS
