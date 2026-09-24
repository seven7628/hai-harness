package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/skills"
)

// Result 单次安装结果。
type Result struct {
	Name   string // 技能名（= 安装目录名）
	Source string // 来源 URL
	Scope  string // global|workspace
	Target string // 实际安装目录
}

// InstallOptions 安装选项。
type InstallOptions struct {
	Scope     string // "global" | "workspace"
	Workspace string // workspace 作用域时必填
	Select    string // 可选：仅安装解析结果中名==Select 的技能（市场浏览后精确安装单个）
}

// Install 安装远程技能：git 克隆 → 解析候选 → 校验（skills.ParseSkill）→ 复制到目标作用域目录
// → 写来源清单。返回一个或多个结果（marketplace 装多个）。已存在的技能拒绝（防覆盖误删），
// 需先卸载或用 Update。Select 非空 → 只安装名==Select 的技能（匹配不到报错）。
func Install(ctx context.Context, rawURL string, opts InstallOptions, env Env) ([]Result, error) {
	if opts.Scope != "global" && opts.Scope != "workspace" {
		return nil, fmt.Errorf("未知作用域 %q（支持 global/workspace）", opts.Scope)
	}
	if opts.Scope == "workspace" && opts.Workspace == "" {
		return nil, errors.New("workspace 作用域需要工作区路径")
	}
	src, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}

	staging, err := clone(ctx, src)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	head, err := headCommit(ctx, staging)
	if err != nil {
		return nil, err
	}

	cands, err := resolveSkills(staging, src)
	if err != nil {
		return nil, err
	}

	manifestMu.Lock()
	defer manifestMu.Unlock()
	man := loadManifest(env)

	var results []Result
	for _, c := range cands {
		sk, err := skills.ParseSkill(c.dir)
		if err != nil {
			return nil, fmt.Errorf("技能校验失败: %w", err)
		}
		name := sk.Name
		if opts.Select != "" && name != opts.Select {
			continue // 市场精确安装：跳过未选中的技能
		}
		if !validSkillName(name) {
			return nil, fmt.Errorf("非法技能名 %q（来自 SKILL.md frontmatter）", name)
		}
		if _, exists := man.Items[name]; exists {
			return nil, fmt.Errorf("技能 %q 已安装（来源 %s）；先卸载或更新", name, man.Items[name].Source)
		}
		target := targetDir(opts.Scope, opts.Workspace, name, env)
		if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err == nil {
			return nil, fmt.Errorf("目标目录已存在技能 %q（%s），请先卸载", name, target)
		}
		// 旧路径（.go-code 命名空间）已存在同名技能 → 拒绝，避免新旧路径双份。
		if legacy := legacyPath(opts.Scope, opts.Workspace, name, env); legacy != "" {
			if _, err := os.Stat(filepath.Join(legacy, "SKILL.md")); err == nil {
				return nil, fmt.Errorf("技能 %q 已存在于旧路径 %s，请先卸载", name, legacy)
			}
		}
		if err := copyDir(c.dir, target); err != nil {
			return nil, fmt.Errorf("安装 %s: %w", name, err)
		}
		now := time.Now().Format(time.RFC3339)
		man.Items[name] = Item{
			Source: rawURL, Kind: src.Kind, Ref: src.Ref, Subdir: src.Subdir,
			Scope: opts.Scope, Workspace: opts.Workspace, Target: target,
			Installed: now, Commit: head,
		}
		results = append(results, Result{Name: name, Source: rawURL, Scope: opts.Scope, Target: target})
	}
	if len(results) == 0 {
		if opts.Select != "" {
			return nil, fmt.Errorf("未找到技能 %q", opts.Select)
		}
		return nil, errors.New("未找到可安装的技能")
	}
	if err := saveManifest(env, man); err != nil {
		return results, fmt.Errorf("来源清单写盘失败: %w", err)
	}
	return results, nil
}

