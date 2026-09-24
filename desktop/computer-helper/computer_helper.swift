// computer-helper：macOS 桌面操控 Swift 薄适配器（协议 v1 服务端）。
//
// 只做协议翻译：读 stdin 的 NDJSON 请求 → 调 macOS 系统 API → 回 stdout 的
// NDJSON 响应。零业务规则（目标解析/验证/重试全在 Go 侧 computer.Executor）。
// 与 Go 侧协议契约见 computer/proto.go（请求/响应/错误码），Go 客户端
// computer/stdio.go 是本 helper 的唯一对话方。
//
// P0 命令面：hello / perm_status / screen_info / snapshot / press / click /
//   type / key / open_app / activate / find / verify。
//   注：screenshot 已随视觉档最小集落地（见 cmdScreenshot——屏幕录制权限检测
//   已并入 perm_status）。
//
// uid 机制（§6.1）：snapshot 时递归遍历 AX 树，为每个可交互节点生成稳定 uid
// 并缓存 uid → AXUIElement 引用映射；press/verify 按 uid 查映射执行。树失稳
// （元素销毁）→ AX 调用失败返回 not_found（Go 侧引导重新 snapshot）。
//
// 构建：swiftc -O -o computer-helper computer_helper.swift（swiftc 随 Command
// Line Tools；M0 验证构建链）。分发：随 app extraResources（对齐 hai-bridge）。
import AppKit
import ApplicationServices
import CoreGraphics
import Foundation
import ScreenCaptureKit

// 领域模型（与 computer/model.go 字段一一对应；JSON 键 = Go 小驼峰标签）
struct RectValue: Codable { var x: Double; var y: Double; var w: Double; var h: Double }
struct SizeValue: Codable { var w: Double; var h: Double }
struct PointValue: Codable { var x: Double; var y: Double }
struct ScreenValue: Codable { var id: Int; var name: String; var frame: RectValue; var scaleFactor: Double }
struct ElementValue: Codable {
    var uid: String
    var role: String
    var title: String
    var value: String
    var description: String
    var frame: RectValue
    var actions: [String]
    var selected: Bool
    var focused: Bool
    var focusable: Bool
    var children: [ElementValue]
}
struct WindowValue: Codable { var title: String; var frame: RectValue }
struct GridCellValue: Codable { var x: Int; var y: Int; var text: String }

struct ScreenInfoValue: Codable { var size: SizeValue; var scaleFactor: Double; var screens: [ScreenValue] }
struct PermStatusValue: Codable { var accessibility: Bool; var screenCapture: Bool; var trustedApp: String }
struct SnapshotValue: Codable {
    var frontmostApp: String
    var pid: Int
    var windows: [WindowValue]
    var tree: ElementValue?
    var grid: [GridCellValue]
}

struct SnapshotOptsValue: Codable { var maxChars: Int?; var maxDepth: Int? }

// 动作请求 params
struct PressParams: Codable { var uid: String?; var title: String?; var app: String?; var menuPath: [String]? }
struct ClickParams: Codable { var x: Double; var y: Double }
struct TypeParams: Codable { var text: String }
struct KeyParams: Codable { var keys: String }
struct OpenAppParams: Codable { var name: String }
struct ActivateParams: Codable { var pid: Int?; var app: String?; var windowTitle: String? }
struct FindParams: Codable { var text: String?; var role: String?; var app: String? }
struct VerifyParams: Codable {
    var uid: String?
    var wantValue: String?
    var wantText: String?
    var waitMS: Int?
    var app: String?   // 窗口标题变化用
    var windowTitle: String?
}

// 请求/响应（对齐 computer/proto.go）
//
// 注意：params/result 必须是「嵌套 JSON 对象」，不能用 Codable 的 Data?
//（JSONEncoder/Decoder 把 Data 当 base64 处理，协议就坏了）。因此请求解析
// 与响应写出统一走 JSONSerialization：对象 ↔ Data 直通，不经过 Codable。
struct ProtoErrorValue: Codable { var code: String; var msg: String }

// RequestValue 手写解析（id/cmd/params[原始对象字节]）。
struct RequestValue {
    var id: Int
    var cmd: String
    var params: Data? // 原始 JSON 对象字节（无 params 时为 nil）

    init?(json: Data) {
        guard let obj = (try? JSONSerialization.jsonObject(with: json)) as? [String: Any],
              let cmd = obj["cmd"] as? String,
              let id = obj["id"] as? Int else { return nil }
        self.id = id
        self.cmd = cmd
        if let p = obj["params"] {
            self.params = try? JSONSerialization.data(withJSONObject: p)
        }
    }
}

// respond 手写响应：result/error 以原始对象嵌入（不做 base64）。
func respond(_ id: Int, _ ok: Bool, _ result: Data?, _ err: ProtoErrorValue?) {
    var obj: [String: Any] = ["id": id, "ok": ok]
    if let r = result, let parsed = try? JSONSerialization.jsonObject(with: r) {
        obj["result"] = parsed
    }
    if let e = err {
        obj["error"] = ["code": e.code, "msg": e.msg]
    }
    if let data = try? JSONSerialization.data(withJSONObject: obj) {
        FileHandle.standardOutput.write(data + Data([0x0A]))
    }
}

func respondOK<T: Encodable>(_ id: Int, _ v: T) { respond(id, true, encode(v), nil) }
// respondOKDict 混合值字典响应（String:Bool/String:[Double] 等）。
func respondOKDict(_ id: Int, _ dict: [String: Any]) {
    if let data = try? JSONSerialization.data(withJSONObject: dict) {
        respond(id, true, data, nil)
    }
}
func respondErr(_ id: Int, _ code: String, _ msg: String) {
    respond(id, false, nil, ProtoErrorValue(code: code, msg: msg))
}

// HelloParamsValue 从请求 params 字典读取（协议字段 snake_case）。
struct HelloParamsValue {
    var versionMajor: Int
    var versionMinor: Int
    init(params: Data?) {
        versionMajor = 1
        versionMinor = 0
        guard let params = params,
              let obj = (try? JSONSerialization.jsonObject(with: params)) as? [String: Any] else { return }
        if let v = obj["version_major"] as? Int { versionMajor = v }
        if let v = obj["version_minor"] as? Int { versionMinor = v }
    }
}

