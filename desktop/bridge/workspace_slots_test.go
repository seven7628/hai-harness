package main

// BR-21 回归：预览转换器的**工作区槽位**（owner = 工作区）+ 进程级单例的生命周期。
//
// 五者分两类，本文件各钉一条不变式：
//   - 预览器：沙箱根按工作区构造 → 按工作区建槽，root 只能来自槽位（不能来自调用点）；
//   - hookTrust / imPlugin：**底层连接/账本是进程级一份**（一个 hooks-state.json、
//     一个 IM 账号），但**归属（owner）必须隔离** —— 条目按 agent/group/workspace 分表，
//     启用与路由按 owner 记。共享实例是为了「即刻生效 / 不断线」，不是为了让授权互通。
//     owner 维度的断言见 hooks_owner_test.go / im_owner_test.go；本文件只钉连接生命周期。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/im"
	"github.com/seven7628/hai-harness/owner"
	"github.com/seven7628/hai-harness/plugin"
	implugin "github.com/seven7628/hai-harness/plugin/im"
)

// TestPreviewSandboxRootBoundToSlot 槽位的沙箱根只看槽位工作区：
// 调用点传进来的 workspace（哪怕是别的工作区）都不参与决定。
// 这条不变式一旦被改回「调用点说了算」，跨工作区越权就会静默复发。
func TestPreviewSandboxRootBoundToSlot(t *testing.T) {
	cases := []struct{ name, slot, call, want string }{
		{"槽位有工作区，调用点传别的 → 以槽位为准", "/ws/a", "/ws/b", "/ws/a"},
		{"槽位有工作区，调用点传空 → 仍以槽位为准", "/ws/a", "", "/ws/a"},
		{"工作区外预览（ws=\"\"）→ 保持旧语义（根为空）", "", "", ""},
		{"无槽位工作区时兜底调用点", "", "/ws/b", "/ws/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := previewSandboxRoot(tc.slot, tc.call); got != tc.want {
				t.Fatalf("previewSandboxRoot(%q, %q) = %q, want %q", tc.slot, tc.call, got, tc.want)
			}
		})
	}
}

// TestPreviewSlotsArePerWorkspace 每个工作区一个槽位：同 key 复用、不同 key 隔离，
// 且三个转换器（xlsx/docx/pptx）各自绑定同一个工作区根。
func TestPreviewSlotsArePerWorkspace(t *testing.T) {
	m := &manager{}
	py := testPythonRuntime(t)

	a1 := m.previewSlotFor("/ws/a", py)
	a2 := m.previewSlotFor("/ws/a", py)
	if a1 != a2 {
		t.Fatal("同一工作区必须复用同一槽位（懒建只建一次）")
	}
	b := m.previewSlotFor("/ws/b", py)
	if b == a1 {
		t.Fatal("不同工作区必须是不同槽位（否则又回到进程级共享）")
	}
	if a1.workspace != "/ws/a" || b.workspace != "/ws/b" {
		t.Fatalf("槽位 owner 必须是工作区路径，得到 %q / %q", a1.workspace, b.workspace)
	}
	if a1.xlsx == b.xlsx || a1.docx == b.docx || a1.pptx == b.pptx {
		t.Fatal("不同槽位的转换器实例必须互不相同（每个实例绑定自己的沙箱根）")
	}
	if got := m.previewSlotWorkspaces(); len(got) != 2 || got[0] != "/ws/a" || got[1] != "/ws/b" {
		t.Fatalf("owner 清单应为 [/ws/a /ws/b]（升序），得到 %v", got)
	}
}

// TestPreviewSlotRebuiltOnRuntimeChange 运行时实例换了 → 槽位重建（不会拿着旧 py 继续跑），
// 但沙箱根仍是同一个工作区。
func TestPreviewSlotRebuiltOnRuntimeChange(t *testing.T) {
	m := &manager{}
	py1, py2 := testPythonRuntime(t), testPythonRuntime(t)
	s1 := m.previewSlotFor("/ws/a", py1)
	s2 := m.previewSlotFor("/ws/a", py2)
	if s1 == s2 {
		t.Fatal("运行时变更必须重建槽位（否则新运行时永远不生效）")
	}
	if s2.workspace != "/ws/a" {
		t.Fatalf("重建后 owner 仍是原工作区，得到 %q", s2.workspace)
	}
}