// Update 更新已安装技能：按清单条目重拉最新 ref → 校验同名 → 替换目标目录 → 刷新清单。
func Update(ctx context.Context, name string, env Env) error {
	manifestMu.Lock()
	man := loadManifest(env)
	item, ok := man.Items[name]
	manifestMu.Unlock()
	if !ok {
		return fmt.Errorf("未安装的远程技能 %q", name)
	}
	src, err := ParseURL(item.Source)
	if err != nil {
		return err
	}
	staging, err := clone(ctx, src)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	head, err := headCommit(ctx, staging)
	if err != nil {
		return err
	}

	cands, err := resolveSkills(staging, src)
	if err != nil {
		return err
	}
	var found *string
	for _, c := range cands {
		if sk, err := skills.ParseSkill(c.dir); err == nil && sk.Name == name {
			found = &c.dir
			break
		}
	}
	if found == nil {
		return fmt.Errorf("远端已找不到技能 %q", name)
	}
	target := installedTarget(item, env)
	tmp := target + ".tmp-" + time.Now().Format("150405")
	if err := copyDir(*found, tmp); err != nil {
		return fmt.Errorf("更新 %s: %w", name, err)
	}
	if err := os.RemoveAll(target); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	item.Installed = time.Now().Format(time.RFC3339)
	item.Commit = head
	item.Target = target
	manifestMu.Lock()
	man = loadManifest(env)
	man.Items[name] = item
	err = saveManifest(env, man)
	manifestMu.Unlock()
	return err
}

// Uninstall 卸载：删除目标目录 + 清单条目。
func Uninstall(name string, env Env) error {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	man := loadManifest(env)
	item, ok := man.Items[name]
	if !ok {
		return fmt.Errorf("未安装的远程技能 %q", name)
	}
	target := installedTarget(item, env)
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("删除 %s: %w", target, err)
	}
	delete(man.Items, name)
	return saveManifest(env, man)
}

// List 已安装远程技能（按名排序；UI 展示来源/作用域/更新时间）。
func List(env Env) []Item {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	man := loadManifest(env)
	out := make([]Item, 0, len(man.Items))
	for name, it := range man.Items {
		it.Name = name
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// IsInstalled 是否已安装（bridge 检查冲突用）。
func IsInstalled(name string, env Env) bool {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	_, ok := loadManifest(env).Items[name]
	return ok
}

// candidate 一个待安装技能（源目录；name 由 ParseSkill 填充）。
type candidate struct {
	dir string
}

// gitWaitDelay 子命令**成功退出后**等待 I/O 管道关闭的上限（C10 同型站点，2026-09-18）。
//
// **为什么必需**：`CombinedOutput()` 内部就是 `cmd.Stdout = &buf`（io.Writer）⇒ Go 建
// `os.Pipe` + 拷贝协程 ⇒ `Wait()` **依赖管道关闭**（os/exec 的 awaitGoroutines 要等到
// 管道 EOF）；而 `exec.CommandContext` 在 ctx 到期时**只杀直接子进程**，git 拉起的后代
// （credential helper、传输进程、钩子）仍存活并握住写端 ⇒ `Wait()` 永不返回 ——
// **超时在这里是失效的**（超时管不住一个「进程自己已退出、但管道被后代持有」的等待）。
//
// 这正是 2026-09-18 **C8** 在 `sandbox.runExec` 上修掉的同一缺陷（生产实测挂起 602s）；
// 本处是同型站点，故采用同一处方（与 sandbox.waitDelayAfterKill、runtime/pyWaitDelay 同值）。
//
// 语义：进程退出后最多再等 `gitWaitDelay`，到点由 `os/exec` 强制关管道并使 `Wait` 返回
// （返回 `exec.ErrWaitDelay`）；该情形下输出可能被进程组外的后代截断，由调用点负责
// **不把成功印成失败**（见 gitCombined）。
const gitWaitDelay = 3 * time.Second

// gitCombined 执行 git 命令并合并捕获 stdout+stderr（C10：本包 clone / sparse-checkout /
// rev-parse 三处的统一出口）。
//
// dir 为子进程工作目录（"" = 继承当前目录，clone 用 URL + 目标路径、不需要 -C）；
// GIT_TERMINAL_PROMPT=0 防认证卡死（原先三处各自设置，现汇总于此）。
//
// C10 为什么只做两件事（不注入带锁缓冲）：CombinedOutput 自带缓冲、**无法**注入自定义
// io.Writer，所以这里只有 `WaitDelay`（保证有界返回）与 `ErrWaitDelay` 放行（不把成功
// 判成失败）。为加锁而把 CombinedOutput 改写成显式 Stdout/Stderr 是更大的重构，
// 且该竞争窗口在主会话实测中是微秒级、-race 下未复现 —— 不在本修复范围内。
func gitCombined(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	// C10：进程退出后最多再等 gitWaitDelay 即强制关管道并返回（见 gitWaitDelay 注释）。
	cmd.WaitDelay = gitWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		// C10：ErrWaitDelay = 进程**已成功退出**，只是管道被进程组外的后代占着。
		// CombinedOutput 把 `Wait` 的错误**原样返回** ⇒ 不在这里放行，就等于把「罕见挂住」
		// 换成「罕见假失败」——比现状更糟。os/exec 仅在进程成功退出且未走 Cancel 时才返回它。
		//
		// 为什么不追加「输出可能被截断」的说明：成功路径上 clone / sparse-checkout 的输出
		// 本就被丢弃，而 rev-parse 的输出是**机器解析**的 commit 哈希，附加说明会毁掉它
		// （见 headCommit 对截断后果的说明）。
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return out, nil
		}
	}
	return out, err
}