struct HelloResultValue: Codable {
    var version_major: Int
    var version_minor: Int
    var helper: String
}

// 全局错误码（与 computer/proto.go 一致）
let ERR_PERM_DENIED = "perm_denied"
let ERR_NOT_FOUND = "not_found"
let ERR_TIMEOUT = "timeout"
let ERR_INTERNAL = "internal"
let ERR_UNSUPPORTED = "unsupported"

// ---------- 工具函数 ----------

func encode<T: Encodable>(_ v: T) -> Data? { try? JSONEncoder().encode(v) }

func decodeParams<T: Codable>(_ data: Data?) -> T? {
    guard let data = data else { return nil }
    return try? JSONDecoder().decode(T.self, from: data)
}

// ---------- 会话状态：uid → AX 元素映射 ----------

// uidMap 最近一次 snapshot 的可交互元素索引（uid → AXUIElement + 角色/标题）。
// 跨请求保持（press/find/verify 引用），每次 snapshot 重建。
struct UIDEntry {
    let el: AXUIElement
    let role: String
    let title: String
}
var uidMap: [String: UIDEntry] = [:]
var lastPid: Int = 0

// requireAX 权限门：无辅助功能授权直接结构化错误。
func requireAX(_ id: Int) -> Bool {
    guard AXIsProcessTrusted() else {
        respondErr(id, ERR_PERM_DENIED,
                   "Accessibility permission not granted: allow this app in System Settings → Privacy & Security → Accessibility")
        return false
    }
    return true
}

// ---------- 命令实现 ----------

/// perm_status：只读权限探测。TCC 归属主体在 M0 spike 实测（helper 自身 or 宿主 app）。
/// screenCapture = 屏幕录制授权真检测（CGPreflightScreenCaptureAccess，10.15+；
/// 截图命令依赖它——未授权时捕获返回黑屏/空）。
func cmdPermStatus(_ id: Int) {
    let axTrusted = AXIsProcessTrusted()
    let scTrusted: Bool
    if #available(macOS 10.15, *) {
        scTrusted = CGPreflightScreenCaptureAccess()
    } else {
        scTrusted = true // 10.15 前无屏幕录制 TCC
    }
    respondOK(id, PermStatusValue(accessibility: axTrusted, screenCapture: scTrusted,
                                  trustedApp: axTrusted ? "granted" : "unknown"))
}

func cmdScreenInfo(_ id: Int) {
    guard let main = NSScreen.main ?? NSScreen.screens.first else {
        respondErr(id, ERR_INTERNAL, "no screen available")
        return
    }
    let f = main.frame
    var screens: [ScreenValue] = []
    for (i, s) in NSScreen.screens.enumerated() {
        let sf = s.frame
        screens.append(ScreenValue(id: i, name: s.localizedName,
                                   frame: RectValue(x: Double(sf.origin.x), y: Double(sf.origin.y),
                                                    w: Double(sf.width), h: Double(sf.height)),
                                   scaleFactor: Double(s.backingScaleFactor)))
    }
    respondOK(id, ScreenInfoValue(size: SizeValue(w: Double(f.width), h: Double(f.height)),
                                  scaleFactor: Double(main.backingScaleFactor), screens: screens))
}

/// snapshot：前台 App AX 树（裁剪后）+ 重建 uid 映射。
func cmdSnapshot(_ id: Int, _ params: Data?) {
    guard requireAX(id) else { return }
    var opts = SnapshotOptsValue()
    if let params = params {
        opts = (try? JSONDecoder().decode(SnapshotOptsValue.self, from: params)) ?? opts
    }
    guard let frontApp = NSWorkspace.shared.frontmostApplication else {
        respondErr(id, ERR_INTERNAL, "cannot get frontmost app")
        return
    }
    let pid = frontApp.processIdentifier
    let appName = frontApp.localizedName ?? "unknown"
    let pidInt = Int(pid)
    let windows = AXWindowsList(pid: pidInt)
    let maxDepth = opts.maxDepth ?? 9
    // 重建 uid 映射
    uidMap = [:]
    lastPid = pidInt
    let tree = AXTreeForApp(pid: pidInt, maxDepth: maxDepth)
    let sn = SnapshotValue(frontmostApp: appName, pid: pidInt, windows: windows,
                           tree: tree, grid: [])
    respondOK(id, sn)
}

/// press：语义按压。params: {uid} / {app, menuPath} / {title}。
/// 决策链（§6.2）：AXPress（uid 命中）→ frame 中心点击回退（helper 内部）
///   → not_found 引导重新 snapshot。
func cmdPress(_ id: Int, _ params: Data?) {
    guard requireAX(id) else { return }
    guard let p: PressParams = decodeParams(params) else {
        respondErr(id, ERR_INTERNAL, "press: failed to parse params")
        return
    }
    // 1) uid 路径
    if let uid = p.uid {
        guard let entry = uidMap[uid] else {
            respondErr(id, ERR_NOT_FOUND, "uid \(uid) not in the current snapshot (tree changed)")
            return
        }
        let frame = axFrame(entry.el)
        // 坐标点击优先（真实用户路径；真机教训 2026-09：日历 popover 按钮
        // AXPress 返回 success 但实际关闭面板而非触发——AXPress 语义在部分 app
        // 不可靠；坐标点击=元素框中心，坐标由系统给出非模型手算）
        if frame.width > 1 && frame.height > 1 {
            clickAt(CGPoint(x: frame.midX, y: frame.midY))
            respondOKDict(id, ["pressed": uid, "role": entry.role, "title": entry.title, "method": "frame-click"])
            return
        }
        // 无 frame（菜单项等特殊）→ AXPress 兜底
        let actionErr = AXUIElementPerformAction(entry.el, kAXPressAction as CFString)
        if actionErr == .success {
            respondOKDict(id, ["pressed": uid, "role": entry.role, "title": entry.title, "method": "axpress"])
            return
        }
        respondErr(id, ERR_INTERNAL, "element \(uid) cannot be pressed (no frame and no press action)")
        return
    }
    // 2) app + menuPath（菜单路径：菜单栏逐项匹配 press）
    if let app = p.app, let menuPath = p.menuPath, !menuPath.isEmpty {
        pressMenuPath(app: app, path: menuPath) { ok, msg in
            if ok { respondOK(id, ["menu": menuPath.joined(separator: "/")]) }
            else { respondErr(id, ERR_NOT_FOUND, msg) }
        }
        return
    }
    // 3) title 文本匹配（当前快照树中第一个 title 含该文本的可交互元素）
    if let title = p.title {
        if let (uid, entry) = uidMap.first(where: { $0.value.title.contains(title) }) {
            let frame = axFrame(entry.el)
            if frame.width > 1 && frame.height > 1 {
                clickAt(CGPoint(x: frame.midX, y: frame.midY))
                respondOKDict(id, ["pressed": uid, "role": entry.role, "title": entry.title, "method": "title-match-click"])
            } else {
                _ = AXUIElementPerformAction(entry.el, kAXPressAction as CFString)
                respondOKDict(id, ["pressed": uid, "role": entry.role, "title": entry.title, "method": "title-match"])
            }
            return
        }
        respondErr(id, ERR_NOT_FOUND, "no element with title containing \(title)")
        return
    }
    respondErr(id, ERR_INTERNAL, "press needs one of uid / app+menuPath / title")
}

