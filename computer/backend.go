package computer

// Backend 平台契约。全部方法返回领域模型（model.go）；实现负责
// 平台调用 + 权限状态 + TCC 归属。nil/错误 = 结构化平台错误
// （权限缺失/目标失稳/超时），Executor 转成模型可见的纠错文本。
//
// 依赖方向：computer 内核 ← desktop/bridge（注册/装配）← desktop/app（UI）。
// 内核不 import 任何平台/桥接代码；协议 proto.go 是 Backend 的 stdio
// 实例化（macOS adapter 用它），fake 单测直接实现本接口（COMPUTER_USE_DESIGN.md §3.5）。
type Backend interface {
	ScreenInfo() (ScreenInfo, error) // 尺寸/scaleFactor/显示器列表
	Snapshot(opts SnapshotOpts) (*Snapshot, error)
	Find(query FindQuery) ([]Element, error)   // 增量目标检索
	Press(uid string) error                    // 语义按压（AXPress/键盘可达）
	PressMenu(app string, path []string) error // 菜单路径语义按压（菜单栏逐项 AXPress）
	Click(pt Point) error                      // 像素点击（坐标回退/看图工具族）
	TypeText(text string) error                // 键入当前焦点
	KeyChord(chord string) error               // 快捷键（cmd+shift+p / Return / Tab…）
	OpenApp(name string) error
	Activate(pid int, app, windowTitle string) error
	Verify(query VerifyQuery) (bool, string, error) // AX 轮询断言
	PermStatus() (PermStatus, error)
	Screenshot(region Rect, maxDim int) (*Image, error) // P1 看图工具族
	Close() error
}

// Image 截图（P1 看图工具族；数据为 PNG/JPEG 编码字节）。
type Image struct {
	Width  int    // 像素宽
	Height int    // 像素高
	Format string // "png" / "jpeg"
	Data   []byte // 编码后字节
}