// clone git 克隆到临时目录（浅克隆；子目录技能加 sparse）。GIT_TERMINAL_PROMPT=0 防认证卡死。
func clone(ctx context.Context, src Source) (string, error) {
	dir, err := os.MkdirTemp("", "go-code-skill-remote-")
	if err != nil {
		return "", err
	}
	args := []string{"clone", "--depth", "1"}
	if src.Ref != "" {
		args = append(args, "--branch", src.Ref)
	}
	if src.Kind == "subdir" {
		args = append(args, "--filter=blob:none", "--sparse")
	}
	args = append(args, src.URL, dir)
	if out, err := gitCombined(ctx, "", args...); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("git clone %s: %w\n%s", src.URL, err, tail(out))
	}
	if src.Kind == "subdir" && src.Subdir != "" {
		if out, err := gitCombined(ctx, "", "-C", dir, "sparse-checkout", "set", src.Subdir); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("sparse checkout %s: %w\n%s", src.Subdir, err, tail(out))
		}
	}
	return dir, nil
}

// headCommit 取克隆仓库的 HEAD commit（安装/更新时记录，Browse 据此判更新）。
func headCommit(ctx context.Context, dir string) (string, error) {
	out, err := gitCombined(ctx, "", "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w\n%s", err, tail(out))
	}
	// 管道被进程组外后代占据（ErrWaitDelay ⇒ 见 gitCombined）时输出可能被截断，这里照旧
	// 返回取到的内容而**不判失败**：极坏情况是 commit 为空/不全，Browse 对空 commit 的
	// 既有处理是「视为已安装·最新、不提示更新」（见 browse.go）——只少一次更新提示，
	// 而「判失败」会把一次成功安装整体打成失败，比现状更糟。
	return strings.TrimSpace(string(out)), nil
}