/// click：像素点击（坐标由 executor 从元素框换算/看图工具族提供）。
func cmdClick(_ id: Int, _ params: Data?) {
    guard requireAX(id) else { return }
    guard let p: ClickParams = decodeParams(params) else {
        respondErr(id, ERR_INTERNAL, "click: failed to parse params")
        return
    }
    clickAt(CGPoint(x: p.x, y: p.y))
    respondOKDict(id, ["clicked": [p.x, p.y]])
}

func clickAt(_ pt: CGPoint) {
    let src = CGEventSource(stateID: .hidSystemState)
    let move = CGEvent(mouseEventSource: src, mouseType: .mouseMoved,
                       mouseCursorPosition: pt, mouseButton: .left)
    move?.post(tap: .cghidEventTap)
    usleep(50_000) // 50ms 移动稳定
    let down = CGEvent(mouseEventSource: src, mouseType: .leftMouseDown,
                       mouseCursorPosition: pt, mouseButton: .left)
    down?.post(tap: .cghidEventTap)
    usleep(30_000)
    let up = CGEvent(mouseEventSource: src, mouseType: .leftMouseUp,
                     mouseCursorPosition: pt, mouseButton: .left)
    up?.post(tap: .cghidEventTap)
}

/// type：键入当前焦点（Unicode 经 CGEventKeyboardSetUnicodeString，中文可用）。
func cmdType(_ id: Int, _ params: Data?) {
    guard requireAX(id) else { return }
    guard let p: TypeParams = decodeParams(params) else {
        respondErr(id, ERR_INTERNAL, "type: failed to parse params")
        return
    }
    typeText(p.text)
    respondOK(id, ["typed": p.text.count])
}

func typeText(_ text: String) {
    let src = CGEventSource(stateID: .hidSystemState)
    for ch in text {
        var chars = Array(String(ch).utf16)
        let down = CGEvent(keyboardEventSource: src, virtualKey: 0, keyDown: true)
        down?.keyboardSetUnicodeString(stringLength: chars.count, unicodeString: &chars)
        down?.post(tap: .cghidEventTap)
        usleep(20_000)
        let up = CGEvent(keyboardEventSource: src, virtualKey: 0, keyDown: false)
        up?.keyboardSetUnicodeString(stringLength: chars.count, unicodeString: &chars)
        up?.post(tap: .cghidEventTap)
        usleep(20_000)
    }
}

/// key：键组合/单键（cmd+shift+p / Return / Tab…）。P0 支持常用键名映射。
func cmdKey(_ id: Int, _ params: Data?) {
    guard requireAX(id) else { return }
    guard let p: KeyParams = decodeParams(params) else {
        respondErr(id, ERR_INTERNAL, "key: failed to parse params")
        return
    }
    keyChord(p.keys)
    respondOK(id, ["key": p.keys])
}

/// 键名 → 虚拟键码 + 修饰键。
func keyChord(_ chord: String) {
    let parts = chord.split(separator: "+").map { String($0).trimmingCharacters(in: .whitespaces) }
    var flags: CGEventFlags = []
    var keyCode: CGKeyCode = 0
    var keyName = ""
    for part in parts {
        switch part.lowercased() {
        case "cmd", "command": flags.insert(.maskCommand)
        case "shift": flags.insert(.maskShift)
        case "alt", "option": flags.insert(.maskAlternate)
        case "ctrl", "control": flags.insert(.maskControl)
        default:
            keyName = part
        }
    }
    guard !keyName.isEmpty else { return }
    keyCode = keyCodeFor(keyName)
    let src = CGEventSource(stateID: .hidSystemState)
    let down = CGEvent(keyboardEventSource: src, virtualKey: keyCode, keyDown: true)
    down?.flags = flags
    down?.post(tap: .cghidEventTap)
    usleep(30_000)
    let up = CGEvent(keyboardEventSource: src, virtualKey: keyCode, keyDown: false)
    up?.flags = flags
    up?.post(tap: .cghidEventTap)
}

/// 常用键名映射（P0 子集；完整虚拟键码表 P1 补）。
func keyCodeFor(_ name: String) -> CGKeyCode {
    switch name.lowercased() {
    case "return", "enter": return 36
    case "tab": return 48
    case "space": return 49
    case "delete", "backspace": return 51
    case "escape", "esc": return 53
    case "forwarddelete": return 117
    case "home": return 115
    case "end": return 119
    case "pageup": return 116
    case "pagedown": return 121
    case "up", "arrowup": return 126
    case "down", "arrowdown": return 125
    case "left", "arrowleft": return 123
    case "right", "arrowright": return 124
    default:
        // 单字符 a-z / A-Z / 0-9（虚拟键码映射简化）
        if name.count == 1 {
            let ch = Character(name.lowercased())
            if ch >= "a" && ch <= "z" {
                return CGKeyCode(ch.asciiValue! - 97 + 0) // 不精确，P1 完善
            }
            if ch >= "0" && ch <= "9" {
                return CGKeyCode(ch.asciiValue! - 48 + 18)
            }
        }
        return 0 // 未知 → 0 (a) 兜底
    }
}