// TestPreviewSlotReleaseAndReclaim 回收路径：释放后登记表里没有该工作区，
// 且再次使用会重建（不是拿到已被释放的旧槽位）。
func TestPreviewSlotReleaseAndReclaim(t *testing.T) {
	m := &manager{}
	py := testPythonRuntime(t)
	old := m.previewSlotFor("/ws/a", py)
	m.previewSlotFor("/ws/b", py)
	m.releasePreviewSlot("/ws/a")
	if got := m.previewSlotWorkspaces(); len(got) != 1 || got[0] != "/ws/b" {
		t.Fatalf("释放后 owner 清单应只剩 /ws/b，得到 %v", got)
	}
	again := m.previewSlotFor("/ws/a", py)
	if again == old {
		t.Fatal("释放后应重建槽位（旧实例不得被复活复用）")
	}
	m.releasePreviewSlot("/never-registered") // 幂等：未知 key 无副作用
	m.releaseAllPreviewSlots()
	if got := m.previewSlotWorkspaces(); len(got) != 0 {
		t.Fatalf("releaseAll 后不应残留任何槽位，得到 %v", got)
	}
}

// TestPreviewSlotsBounded 槽位表有上限：任何 workspace 字符串都不能把它撑成无界登记表
// （上限淘汰按建槽时刻，最新使用的那一个必须留下）。
func TestPreviewSlotsBounded(t *testing.T) {
	m := &manager{}
	py := testPythonRuntime(t)
	for i := 0; i < previewSlotLimit+8; i++ {
		m.previewSlotFor(fmt.Sprintf("/ws/%02d", i), py)
	}
	got := m.previewSlotWorkspaces()
	if len(got) != previewSlotLimit {
		t.Fatalf("槽位数应为上限 %d，得到 %d（%v）", previewSlotLimit, len(got), got)
	}
	want := fmt.Sprintf("/ws/%02d", previewSlotLimit+7)
	if got[len(got)-1] != want {
		t.Fatalf("最新建槽的 %s 必须保留，得到 %v", want, got)
	}
	if got[0] == "/ws/00" {
		t.Fatalf("最旧的槽位应被淘汰，得到 %v", got)
	}
}

// TestDeleteWorkspaceReleasesPreviewSlot 删除工作区必须带走它的槽位
// （登记表只增不减正是本批要治的病）。
func TestDeleteWorkspaceReleasesPreviewSlot(t *testing.T) {
	m, _ := testManager(t)
	wsPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_ = m.runtime(wsPath) // 登记运行态（deleteWorkspace 只处理已登记的）
	py := testPythonRuntime(t)
	m.previewSlotFor(wsPath, py)
	if len(m.previewSlotWorkspaces()) != 1 {
		t.Fatalf("建槽后应有 1 个槽位，得到 %v", m.previewSlotWorkspaces())
	}
	m.deleteWorkspace(wsPath)
	if got := m.previewSlotWorkspaces(); len(got) != 0 {
		t.Fatalf("删除工作区后槽位必须一起回收，得到 %v", got)
	}
}

// TestShutdownWorkspaceReleasesPreviewSlot 关闭工作区同样回收槽位（下次预览按需重建）。
func TestShutdownWorkspaceReleasesPreviewSlot(t *testing.T) {
	m, _ := testManager(t)
	wsPath := filepath.Join(t.TempDir(), "proj2")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_ = m.runtime(wsPath)
	m.previewSlotFor(wsPath, testPythonRuntime(t))
	m.shutdownWorkspace(wsPath)
	if got := m.previewSlotWorkspaces(); len(got) != 0 {
		t.Fatalf("关闭工作区后槽位必须回收，得到 %v", got)
	}
}

// TestHookTrustStoreSingleInstanceOwnerScoped 钉住两件**同时成立**的事：
//  1. 实例是进程级一份（一个文件、即刻生效，契约 §5.5）—— 否则 hooks_trust 写完要
//     重建会话才生效，或多份副本互相覆盖 ~/.go-code/hooks-state.json；
//  2. 条目**按 owner 隔离** —— 同一实例里 agent:a 的确认对 agent:b / group:g /
//     workspace:w 一律不生效（旧实现在这里正是「全局 trusted」，即审计的跨 owner 越权）。
func TestHookTrustStoreSingleInstanceOwnerScoped(t *testing.T) {
	m, _ := testManager(t)
	ts1 := m.hookTrustStore()
	ts2 := m.hookTrustStore()
	if ts1 != ts2 {
		t.Fatal("hookTrustStore 必须返回同一实例（一个文件、即刻生效）")
	}
	if err := ts1.Trust(owner.Agent("a"), "echo hi", "project", "test"); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if got := ts2.Check(owner.Agent("a"), "echo hi"); got != "trusted" {
		t.Fatalf("同 owner 写入后应立即可见（共享实例），得到 %q", got)
	}
	for _, other := range []string{owner.Agent("b"), owner.Group("g"), owner.Workspace("/ws/x")} {
		if got := ts2.Check(other, "echo hi"); got != "untrusted" {
			t.Fatalf("%s 不得继承 agent:a 的确认，得到 %q", other, got)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".go-code", "hooks-state.json")); err != nil {
		t.Fatalf("信任库应落在隔离 HOME 下的 hooks-state.json: %v", err)
	}
}

