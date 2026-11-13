// Package officekit LibreOffice kit 的探测、校验、转换与栅格化（R08d）。
//
// 定位（00-conventions §1.4）：本包**只做转换** —— 探测哪个 soffice 可用、跑一次
// `--convert-to pdf`、把 PDF 逐页栅格成 PNG。排队/超时口径/缓存/分发/UI 都在宿主
// （desktop/bridge）。kit 是**可选组件**：缺失时宿主继续走既有近似渲染路径，
// 本包只负责如实回「不可用」及其原因，绝不静默挑一个来路不明的二进制。
//
// 为什么不内置 LibreOffice：安装包 +400MB 量级（见 Platforms 的体积口径），
// 而绝大多数会话根本不开 Office 文件。分发（下载/签名/许可 NOTICE）是 PR-13，
// 本轮只有「探测 + 校验 + 说明」。
package officekit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable kit 不可用（未安装 / 校验失败 / 平台不支持）。
//
// 调用方据此走降级路径（近似渲染 + 横幅），**不是**致命错误：所有返回它的地方
// 都已把「为什么」写进 Status.Reason / Errors。
var ErrUnavailable = errors.New("officekit: LibreOffice kit 不可用")

// manifestName kit 自描述文件名（DSH 同款；PR-13 分发时由打包脚本写入）。
const manifestName = "KIT.json"

// PinnedVersion 本版 app 唯一接受的 kit 版本。
//
// 为什么钉死（而不是「有就用」）：kit 是随 app 一起被验证过的二进制 —— 换一个
// 版本意味着换了整套转换语义（字体、PDF 导出、许可声明），而我们只在钉住的那一版上
// 跑过验收。PR-13 分发时必须与本常量同步 bump，并同步更新期望的 sha256。
const PinnedVersion = "24.8.7-1"

// DefaultTimeout 单次转换时限（含冷启）。与 conversion.DefaultTimeout 同口径：
// 60s 是「明显不正常就报错」的阈值，不是性能目标。
const DefaultTimeout = 60 * time.Second

// manifestName 之外的 kit 目录约定：<root>/<version>/<platform>/{soffice,KIT.json}。

// Manifest kit 自描述（KIT.json）。
//
// sha256 是**对二进制本体**的，不是对整棵目录：目录里还有 resources/ 等（几十万文件时
// 逐个哈希要分钟级），而二进制才是「被执行的那一个」—— 校验要拦的正是它。
type Manifest struct {
	Version    string `json:"version"`    // kit 版本（必须 == PinnedVersion）
	Platform   string `json:"platform"`   // darwin-arm64 / darwin-x64 / windows-x64 / linux-x64
	SHA256     string `json:"sha256"`     // soffice 二进制的 sha256（小写十六进制）
	SizeBytes  int64  `json:"sizeBytes"`  // 二进制字节数（>0 时先比大小，省一次全量哈希）
	BuiltAt    string `json:"builtAt"`    // 构建时间（诊断）
	License    string `json:"license"`    // 许可（MPL-2.0 / LGPL-3.0 混合）
	LicenseURL string `json:"licenseUrl"` // 许可全文
	SourceURL  string `json:"sourceUrl"`  // 源码获取地址（MPL/LGPL 合规要求）
}