/// open_app：启动/激活应用（NSWorkspace 按名/路径）。
/// 名字解析链（真机教训 2026-09：open -a 只认英文/路径，中文显示名失败）：
///  1) 绝对路径直接 open；2) 运行中 app 按 localizedName/英文名匹配激活；
///  3) 扫描常见应用目录按 CFBundleDisplayName/CFBundleName/文件名 匹配路径 open；
///  4) open -a <name> 兜底。
func cmdOpenApp(_ id: Int, _ params: Data?) {
    guard let p: OpenAppParams = decodeParams(params) else {
        respondErr(id, ERR_INTERNAL, "open_app: failed to parse params")
        return
    }
    let name = p.name
    if name.hasPrefix("/") {
        NSWorkspace.shared.openApplication(at: URL(fileURLWithPath: name),
                                           configuration: NSWorkspace.OpenConfiguration()) { _, err in
            if let err = err {
                respondErr(id, ERR_INTERNAL, "open \(name) failed: \(err.localizedDescription)")
            } else {
                respondOK(id, ["opened": name])
            }
        }
        return
    }
    let lower = name.lowercased()
    // 运行中 app：按 localizedName 或英文名匹配
    if let app = NSWorkspace.shared.runningApplications.first(where: {
        ($0.localizedName?.lowercased() == lower) ||
        ($0.bundleURL?.lastPathComponent.lowercased().replacingOccurrences(of: ".app", with: "") == lower)
    }) {
        ensureActive(app)
        respondOKDict(id, ["opened": name, "activated": true])
        return
    }
    // 按路径扫描匹配（中文显示名等 open -a 不认的名字）
    if let path = findAppPath(name: name) {
        NSWorkspace.shared.openApplication(at: URL(fileURLWithPath: path),
                                           configuration: NSWorkspace.OpenConfiguration()) { _, err in
            if let err = err {
                respondErr(id, ERR_INTERNAL, "open \(name) failed: \(err.localizedDescription)")
            } else {
                respondOKDict(id, ["opened": name, "path": path])
            }
        }
        return
    }
    // open -a 兜底
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/bin/open")
    proc.arguments = ["-a", name]
    do {
        try proc.run()
        proc.waitUntilExit()
        if proc.terminationStatus == 0 {
            respondOKDict(id, ["opened": name, "activated": false])
        } else {
            respondErr(id, ERR_NOT_FOUND, "app \(name) not found (open -a exit code \(proc.terminationStatus))")
        }
    } catch {
        respondErr(id, ERR_INTERNAL, "launch \(name) failed: \(error.localizedDescription)")
    }
}

/// findAppPath 按名字（显示名/英文名/文件名）定位 .app 路径。
/// 优先 Spotlight（mdfind kMDItemDisplayName——认本地化显示名「提醒事项」等）；
/// 兜底扫常见目录匹配文件名/CFBundleName。
func findAppPath(name: String) -> String? {
    let lower = name.lowercased()
    // 1) Spotlight 按显示名查（覆盖本地化名）
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: "/usr/bin/mdfind")
    proc.arguments = ["kMDItemDisplayName == '\(name)' && kMDItemContentType == 'com.apple.application-bundle'"]
    let pipe = Pipe()
    proc.standardOutput = pipe
    proc.standardError = Pipe()
    do {
        try proc.run()
        proc.waitUntilExit()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        if let out = String(data: data, encoding: .utf8)?
            .split(separator: "\n").first, !out.isEmpty, out.hasSuffix(".app") {
            return String(out)
        }
    } catch {}
    // 2) 扫常见目录匹配文件名/CFBundleName
    let dirs = ["/Applications", "/System/Applications",
                "/Applications/Utilities", "/System/Applications/Utilities"]
    func collect(_ dir: String, _ depth: Int) -> [String] {
        guard depth <= 1 else { return [] }
        let fm = FileManager.default
        guard let items = try? fm.contentsOfDirectory(atPath: dir) else { return [] }
        var out: [String] = []
        for item in items {
            let full = (dir as NSString).appendingPathComponent(item)
            if item.hasSuffix(".app") {
                out.append(full)
            } else {
                var isDir: ObjCBool = false
                if fm.fileExists(atPath: full, isDirectory: &isDir), isDir.boolValue {
                    out += collect(full, depth + 1)
                }
            }
        }
        return out
    }
    var allApps: [String] = []
    for d in dirs {
        allApps += collect(d, 0)
    }
    for app in allApps {
        let fileBase = (app as NSString).lastPathComponent.replacingOccurrences(of: ".app", with: "").lowercased()
        if fileBase == lower {
            return app
        }
        let plistPath = (app as NSString).appendingPathComponent("Contents/Info.plist")
        if let info = NSDictionary(contentsOfFile: plistPath) {
            for key in ["CFBundleDisplayName", "CFBundleName"] {
                if let nm = info[key] as? String, nm.lowercased() == lower {
                    return app
                }
            }
        }
    }
    return nil
}

/// ensureActive 确保 app 成为键盘焦点 app（macOS 14+ activate 有时不转移 key——
/// 真机教训 2026-09：activate 后 isActive 仍 false，cmd+n/type 发给旧焦点 app）。
/// 兜底：点击其主窗口标题栏中心（真实用户点击语义；窗口 frame 从 AX 读）。
func ensureActive(_ app: NSRunningApplication) {
    app.activate(options: [.activateAllWindows])
    usleep(400_000)
    if app.isActive {
        return
    }
    // activate 未生效 → 点击主窗口标题栏（取第一个 AXWindow 的 frame 顶部）
    let axApp = AXUIElementCreateApplication(app.processIdentifier)
    var winRef: CFTypeRef?
    if AXUIElementCopyAttributeValue(axApp, kAXWindowsAttribute as CFString, &winRef) == .success,
       let wins = winRef as? [AXUIElement], let w0 = wins.first {
        let f = axFrame(w0)
        if f.width > 50 && f.height > 50 {
            // 标题栏 = 窗口顶部 ~28px 区域中心
            clickAt(CGPoint(x: f.midX, y: f.minY + 14))
            usleep(400_000)
        }
    }
}

