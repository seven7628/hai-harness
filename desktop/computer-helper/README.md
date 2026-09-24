# computer-helper（macOS Swift 薄适配器，协议 v1 服务端）

Computer Use 的 macOS 原生载体：读 stdin NDJSON 请求 → 调系统 API（AX/
CGEvent/ScreenCaptureKit）→ 回 stdout NDJSON 响应。**零业务规则**（决策全在
`computer/` Go 内核）；与 Go 侧契约见 `computer/proto.go` / `computer/model.go`。

## 构建

```sh
swiftc -O -o computer-helper computer_helper.swift
```

- swiftc 随 Command Line Tools（M0 实测 Swift 6.1.2 可用）；产物为 arm64 Mach-O。
- `computer-helper` 为本地构建产物，不入库；分发随 app extraResources
  （对齐 `dist/hai-bridge`，见 `desktop/app/package.json` + electron-builder）。

## 冒烟

```sh
printf '%s\n' \
  '{"id":1,"cmd":"hello","params":{"version_major":1,"version_minor":0}}' \
  '{"id":2,"cmd":"perm_status"}' \
  '{"id":3,"cmd":"screen_info"}' | ./computer-helper
```

## 端到端（Go 客户端 ↔ helper）

```sh
go run ./computer/cmd/computer_check desktop/computer-helper/computer-helper
```

## 命令面

hello / perm_status / screen_info / snapshot / press / click / type / key /
open_app / activate / find / verify（snapshot 需辅助功能授权：
`AXIsProcessTrusted()`，TCC 归属待宿主 app 场景终裁，见设计文档 §12.9）。

screenshot 实现保留（ScreenCaptureKit/CGWindowList，见 cmdScreenshot）但暂未启用
（档 V 暂缓）；ocr 已随死代码清理移除（见设计文档 §4.1 尾注）。