// stubIMConfig 返回一个「主开关关」的配置：Enable 会建 Runtime（不含真网关），
// 足以观察「Runtime 是否被停掉」。
func stubIMConfig(string) (im.Config, error) { return im.Config{Enabled: false}, nil }

// inProcessIMPlugin 造一个可断言 Runtime 生命周期的 IM 插件实例。
func inProcessIMPlugin(t *testing.T) *implugin.Plugin {
	t.Helper()
	p := implugin.New(filepath.Join(t.TempDir(), "im-config.json"))
	p.LoadConfig = stubIMConfig
	if _, err := p.Enable(context.Background()); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if p.Runtime() == nil {
		t.Fatal("Enable 后应有 Runtime")
	}
	return p
}

// TestIMPluginSurvivesWorkspaceClose 是本条最有价值的一例：IM 实例被登记进每个工作区的
// 插件注册表，而 Registry.Close 是**工作区级**收尾 —— 没有 ClosePreserve 时，
// 删掉别的项目 / 改一次浏览器配置 / 关一个工作区都会把它 Disable 掉，正在用的 IM 连接被
// 静默掐断（imBridge 还持着已停的 Runtime）。
func TestIMPluginSurvivesWorkspaceClose(t *testing.T) {
	p := inProcessIMPlugin(t)
	// 复刻真实形态：每个工作区一份 registry，但共享同一个 IM 实例（buildPlugins）。
	wsReg := plugin.NewRegistry(p)
	wsReg.Close(context.Background())
	if p.Runtime() == nil {
		t.Fatal("工作区级 Close 不得停掉进程级 IM 连接（implugin.Plugin.ClosePreserve）")
	}
	// 多次（多工作区）同样不得影响：幂等且无副作用。
	plugin.NewRegistry(p).Close(context.Background())
	plugin.NewRegistry(p).Close(context.Background())
	if p.Runtime() == nil {
		t.Fatal("反复工作区级 Close 后 Runtime 仍应在")
	}
}

// TestUserDisableStillStopsIM 用户显式关闭插件必须照旧停连接
// （ClosePreserve 只挡「工作区级收尾」，不能把用户的关闭动作也吞掉）。
func TestUserDisableStillStopsIM(t *testing.T) {
	p := inProcessIMPlugin(t)
	reg := plugin.NewRegistry(p)
	if err := reg.Disable(context.Background(), implugin.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if p.Runtime() != nil {
		t.Fatal("用户显式 Disable 必须停掉连接")
	}
}

// TestCloseIMPluginIsProcessScoped 进程级收尾：closeIMPlugin 是唯一停连接的地方，
// 且对 nil 实例/重复调用安全。
func TestCloseIMPluginIsProcessScoped(t *testing.T) {
	m, _ := testManager(t)
	m.closeIMPlugin() // imPlugin 为 nil：不得 panic
	p := inProcessIMPlugin(t)
	m.imPlugin = p
	m.closeIMPlugin()
	if p.Runtime() != nil {
		t.Fatal("进程级收尾必须停掉 IM 连接")
	}
	m.closeIMPlugin() // 幂等
	if p.Runtime() != nil {
		t.Fatal("重复收尾后 Runtime 仍应为 nil")
	}
}

// TestIMPluginEnableErrorIsReported 守住一个易被顺手改坏的语义：Enable 的部分失败
// 必须仍返回错误（ClosePreserve 的引入不应吃掉错误路径）。
func TestIMPluginEnableErrorIsReported(t *testing.T) {
	p := implugin.New(filepath.Join(t.TempDir(), "im-config.json"))
	p.LoadConfig = func(string) (im.Config, error) { return im.Config{}, errors.New("坏配置") }
	if _, err := p.Enable(context.Background()); err == nil {
		t.Fatal("配置读取失败时 Enable 必须报错")
	}
}