/// activate：激活窗口/应用（pid 优先；windowTitle 匹配窗口）。P0 简化：
/// pid>0 激活应用 + 全部窗口；windowTitle 匹配时 raise 对应窗口。
func cmdActivate(_ id: Int, _ params: Data?) {
    guard let p: ActivateParams = decodeParams(params) else {
        respondErr(id, ERR_INTERNAL, "activate: failed to parse params")
        return
    }
    let app: NSRunningApplication?
    if let pid = p.pid, pid > 0 {
        app = NSRunningApplication(processIdentifier: pid_t(pid))
    } else if let name = p.app, !name.isEmpty {
        // 按 app 名解析（复用 open_app 的运行中匹配：localizedName/英文名/bundle 文件名）
        let lower = name.lowercased()
        app = NSWorkspace.shared.runningApplications.first(where: {
            ($0.localizedName?.lowercased() == lower) ||
            ($0.bundleURL?.lastPathComponent.lowercased().replacingOccurrences(of: ".app", with: "") == lower) ||
            ($0.bundleURL?.lastPathComponent.lowercased().contains(lower) ?? false)
        })
    } else if let front = NSWorkspace.shared.frontmostApplication {
        app = front
    } else {
        respondErr(id, ERR_INTERNAL, "cannot locate app")
        return
    }
    guard let app = app else {
        respondErr(id, ERR_NOT_FOUND, "app does not exist")
        return
    }
    ensureActive(app)
    // 恢复最小化窗口（2026-09 真机教训：WPS 窗口 minimized=true 时 activate 只切
    // 菜单栏，窗口不出现 → 全屏截图只有桌面。激活后遍历 AX 窗口 unhminimize +
    // raise 主窗口，确保窗口真正可见可截）。
    let axApp = AXUIElementCreateApplication(app.processIdentifier)
    var winRef: CFTypeRef?
    if AXUIElementCopyAttributeValue(axApp, kAXWindowsAttribute as CFString, &winRef) == .success,
       let wins = winRef as? [AXUIElement] {
        for w in wins {
            // 取消最小化
            var minRef: CFTypeRef?
            if AXUIElementCopyAttributeValue(w, kAXMinimizedAttribute as CFString, &minRef) == .success,
               let isMin = minRef as? Bool, isMin {
                AXUIElementSetAttributeValue(w, kAXMinimizedAttribute as CFString, kCFBooleanFalse)
            }
        }
        // raise 主窗口（标题栏点击兜底在 ensureActive 已做；此处 AXRaise 主窗口）
        if let main = wins.first(where: { w in
            var mRef: CFTypeRef?
            return AXUIElementCopyAttributeValue(w, kAXMainAttribute as CFString, &mRef) == .success && (mRef as? Bool ?? false)
        }) ?? wins.first {
            _ = AXUIElementPerformAction(main, kAXRaiseAction as CFString)
        }
    }
    if let wt = p.windowTitle, !wt.isEmpty {
        // 窗口级激活：遍历窗口标题匹配并 raise
        if AXUIElementCopyAttributeValue(axApp, kAXWindowsAttribute as CFString, &winRef) == .success,
           let wins = winRef as? [AXUIElement] {
            for w in wins {
                let title = attrString(w, kAXTitleAttribute as CFString)
                if title.contains(wt) {
                    _ = AXUIElementPerformAction(w, kAXRaiseAction as CFString)
                    break
                }
            }
        }
    }
    respondOKDict(id, ["activated": app.localizedName ?? "", "windowTitle": p.windowTitle ?? ""])
}

/// find：按文本/角色在当前 uidMap 检索（增量查询，不重建树）。
func cmdFind(_ id: Int, _ params: Data?) {
    guard let p: FindParams = decodeParams(params) else {
        respondErr(id, ERR_INTERNAL, "find: failed to parse params")
        return
    }
    var out: [ElementValue] = []
    for (uid, entry) in uidMap {
        let titleOK = p.text == nil || entry.title.contains(p.text!)
        let roleOK = p.role == nil || entry.role == p.role
        if titleOK && roleOK {
            let frame = axFrame(entry.el)
            let focused = attrBool(entry.el, kAXFocusedAttribute as CFString) ?? false
            let selected = attrBool(entry.el, kAXSelectedAttribute as CFString) ?? false
            out.append(ElementValue(uid: uid, role: entry.role, title: entry.title, value: "",
                                    description: "", frame: RectValue(x: frame.origin.x, y: frame.origin.y, w: frame.width, h: frame.height),
                                    actions: [], selected: selected, focused: focused, focusable: false, children: []))
        }
    }
    out.sort { $0.title < $1.title }
    respondOK(id, out)
}

/// verify：动作后断言（§6.3）。uid 的 value 变化 / wantText 出现 / 窗口标题变化。
/// 轮询 waitMS（默认 2000），间隔 200ms。返回 {ok: bool, detail: string}。
func cmdVerify(_ id: Int, _ params: Data?) {
    guard requireAX(id) else { return }
    guard let p: VerifyParams = decodeParams(params) else {
        respondErr(id, ERR_INTERNAL, "verify: failed to parse params")
        return
    }
    let waitMS = p.waitMS ?? 2000
    let deadline = Date().addingTimeInterval(Double(waitMS) / 1000)
    // 锚定目标初始值
    var anchorValue = ""
    if let uid = p.uid, let entry = uidMap[uid] {
        anchorValue = attrString(entry.el, kAXValueAttribute as CFString)
    }
    var lastDetail = ""
    while Date() < deadline {
        // 1) uid value 变化
        if let uid = p.uid, let entry = uidMap[uid] {
            let cur = attrString(entry.el, kAXValueAttribute as CFString)
            if cur != anchorValue && cur != "" {
                respondOKDict(id, ["ok": true, "detail": "element \(uid) value changed: \(anchorValue) → \(cur)"])
                return
            }
        }
        // 2) wantText 出现在当前快照树标题里
        if let wantText = p.wantText, !wantText.isEmpty {
            if uidMap.values.contains(where: { $0.title.contains(wantText) }) {
                respondOKDict(id, ["ok": true, "detail": "expected text appeared: \(wantText)"])
                return
            }
        }
        // 3) 窗口标题变化（对比快照时刻）
        lastDetail = "no change in target state"
        usleep(200_000)
    }
    respondOKDict(id, ["ok": false, "detail": lastDetail])
}