// PlatformInfo 平台矩阵的一行：这个平台怎么供 kit、多大、什么许可。
//
// 为什么需要它（而不是散在前端文案里）：设置页要展示「本机需要装什么、多大、许可如何」，
// 这张表是唯一事实源；PR-13 的下载器也按 id 选产物。Linux 直接复用系统 soffice
// （发行版仓库里有，再分发一份没有收益）→ Supply="system"、Size=0。
type PlatformInfo struct {
	ID         string `json:"id"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Supply     string `json:"supply"` // "kit"（随 app 下载）| "system"（用系统装的）
	SizeBytes  int64  `json:"sizeBytes"`
	License    string `json:"license"`
	LicenseURL string `json:"licenseUrl"`
	SourceURL  string `json:"sourceUrl"`
	Note       string `json:"note,omitempty"`
}

// 许可与源码地址（三个平台同一份：LibreOffice 上游发行的就是同一套）。
const (
	licenseName = "MPL-2.0 / LGPL-3.0（混合）"
	licenseURL  = "https://www.libreoffice.org/download/license/"
	sourceURL   = "https://www.libreoffice.org/about-us/source-code/"
	// sizeNote 体积口径的边界：这些是分发**估算**，PR-13 以实际产物为准。
	sizeNote = "体积为分发估算（含 resources/），PR-13 以实际产物为准"
)

// Platforms 平台矩阵。体积按「裁剪后的运行时 + resources」估算。
var Platforms = []PlatformInfo{
	{ID: "darwin-arm64", OS: "darwin", Arch: "arm64", Supply: "kit", SizeBytes: 412 << 20, License: licenseName, LicenseURL: licenseURL, SourceURL: sourceURL, Note: sizeNote},
	{ID: "darwin-x64", OS: "darwin", Arch: "amd64", Supply: "kit", SizeBytes: 445 << 20, License: licenseName, LicenseURL: licenseURL, SourceURL: sourceURL, Note: sizeNote},
	{ID: "windows-x64", OS: "windows", Arch: "amd64", Supply: "kit", SizeBytes: 398 << 20, License: licenseName, LicenseURL: licenseURL, SourceURL: sourceURL, Note: sizeNote},
	{ID: "linux-x64", OS: "linux", Arch: "amd64", Supply: "system", SizeBytes: 0, License: licenseName, LicenseURL: licenseURL, SourceURL: sourceURL, Note: "复用系统 soffice（发行版仓库安装），不随 app 分发"},
}

// PlatformOf 返回某平台在矩阵里的那一行；未知平台返回 false。
func PlatformOf(id string) (PlatformInfo, bool) {
	for _, p := range Platforms {
		if p.ID == id {
			return p, true
		}
	}
	return PlatformInfo{}, false
}

// CurrentPlatform 本机平台 id（矩阵键）。
func CurrentPlatform() string { return platformID(runtime.GOOS, runtime.GOARCH) }

// platformID 平台键：GOOS-GOARCH（amd64 统一写作 x64，与矩阵/目录名一致）。
//
// 为什么把 amd64 改写成 x64：kit 目录名要给人看、也要和分发产物的文件名对齐，
// 而 Go 的 GOARCH 是三个平台里唯一会把 x86_64 叫成 amd64 的。
func platformID(goos, goarch string) string {
	if goarch == "amd64" {
		goarch = "x64"
	}
	return goos + "-" + goarch
}

// DefaultRoot kit 根目录：~/.go-code/office-kit。
//
// 与 bridge 的 appDataDir 同一约定（APP 级数据都在 ~/.go-code 下），但本包是 SDK 层，
// 不能 import desktop/bridge —— 自行解析 home（同 runtime.DefaultRoot 的理由）。
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".go-code", "office-kit")
}

// SystemPaths 系统 soffice 的固定安装位置（PATH 之外的兜底）。
//
// macOS 的 LibreOffice.app 不会把 soffice 放进 PATH（它是 app 内的辅助可执行文件），
// 所以「装了 LibreOffice 却探测不到」是这一类探测最典型的误报 —— 必须显式列出。
var SystemPaths = []string{"/Applications/LibreOffice.app/Contents/MacOS/soffice"}

// Source kit 的来源。
type Source string

const (
	SourceSystem Source = "system" // 系统/用户自装的 soffice（PATH 或 SystemPaths）
	SourceKit    Source = "kit"    // ~/.go-code/office-kit 下的受管 kit
)

// Kit 一个可用的 soffice。
type Kit struct {
	Bin      string   // 可执行文件绝对路径
	Source   Source   // system | kit
	Version  string   // kit 版本；系统 soffice 为 "system"
	Root     string   // kit 根目录（SourceSystem 时为空）
	Manifest Manifest // kit 自描述（SourceSystem 时为零值）
}

// State 探测结论（设置页/横幅直接展示）。
type State string

const (
	StateOK      State = "ok"      // 可用
	StateMissing State = "missing" // 没装（既没有系统 soffice，也没有 kit 目录）
	StateInvalid State = "invalid" // 装了但不接受（KIT.json/sha256/版本/平台任一不符）
)

// Status 一次探测的完整结论。**前端只消费这个**（不要让它自己拼路径判断）。
type Status struct {
	Available     bool           `json:"available"`
	State         State          `json:"state"`
	Source        string         `json:"source,omitempty"`
	Bin           string         `json:"bin,omitempty"`
	Root          string         `json:"root,omitempty"`
	Version       string         `json:"version,omitempty"`
	Platform      string         `json:"platform"`
	SizeBytes     int64          `json:"sizeBytes"` // kit 目录总大小（系统 soffice 时 0）
	License       string         `json:"license,omitempty"`
	LicenseURL    string         `json:"licenseUrl,omitempty"`
	SourceURL     string         `json:"sourceUrl,omitempty"`
	Reason        string         `json:"reason,omitempty"` // 不可用的人话原因
	Errors        []string       `json:"errors,omitempty"` // 校验失败逐条（诊断/设置页展开）
	Platforms     []PlatformInfo `json:"platforms"`        // 平台矩阵（设置页「如何安装」用）
	PinnedVersion string         `json:"pinnedVersion"`
	Kit           *Kit           `json:"-"` // 可用时的句柄（不进 JSON：前端不该拿它拼路径）
}

// ProbeOptions 探测输入（零值 = 生产口径）。
type ProbeOptions struct {
	// KitRoot 覆盖 kit 根（测试注入；空 = DefaultRoot()）。
	KitRoot string
	// RequiredVersion 覆盖钉住的 kit 版本（空 = PinnedVersion）。
	RequiredVersion string
	// LookPath 覆盖 PATH 查找（测试注入；nil = exec.LookPath）。
	LookPath func(string) (string, error)
	// SystemPaths 覆盖系统安装位置（测试注入；nil = SystemPaths）。
	SystemPaths []string
	// Platform 覆盖平台键（测试注入；空 = CurrentPlatform()）。
	Platform string
	// SkipSize 跳过目录体积统计（慢；转换热路径不需要它）。
	SkipSize bool
}

// Probe 一次探测：系统 soffice 优先，其次受管 kit。
//
// 顺序为什么不能反：用户自己装了 LibreOffice 时，那是**他**的环境（更新由他控制），
// 用 kit 覆盖等于把「我装的版本」偷偷换掉；且系统版通常更全（带 Java 宏支持）。
func Probe(opts ProbeOptions) Status {
	platform := opts.Platform
	if platform == "" {
		platform = CurrentPlatform()
	}
	st := Status{State: StateMissing, Platform: platform, Platforms: Platforms, PinnedVersion: requiredVersion(opts)}

	// ① 系统 soffice：PATH → SystemPaths。
	if bin, ok := findSystem(platform, opts); ok {
		st.Available = true
		st.State = StateOK
		st.Source = string(SourceSystem)
		st.Bin = bin
		st.Version = "system"
		st.fillLicense(platform)
		st.Kit = &Kit{Bin: bin, Source: SourceSystem, Version: "system"}
		return st
	}

	// ② 受管 kit：<root>/<version>/<platform>/{soffice,KIT.json}。
	root := opts.KitRoot
	if root == "" {
		root = DefaultRoot()
	}
	st.Root = root
	kit, errs, reason := probeKit(root, platform, requiredVersion(opts))
	if kit != nil {
		st.Available = true
		st.State = StateOK
		st.Source = string(SourceKit)
		st.Bin = kit.Bin
		st.Version = kit.Version
		st.License, st.LicenseURL, st.SourceURL = kit.Manifest.License, kit.Manifest.LicenseURL, kit.Manifest.SourceURL
		if st.License == "" {
			st.fillLicense(platform)
		}
		if !opts.SkipSize {
			st.SizeBytes = dirSize(root)
		}
		st.Kit = kit
		return st
	}
	if len(errs) > 0 {
		st.State = StateInvalid
		st.Errors = errs
	}
	st.Reason = reason
	return st
}

// requiredVersion 生效的钉住版本。
func requiredVersion(opts ProbeOptions) string {
	if opts.RequiredVersion != "" {
		return opts.RequiredVersion
	}
	return PinnedVersion
}

// fillLicense 平台矩阵里的许可口径（系统 soffice 没有 KIT.json 可读）。
func (s *Status) fillLicense(platform string) {
	if p, ok := PlatformOf(platform); ok {
		s.License, s.LicenseURL, s.SourceURL = p.License, p.LicenseURL, p.SourceURL
		return
	}
	s.License, s.LicenseURL, s.SourceURL = licenseName, licenseURL, sourceURL
}

// findSystem 系统 soffice：PATH 优先，其次固定安装位置。
func findSystem(platform string, opts ProbeOptions) (string, bool) {
	look := opts.LookPath
	if look == nil {
		look = exec.LookPath
	}
	if p, err := look("soffice"); err == nil && isExecutable(p) {
		return p, true
	}
	paths := opts.SystemPaths
	if paths == nil {
		// 只有 macOS 有「装在 app 包里、不在 PATH」这种形态；其它平台靠包管理器
		// 装进 PATH，列固定位置反而会误导。
		if strings.HasPrefix(platform, "darwin") {
			paths = SystemPaths
		}
	}
	for _, p := range paths {
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}

// isExecutable 是不是可执行普通文件。
func isExecutable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// probeKit 在 `<root>/<version>/<platform>/` 里找 kit。
//
// 「钉住的版本目录存在就只认它」是刻意的：如果 <pinned> 目录在但校验不过，
// 而 <other> 目录能过，回退到 other 就是**静默换版本** —— 用户装的是坏的 24.8.7，
// 我们却用 24.2 跑他的合同，还告诉他「一切正常」。故只认钉住的那一个。
func probeKit(root, platform, wantVersion string) (*Kit, []string, string) {
	if root == "" {
		return nil, nil, "无法解析用户主目录，kit 路径未知"
	}
	dir := filepath.Join(root, wantVersion, platform)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, "未安装 LibreOffice kit（" + root + " 不存在）"
	}
	if err != nil {
		return nil, nil, "读取 kit 目录失败: " + err.Error()
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		// 只有别的版本 → 明确说是版本不符（不静默用）。
		var others []string
		for _, e := range entries {
			if e.IsDir() && e.Name() != wantVersion {
				others = append(others, e.Name())
			}
		}
		if len(others) > 0 {
			sort.Strings(others)
			return nil, []string{fmt.Sprintf("只找到 kit 版本 %s，本版 app 只接受 %s", strings.Join(others, ", "), wantVersion)},
				"office kit 版本不匹配（需要 " + wantVersion + "）"
		}
		return nil, nil, "未安装 LibreOffice kit（" + dir + " 不存在）"
	}

	kit, errs, reason := validateKitDir(dir, platform, wantVersion)
	if kit == nil {
		return nil, errs, reason
	}
	kit.Root = root
	return kit, nil, ""
}

// validateKitDir 校验一个 kit 目录（KIT.json → 平台 → 版本 → 二进制 → sha256）。
//
// 顺序即成本顺序：先读几 KB 的 JSON，再比二进制大小，最后才做全量哈希 ——
// 一个 400MB 的坏 kit 不该每次启动都白算一遍 sha256。
func validateKitDir(dir, platform, wantVersion string) (*Kit, []string, string) {
	var errs []string
	fail := func(reason string) (*Kit, []string, string) { return nil, errs, reason }

	m, err := readManifest(filepath.Join(dir, manifestName))
	if err != nil {
		errs = append(errs, err.Error())
		return fail("kit 缺少可用的 " + manifestName + "（校验不过的 kit 一律不用）")
	}
	if m.Platform != platform {
		errs = append(errs, fmt.Sprintf("KIT.json 平台 = %q，本机 = %q", m.Platform, platform))
	}
	if m.Version != wantVersion {
		errs = append(errs, fmt.Sprintf("KIT.json 版本 = %q，需要 %q", m.Version, wantVersion))
	}
	if m.SHA256 == "" {
		errs = append(errs, "KIT.json 缺 sha256")
	}
	bin := filepath.Join(dir, "soffice")
	info, statErr := os.Stat(bin)
	switch {
	case statErr != nil:
		errs = append(errs, "缺少可执行文件 "+bin)
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0:
		errs = append(errs, bin+" 不是可执行文件")
	case m.SizeBytes > 0 && info.Size() != m.SizeBytes:
		errs = append(errs, fmt.Sprintf("二进制大小 %d ≠ KIT.json %d", info.Size(), m.SizeBytes))
	case m.SHA256 != "":
		got, herr := fileSHA256(bin)
		if herr != nil {
			errs = append(errs, "计算 sha256 失败: "+herr.Error())
		} else if !strings.EqualFold(got, m.SHA256) {
			errs = append(errs, fmt.Sprintf("二进制 sha256 = %s，KIT.json = %s", got, m.SHA256))
		}
	}
	if len(errs) > 0 {
		return fail("kit 校验失败（" + errs[0] + "）")
	}
	return &Kit{Bin: bin, Source: SourceKit, Version: m.Version, Manifest: m}, nil, ""
}

// readManifest 读并解析 KIT.json。
func readManifest(path string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, fmt.Errorf("%s 不存在: %s", manifestName, path)
	}
	if err != nil {
		return m, fmt.Errorf("读取 %s 失败: %w", manifestName, err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s 解析失败: %w", manifestName, err)
	}
	if m.Version == "" || m.Platform == "" || m.SHA256 == "" {
		return m, fmt.Errorf("%s 缺必填字段（version/platform/sha256）", manifestName)
	}
	return m, nil
}

// fileSHA256 文件内容的 sha256（小写十六进制）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	// 1 MiB 缓冲：几百 MB 的二进制约几百次 syscall，不让哈希成为探测的瓶颈。
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dirSize 目录下常规文件总字节（不跟随软链接；读不了就当 0）。
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Prober 探测结论的进程级缓存 + 预热闸门。
//
// 为什么必须缓存：设置页每次打开、每次预览都要问「kit 可用吗」；而探测会读 KIT.json、
// 哈希 400MB 的二进制 —— 一次几百 ms，不能挂在命令循环上。卸载/安装后由 Refresh 重探。
type Prober struct {
	mu     sync.Mutex
	cached *Status
	kit    *Kit
	warm   sync.Once
	opts   ProbeOptions
}

// NewProber 构造探测缓存（opts 为生产口径；测试可注入假 kit 路径）。
func NewProber(opts ProbeOptions) *Prober { return &Prober{opts: opts} }

// Status 取缓存结论；首次调用才真正探测。
func (p *Prober) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached == nil {
		st := Probe(p.opts)
		p.cached, p.kit = &st, st.Kit
	}
	return *p.cached
}

// Refresh 重新探测（设置页「重新探测」按钮）。返回新结论。
//
// 为什么要显式重探：kit 的安装/卸载发生在本进程之外（用户手动放目录 / PR-13 安装器），
// 缓存不失效的话用户会一直看到「未安装」—— 那正是「按一下没反应」的经典形态。
func (p *Prober) Refresh() Status {
	p.mu.Lock()
	p.cached = nil
	p.mu.Unlock()
	return p.Status()
}

// Kit 当前可用 kit（nil = 不可用）。
func (p *Prober) Kit() *Kit {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached == nil {
		st := Probe(p.opts)
		p.cached, p.kit = &st, st.Kit
	}
	return p.kit
}