// resolveSkills 在克隆仓库里定位技能目录。
func resolveSkills(staging string, src Source) ([]candidate, error) {
	switch src.Kind {
	case "subdir":
		dir := filepath.Join(staging, filepath.FromSlash(src.Subdir))
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			return nil, fmt.Errorf("子目录 %q 无 SKILL.md（%v）", src.Subdir, err)
		}
		return []candidate{{dir: dir}}, nil
	case "name":
		for _, p := range []string{
			filepath.Join(staging, "skills", src.Name),
			filepath.Join(staging, ".claude", "skills", src.Name),
			filepath.Join(staging, src.Name),
		} {
			if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err == nil {
				return []candidate{{dir: p}}, nil
			}
		}
		return nil, fmt.Errorf("仓库内未找到技能 %q（skills/ 或 .claude/skills/）", src.Name)
	default: // repo
		if _, err := os.Stat(filepath.Join(staging, "SKILL.md")); err == nil {
			return []candidate{{dir: staging}}, nil // 仓库根即技能
		}
		if _, err := os.Stat(filepath.Join(staging, ".claude-plugin", "marketplace.json")); err == nil {
			return resolveMarketplace(staging) // 市场仓库：装全部插件技能
		}
		if entries, err := os.ReadDir(filepath.Join(staging, "skills")); err == nil {
			var cands []candidate
			for _, e := range entries {
				if e.IsDir() {
					cands = append(cands, candidate{dir: filepath.Join(staging, "skills", e.Name())})
				}
			}
			if len(cands) > 0 {
				return cands, nil // 仓库 skills/ 目录批量技能
			}
		}
		return nil, errors.New("仓库根无 SKILL.md（不是技能仓库，也不是 marketplace）")
	}
}

// resolveMarketplace 解析 .claude-plugin/marketplace.json：每个插件的 skills/ 目录全部技能。
func resolveMarketplace(staging string) ([]candidate, error) {
	raw, err := os.ReadFile(filepath.Join(staging, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, fmt.Errorf("marketplace.json: %w", err)
	}
	var mk struct {
		Plugins []struct {
			Source string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &mk); err != nil {
		return nil, fmt.Errorf("marketplace.json 解析: %w", err)
	}
	var cands []candidate
	for _, p := range mk.Plugins {
		base := staging
		if p.Source != "" && p.Source != "./" && p.Source != "." {
			base = filepath.Join(staging, filepath.FromSlash(strings.TrimPrefix(p.Source, "./")))
		}
		if _, err := os.Stat(filepath.Join(base, "SKILL.md")); err == nil {
			cands = append(cands, candidate{dir: base}) // 插件根即技能
		}
		if entries, err := os.ReadDir(filepath.Join(base, "skills")); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					cands = append(cands, candidate{dir: filepath.Join(base, "skills", e.Name())})
				}
			}
		}
	}
	if len(cands) == 0 {
		return nil, errors.New("marketplace 未暴露任何技能")
	}
	return cands, nil
}

// targetDir 目标安装目录（2026-08-29 双命名空间兼容）：新安装统一走主路径
// .agents 命名空间；旧路径 .go-code 的既有技能由 installedTarget 定位（管理兼容），
// 双份由 Install 的冲突检测（旧路径同名技能检查）兜底。
//   - workspace 作用域 → {ws}/.agents/skills/<name>
//   - global 作用域 → ~/.agents/skills/<name>
func targetDir(scope, workspace, name string, env Env) string {
	if scope == "workspace" {
		return filepath.Join(workspace, skills.AgentDirName, "skills", name)
	}
	return filepath.Join(env.AgentSkillsDir, name)
}

// installedTarget 已安装条目的实际目录：优先读清单持久化的 Target（旧条目无
// Target 时按 scope 回退到兼容旧路径，保证升级后 update/uninstall 仍找到旧安装）。
func installedTarget(item Item, env Env) string {
	if item.Target != "" {
		return item.Target
	}
	return legacyPath(item.Scope, item.Workspace, item.Name, env)
}

// legacyPath 旧命名空间（.go-code）下的技能目录；新安装不存在该目录时返回空串。
func legacyPath(scope, workspace, name string, env Env) string {
	if scope == "workspace" {
		return filepath.Join(workspace, skills.GoCodeDirName, "skills", name)
	}
	return filepath.Join(env.GlobalSkillsDir, name)
}

// copyDir 递归复制目录（跳过 .git）。
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		_, cerr := io.Copy(out, in)
		ec := out.Close()
		if cerr != nil {
			return cerr
		}
		return ec
	})
}

// tail 截断命令输出尾部（错误摘要）。
func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		return s[len(s)-500:]
	}
	return s
}