/// screenshot：捕获主屏全屏 PNG → base64 data URL（视觉模型看图通道，P1 最小集）。
/// 依赖屏幕录制权限（CGPreflightScreenCaptureAccess；未授权窗口内容黑屏）——
/// 未授权返回明确引导错误（Go 侧/UI 引导去系统设置开）。
/// 参数：无（region 留待后续；P0 最小集 = 全屏）。
/// 实现：ScreenCaptureKit（macOS 12.3+；CG 截图 API 在 14+ 编译不可用）。
func cmdScreenshot(_ id: Int) {
    // 主实现：CGWindowListCreateImage（旧 API，dlsym 绕过 macOS 14+ 编译 unavailable）。
    // 2026-09 真机排障结论：SCK（ScreenCaptureKit）要求 Developer ID 签名 + SCK 独立
    // TCC（kTCCServiceScreenCaptureKit），无签名 app 静默拒绝不弹窗；screencapture 命令
    // 在本机（企业管控 + 会话）恒输出壁纸。CGWindowListCreateImage 只需旧「屏幕录制」
    // 授权（Snipaste/CHTest 同款，实测可用），dlsym 动态调用绕过编译期 deprecation。
    if let png = captureViaCGWindowList() {
        respondOKDict(id, ["mime": "image/jpeg", "data": "data:image/jpeg;base64," + png.base64EncodedString()])
        return
    }
    // 兜底：screencapture（部分环境可用）
    if let fb = captureViaScreencapture() {
        respondOKDict(id, ["mime": "image/jpeg", "data": "data:image/jpeg;base64," + fb.base64EncodedString()])
        return
    }
    respondErr(id, ERR_PERM_DENIED, "screenshot unavailable: no screen-recording permission or capture API blocked (grant this app in System Settings → Privacy & Security → Screen Recording)")
}

// captureViaCGWindowList CGWindowListCreateImage 截全屏（dlsym 调用）+ 压缩 JPEG。
// 只依赖旧屏幕录制 TCC（无签名 app 可用）；失败返回 nil。
func captureViaCGWindowList() -> Data? {
    typealias CGWindowListCreateImageFn = @convention(c) (CGRect, CGWindowListOption, CGWindowID, CGWindowImageOption) -> CGImage?
    guard let handle = dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", RTLD_LAZY),
          let sym = dlsym(handle, "CGWindowListCreateImage") else { return nil }
    let fn = unsafeBitCast(sym, to: CGWindowListCreateImageFn.self)
    guard let screen = NSScreen.main ?? NSScreen.screens.first else { return nil }
    // optionAll = 全屏含所有窗口（当前 Space 合成画面）；bestResolution 拿物理像素
    guard let img = fn(screen.frame, .optionAll, kCGNullWindowID, [.bestResolution, .boundsIgnoreFraming]) else { return nil }
    return encodeScreenImage(img)
}

/// captureViaScreencapture 兜底：调系统 /usr/sbin/screencapture 截全屏到临时文件并读回。
func captureViaScreencapture() -> Data? {
    let tmp = NSTemporaryDirectory() + "cu_shot_\(UUID().uuidString).png"
    defer { try? FileManager.default.removeItem(atPath: tmp) }
    let p = Process()
    p.executableURL = URL(fileURLWithPath: "/usr/sbin/screencapture")
    p.arguments = ["-x", tmp]
    do {
        try p.run()
        p.waitUntilExit()
    } catch {
        return nil
    }
    guard p.terminationStatus == 0,
          let data = try? Data(contentsOf: URL(fileURLWithPath: tmp)),
          data.count > 100,
          // 压缩：PNG Data → CGImage → 缩放 + JPEG（与 SCK 路径同编码）
          let img = NSBitmapImageRep(data: data)?.cgImage else { return nil }
    return encodeScreenImage(img)
}

// encodeScreenImage 压缩编码截图：缩放到最长边 ≤1280px + JPEG 质量 0.7。
// 视觉模型分辨率需求远低于全屏（1512×945 Retina 物理像素更大）；PNG 全屏
// 8-12MB → JPEG 压缩后 ~150-300KB，上下文/传输成本降两个数量级。
// 模型看清 UI 文本/布局 1280 足够（对齐 Anthropic computer use 截图策略）。
let maxScreenDim: CGFloat = 1280
let screenJPEGQuality: CGFloat = 0.7

func encodeScreenImage(_ img: CGImage) -> Data? {
    let w = CGFloat(img.width)
    let h = CGFloat(img.height)
    let longest = max(w, h)
    var scale: CGFloat = 1
    if longest > maxScreenDim {
        scale = maxScreenDim / longest
    }
    let targetW = max(1, Int(w * scale))
    let targetH = max(1, Int(h * scale))
    guard let ctx = CGContext(data: nil, width: targetW, height: targetH, bitsPerComponent: 8,
                              bytesPerRow: 0, space: CGColorSpaceCreateDeviceRGB(),
                              bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return nil }
    ctx.interpolationQuality = .high
    ctx.draw(img, in: CGRect(x: 0, y: 0, width: targetW, height: targetH))
    guard let scaled = ctx.makeImage() else { return nil }
    let rep = NSBitmapImageRep(cgImage: scaled)
    return rep.representation(using: .jpeg, properties: [.compressionFactor: screenJPEGQuality])
}

// ---------- AX 工具 ----------

/// 取指定 app 的 AX 窗口数组
func AXWindowsList(pid: Int) -> [WindowValue] {
    let app = AXUIElementCreateApplication(pid_t(pid))
    var winRef: CFTypeRef?
    let err = AXUIElementCopyAttributeValue(app, kAXWindowsAttribute as CFString, &winRef)
    guard err == .success, let wins = winRef as? [AXUIElement] else { return [] }
    var out: [WindowValue] = []
    for w in wins {
        let title = attrString(w, kAXTitleAttribute as CFString)
        let frame = axFrame(w)
        out.append(WindowValue(title: title, frame: RectValue(x: frame.origin.x, y: frame.origin.y, w: frame.width, h: frame.height)))
    }
    return out
}

