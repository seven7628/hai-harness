package computer

// 领域模型：全部字段带显式小驼峰 JSON 标签——跨语言协议契约
//（Swift/未来 helper 的 JSONEncoder 天然输出小驼峰，两边严格一致）。

// Point/Size/Rect 使用逻辑屏幕点（points；Retina 由 Backend 换算像素），
// 领域层不感知设备像素比。
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}
type Size struct {
	W float64 `json:"w"`
	H float64 `json:"h"`
}
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Element 快照树中的一个可交互节点（领域模型，无平台类型泄漏）。
type Element struct {
	UID         string     `json:"uid"`             // 稳定句柄（跨快照尽量稳定；失稳 = 重新快照）
	Role        string     `json:"role"`            // button / menuItem / textField / checkbox / ...
	Title       string     `json:"title,omitempty"` // 可读主文案
	Value       string     `json:"value,omitempty"` // 当前值（开关/输入框）
	Description string     `json:"description,omitempty"`
	Frame       Rect       `json:"frame"`               // 逻辑屏幕点；供 Executor 坐标回退/验证
	Actions     []string   `json:"actions,omitempty"`   // AX actions（press/…）
	Selected    bool       `json:"selected,omitempty"`  // AX selected（列表项/选项卡选中态）
	Focused     bool       `json:"focused,omitempty"`   // AX focused（当前键盘焦点）
	Focusable   bool       `json:"focusable,omitempty"` // 启发式：是否可聚焦控件
	Children    []*Element `json:"children,omitempty"`  // 已按可交互性过滤/裁剪
}

// Window 快照中的一个窗口。
type Window struct {
	Title string `json:"title"`
	Frame Rect   `json:"frame"` // 逻辑屏幕点
}

// GridCell L4 画布兜底网格的一个格子（符号级摘要）。
type GridCell struct {
	X    int    `json:"x"` // 网格坐标（如 3,5）
	Y    int    `json:"y"`
	Text string `json:"text,omitempty"` // 格内摘要
}

// Snapshot 一次屏幕状态快照（语义工具主数据源）。
type Snapshot struct {
	FrontmostApp string     `json:"frontmostApp"`
	Pid          int        `json:"pid"`
	Windows      []Window   `json:"windows,omitempty"`
	Tree         *Element   `json:"tree,omitempty"` // 前台 App 的（裁剪后）AX 树
	Grid         []GridCell `json:"grid,omitempty"` // L4：画布兜底的网格摘要（可选）
}

// ScreenInfo 屏幕信息（Backend.ScreenInfo 返回值）。
type ScreenInfo struct {
	Size        Size     `json:"size"`              // 主屏逻辑 points 尺寸
	ScaleFactor float64  `json:"scaleFactor"`       // Retina 换算（1.0/2.0…）
	Screens     []Screen `json:"screens,omitempty"` // 显示器列表
}

// Screen 单台显示器。
type Screen struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	Frame       Rect    `json:"frame"` // 逻辑 points（桌面坐标空间）
	ScaleFactor float64 `json:"scaleFactor"`
}

// PermStatus TCC/辅助功能权限状态（Backend.PermStatus 返回值）。
// 注意：macOS 辅助功能授权归属主体可能是 helper 自身或宿主 app（TCC 责任进程
// 规则，M0 spike 实测，§6.6）——各字段值由适配器如实上报，Executor 只读展示。
type PermStatus struct {
	Accessibility bool   `json:"accessibility"` // 辅助功能（AX）已授权
	ScreenCapture bool   `json:"screenCapture"` // 屏幕录制/捕获已授权
	TrustedApp    string `json:"trustedApp"`    // 授权归属主体描述（helper/app/未知）
}

// SnapshotOpts 控制一次快照的范围/深度（Executor 决定裁剪策略，Backend 执行）。
type SnapshotOpts struct {
	MaxChars int `json:"maxChars,omitempty"` // 文本树最大输出字符（≈6–8k）
	MaxDepth int `json:"maxDepth,omitempty"` // 折叠深度（>5 省略）
}

// FindQuery 增量目标检索条件（§6.1 超出快照预算时用 computer_find）。
type FindQuery struct {
	Text  string `json:"text,omitempty"`  // 匹配 title/value/description（子串）
	Role  string `json:"role,omitempty"`  // 限定角色（可空）
	App   string `json:"app,omitempty"`   // 限定应用（可空；默认前台）
	Index int    `json:"index,omitempty"` // 第几个命中（默认 0 = 第一个）
}

// VerifyQuery 动作后断言（§6.3）：AX 状态 / 窗口标题变化轮询。
type VerifyQuery struct {
	UID       string `json:"uid,omitempty"`       // 目标元素（可空：只看窗口变化）
	WantValue string `json:"wantValue,omitempty"` // 期望该元素 value（可空）
	WantText  string `json:"wantText,omitempty"`  // 期望树中出现文本（可空）
	WaitMS    int    `json:"waitMS,omitempty"`    // 轮询时长（默认 2000）
}