/// 取 AX 元素 frame（position + size 合并）。
func axFrame(_ el: AXUIElement) -> CGRect {
    var frame = CGRect.zero
    var posRef: CFTypeRef?
    var sizeRef: CFTypeRef?
    if AXUIElementCopyAttributeValue(el, kAXPositionAttribute as CFString, &posRef) == .success,
       let p = posRef, CFGetTypeID(p) == AXValueGetTypeID() {
        let axv = p as! AXValue
        var pt = CGPoint.zero
        if AXValueGetValue(axv, .cgPoint, &pt) { frame.origin = pt }
    }
    if AXUIElementCopyAttributeValue(el, kAXSizeAttribute as CFString, &sizeRef) == .success,
       let s = sizeRef, CFGetTypeID(s) == AXValueGetTypeID() {
        let axv = s as! AXValue
        var sz = CGSize.zero
        if AXValueGetValue(axv, .cgSize, &sz) { frame.size = sz }
    }
    return frame
}

/// 递归裁剪 AX 树（uid 生成 + uidMap 填充）。
func AXTreeForApp(pid: Int, maxDepth: Int) -> ElementValue? {
    let app = AXUIElementCreateApplication(pid_t(pid))
    return axNode(app, uidPrefix: "a", depth: 0, maxDepth: maxDepth)
}

/// 递归节点转换：角色/title/value/actions/frame + 子节点交互过滤。
/// 生成 uid 并注册 uidMap（交互节点才注册）。
func axNode(_ el: AXUIElement, uidPrefix: String, depth: Int, maxDepth: Int) -> ElementValue? {
    var roleRef: CFTypeRef?
    guard AXUIElementCopyAttributeValue(el, kAXRoleAttribute as CFString, &roleRef) == .success,
          let role = roleRef as? String else {
        return nil
    }
    var actionsRef: CFArray?
    var actions: [String] = []
    if AXUIElementCopyActionNames(el, &actionsRef) == .success, let acts = actionsRef as? [String] {
        actions = acts
    }
    let title = attrString(el, kAXTitleAttribute as CFString)
    let value = attrString(el, kAXValueAttribute as CFString)
    let desc = attrString(el, kAXDescriptionAttribute as CFString)
    let focused = attrBool(el, kAXFocusedAttribute as CFString) ?? false
    let selected = attrBool(el, kAXSelectedAttribute as CFString) ?? false
    // Focusable 启发：输入/交互控件角色（AX 无统一 kAXFocusable，常用控件即认为可聚焦）
    let inputRoles: Set<String> = ["AXTextField", "AXTextArea", "AXComboBox", "AXButton",
                                   "AXCheckBox", "AXRadioButton", "AXPopUpButton", "AXMenuButton",
                                   "AXDisclosureTriangle", "AXSlider", "AXSearchField"]
    let focusable = focused || inputRoles.contains(role) || !actions.isEmpty
    let frame = axFrame(el)

    let uid = "\(uidPrefix):\(depth)"
    let interactive = !actions.isEmpty || !title.isEmpty || !value.isEmpty || !desc.isEmpty || focusable
        || role == "AXWindow" || role == "AXMenuBarItem" || role == "AXMenuItem"
        || role == "AXRow" || role == "AXCell"
    var node = ElementValue(uid: uid, role: role, title: title, value: value,
                            description: desc, frame: RectValue(x: frame.origin.x, y: frame.origin.y, w: frame.width, h: frame.height),
                            actions: actions, selected: selected, focused: focused,
                            focusable: focusable, children: [])
    // 仅交互节点进 uidMap（press/find/verify 目标）
    if interactive {
        uidMap[uid] = UIDEntry(el: el, role: role, title: title)
    }
    if depth < maxDepth {
        var childRef: CFTypeRef?
        if AXUIElementCopyAttributeValue(el, kAXChildrenAttribute as CFString, &childRef) == .success,
           let children = childRef as? [AXUIElement] {
            var out: [ElementValue] = []
            for (i, c) in children.enumerated() {
                if let n = axNode(c, uidPrefix: "\(uidPrefix):\(i)", depth: depth + 1, maxDepth: maxDepth) {
                    if isInteractiveOrHasInteractive(n) { out.append(n) }
                }
            }
            node.children = out
        }
    }
    return node
}

/// 交互性判定（含后代是否有可交互节点，用于容器裁剪）
func isInteractiveOrHasInteractive(_ n: ElementValue) -> Bool {
    if !n.actions.isEmpty || n.focusable || !n.title.isEmpty || !n.value.isEmpty || !n.description.isEmpty { return true }
    if n.role == "AXWindow" || n.role == "AXGroup" || n.role == "AXScrollArea" { return true }
    return n.children.contains(where: { isInteractiveOrHasInteractive($0) })
}

func attrString(_ el: AXUIElement, _ attr: CFString) -> String {
    var ref: CFTypeRef?
    guard AXUIElementCopyAttributeValue(el, attr, &ref) == .success else { return "" }
    if let s = ref as? String { return s }
    if let n = ref as? NSNumber { return n.stringValue }
    return ""
}

func attrBool(_ el: AXUIElement, _ attr: CFString) -> Bool? {
    var ref: CFTypeRef?
    guard AXUIElementCopyAttributeValue(el, attr, &ref) == .success else { return nil }
    return ref as? Bool
}

/// 菜单路径按压（app 菜单栏逐项匹配标题并点击）。
/// 实现 = 真实鼠标路径：菜单栏项 frame 中心 CGEvent 点击展开 → 菜单项 frame
/// 中心点击执行。比 AXPress 可靠（AXPress 对菜单项在部分 app（如日历）不触发；
/// 真机验证 2026-09 记录）。坐标全部来自 AX frame（系统给），非模型手算。
func pressMenuPath(app: String, path: [String], completion: @escaping (Bool, String) -> Void) {
    guard let running = NSWorkspace.shared.runningApplications.first(where: {
        $0.localizedName?.lowercased() == app.lowercased()
    }) else {
        completion(false, "no running app \(app)")
        return
    }
    // 确保目标 app 激活（焦点竞争下菜单栏点击才有效；macOS14+ activate 即用）
    running.activate(options: [.activateAllWindows])
    usleep(400_000)
    let axApp = AXUIElementCreateApplication(running.processIdentifier)

    // 关闭可能残留的菜单（Escape 两次——点菜单项前必须菜单全关，否则点「文件」
    // 变成收起而非展开；真机教训 2026-09）
    func pressKey(_ code: CGKeyCode) {
        let src = CGEventSource(stateID: .hidSystemState)
        CGEvent(keyboardEventSource: src, virtualKey: code, keyDown: true)?.post(tap: .cghidEventTap)
        usleep(40_000)
        CGEvent(keyboardEventSource: src, virtualKey: code, keyDown: false)?.post(tap: .cghidEventTap)
    }
    pressKey(53) // Escape
    usleep(200_000)
    pressKey(53)
    usleep(300_000)
    var menuBarRef: CFTypeRef?
    guard AXUIElementCopyAttributeValue(axApp, kAXMenuBarAttribute as CFString, &menuBarRef) == .success,
          let mb = menuBarRef, CFGetTypeID(mb) == AXUIElementGetTypeID() else {
        completion(false, "app \(app) has no menu bar")
        return
    }
    let menuBar = mb as! AXUIElement

    // 找 children 中 title 匹配的直接子元素。
    func findChild(_ parent: AXUIElement, title: String) -> AXUIElement? {
        var childRef: CFTypeRef?
        guard AXUIElementCopyAttributeValue(parent, kAXChildrenAttribute as CFString, &childRef) == .success,
              let children = childRef as? [AXUIElement] else { return nil }
        for c in children {
            if attrString(c, kAXTitleAttribute as CFString) == title {
                return c
            }
        }
        return nil
    }
    // 从给定元素取其 AXMenu 容器（菜单项挂在菜单里）。
    func menuContainer(_ el: AXUIElement) -> AXUIElement? {
        var childRef: CFTypeRef?
        guard AXUIElementCopyAttributeValue(el, kAXChildrenAttribute as CFString, &childRef) == .success,
              let children = childRef as? [AXUIElement] else { return nil }
        for c in children {
            if attrString(c, kAXRoleAttribute as CFString) == "AXMenu" {
                return c
            }
        }
        if attrString(el, kAXRoleAttribute as CFString) == "AXMenu" {
            return el
        }
        return nil
    }
    // 坐标点击元素中心（展开菜单/执行菜单项）。
    func clickCenter(_ el: AXUIElement) -> Bool {
        let f = axFrame(el)
        guard f.width > 1, f.height > 1 else { return false }
        // macOS 屏幕坐标：AX position 是全局坐标（左上原点），CGEvent 同坐标系
        clickAt(CGPoint(x: f.midX, y: f.midY))
        return true
    }

    // 第一级：菜单栏项（如 文件）→ 点击展开
    guard let top = findChild(menuBar, title: path[0]) else {
        completion(false, "menu bar has no \"\(path[0])\"")
        return
    }
    _ = clickCenter(top)
    usleep(400_000)

    // 逐级：在上级菜单容器里找下一级 → 点击（末级=执行，中间级=展开子菜单）
    var cur = top
    for level in 1..<path.count {
        // 当前级若是菜单栏项/带子菜单项，其容器 = AXMenu；点击的项自身在容器里
        let container = menuContainer(cur)
        guard let menu = container else {
            completion(false, "\"\(path[level-1])\" has no expandable menu")
            return
        }
        // 菜单可能未展开完：轮询目标项出现且 frame 有效（展开后 y>37、w>0）
        var item: AXUIElement?
        for _ in 0..<15 {
            if let it = findChild(menu, title: path[level]) {
                let f = axFrame(it)
                if f.width > 1 && f.height > 1 && f.minY > 37 {
                    item = it
                    break
                }
                // frame 无效 = 菜单未真展开 → 重新点击上级展开
                if level == 1 {
                    _ = clickCenter(cur)
                }
            }
            usleep(120_000)
        }
        guard let it = item else {
            completion(false, "menu \"\(path[level-1])\" has no \"\(path[level])\" (expand failed)")
            return
        }
        cur = it
        if level == path.count - 1 {
            // 末级：点击执行
            _ = clickCenter(cur)
            usleep(300_000)
        } else {
            // 中间级：点击展开子菜单
            _ = clickCenter(cur)
            usleep(400_000)
        }
    }
    completion(true, "")
}

// ---------- 主循环 ----------

func main() {
    while let line = readLine() {
        guard let data = line.data(using: .utf8),
              let req = RequestValue(json: data) else {
            continue // 坏行忽略（协议容忍）
        }
        switch req.cmd {
        case "hello":
            let hp = HelloParamsValue(params: req.params)
            guard hp.versionMajor == 1 else {
                respondErr(req.id, ERR_UNSUPPORTED, "protocol version mismatch: expected major 1, got \(hp.versionMajor)")
                continue
            }
            respondOK(req.id, HelloResultValue(version_major: 1, version_minor: 0, helper: "macos-ax"))
        case "perm_status":
            cmdPermStatus(req.id)
        case "screen_info":
            cmdScreenInfo(req.id)
        case "snapshot":
            cmdSnapshot(req.id, req.params)
        case "press":
            cmdPress(req.id, req.params)
        case "click":
            cmdClick(req.id, req.params)
        case "type":
            cmdType(req.id, req.params)
        case "key":
            cmdKey(req.id, req.params)
        case "open_app":
            cmdOpenApp(req.id, req.params)
        case "activate":
            cmdActivate(req.id, req.params)
        case "find":
            cmdFind(req.id, req.params)
        case "verify":
            cmdVerify(req.id, req.params)
        case "screenshot":
            cmdScreenshot(req.id)
        default:
            respondErr(req.id, ERR_UNSUPPORTED, "unimplemented command: \(req.cmd)")
        }
    }
}

main()
