package main

// B1（SessionSend Review）：session_send 的 workspace 来自模型 JSON，
// CheckAttachments 在建账前必须与 Dispatch/CommandSession 同口径校验「工作区已登记」——
// 否则 hubPrepareAttachments 会把任意根（尤其 "/"）加进附件白名单，未沙箱 bridge
// 就能按任意根读盘；图片还会被整份读入/解码（存在/大小/类型 oracle + CPU/内存 DoS）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/session"
)

func TestHubWorkspaceCheckAttachmentsRejectsUnregisteredWorkspace(t *testing.T) {
	m, buf := testManager(t)
	registered := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "CheckAttachStranger", []string{registered})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(registered); err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	ctx := context.Background()

	// 真实可解码图片：未修的版本会把 "/" 当成白名单根，这张图会被整份读出并解码。
	outside := t.TempDir()
	outsideImg := filepath.Join(outside, "outside.png")
	writeTestPNG(t, outsideImg, 4, 4)
	// 哨兵：chmod 000 的坏图 —— 一旦真去读，错误会是「读取图片失败」（ErrInvalid），
	// 而不是工作区未登记的 ErrNotFound；断言错误类型即可区分「先挡工作区」还是「先读盘」。
	sentinel := filepath.Join(outside, "sentinel.png")
	if err := os.WriteFile(sentinel, []byte("not a png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sentinel, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sentinel, 0o600) })
	before, err := os.Stat(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(outside, "missing.png")
	unregistered := filepath.Join(t.TempDir(), "not-registered")

	for _, ws := range []string{"/", unregistered, outside} {
		for _, req := range []agenthub.AttachmentReq{
			{AgentID: p.ID, Workspace: ws, Files: []string{sentinel}},
			{AgentID: p.ID, Workspace: ws, Images: []string{outsideImg}},
			{AgentID: p.ID, Workspace: ws, Images: []string{sentinel}},
			{AgentID: p.ID, Workspace: ws, Images: []string{missing}},
		} {
			infos, err := ops.CheckAttachments(ctx, req)
			if !errors.Is(err, agenthub.ErrNotFound) {
				t.Fatalf("CheckAttachments(workspace=%q, files=%v, images=%v) = %v, want ErrNotFound（未登记工作区必须在读盘前被拒）",
					ws, req.Files, req.Images, err)
			}
			if len(infos) != 0 {
				t.Fatalf("CheckAttachments(workspace=%q) 返回了附件: %+v", ws, infos)
			}
		}
	}
	// 没有读取/改写哨兵（读取不改 mtime，这里防的是实现「顺手落盘/压缩」之类副作用；
	// 真正的「没读」由上面的错误类型与 missing 路径共同证明：读盘路径一律 ErrInvalid）。
	after, err := os.Stat(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("哨兵文件被改动: before=%v/%d after=%v/%d", before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}

func TestHubWorkspaceCheckAttachmentsAcceptsRegisteredWorkspace(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "CheckAttachOk", []string{wsPath})
	// 只登记 registry、不装配 runtime：模拟重启后还没打开工作区的情形。
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	ctx := context.Background()

	pngPath := filepath.Join(wsPath, "shot.png")
	writeTestPNG(t, pngPath, 4, 4)
	mdPath := filepath.Join(wsPath, "notes.md")
	if err := os.WriteFile(mdPath, []byte("# notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	pngReal, err := filepath.EvalSymlinks(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	mdReal, err := filepath.EvalSymlinks(mdPath)
	if err != nil {
		t.Fatal(err)
	}

	infos, err := ops.CheckAttachments(ctx, agenthub.AttachmentReq{
		AgentID: p.ID, Workspace: wsPath, Files: []string{mdPath}, Images: []string{pngPath},
	})
	if err != nil {
		t.Fatalf("CheckAttachments(registered) = %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("attachments = %+v, want file + image", infos)
	}
	if infos[0].Kind != agenthub.AttachmentKindFile || infos[0].Path != mdReal ||
		infos[0].Name != "notes.md" || infos[0].Bytes != int64(len("# notes")) {
		t.Fatalf("file info = %+v", infos[0])
	}
	if infos[1].Kind != agenthub.AttachmentKindImage || infos[1].Path != pngReal || infos[1].Bytes <= 0 {
		t.Fatalf("image info = %+v", infos[1])
	}

	// 尾斜杠/多余分隔符归一后仍命中原登记（knownWorkspace 返回归一路径，不另造 runtime）。
	if _, err := ops.CheckAttachments(ctx, agenthub.AttachmentReq{
		AgentID: p.ID, Workspace: wsPath + string(filepath.Separator) + ".", Files: []string{mdPath},
	}); err != nil {
		t.Fatalf("CheckAttachments(normalized workspace) = %v", err)
	}

	// 登记工作区不等于放开任意目录：工作区外的文件仍按 policy 拒绝（白名单根没被放宽）。
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.CheckAttachments(ctx, agenthub.AttachmentReq{
		AgentID: p.ID, Workspace: wsPath, Files: []string{outside},
	}); !errors.Is(err, agenthub.ErrForbidden) {
		t.Fatalf("CheckAttachments(outside) = %v, want ErrForbidden", err)
	}
}

// N9：命令先于正文入队；命令失败 = 这一批没投递，pending 与 client_msg_id 占位
// 都必须回滚 —— 否则会话被永久读成忙、合法重试被误判 duplicate。
// 用已关闭的会话让 bs.s.Command 失败（它在 Ask 之前执行，命中的正是命令失败分支）。
func TestHubWorkspaceDispatchCommandFailureRollsBackPendingAndDedup(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "CmdFailAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-cmd-fail", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	ops := newHubWorkspaceOps(m)
	const clientMsgID = "cm-cmd-fail"
	if _, err := ops.Dispatch(context.Background(), agenthub.DispatchReq{
		AgentID: p.ID, Workspace: wsPath, SessionID: bs.id,
		Command: agenthub.CommandCompact, Text: "正文", ClientMsgID: clientMsgID,
	}); !errors.Is(err, session.ErrSessionClosed) {
		t.Fatalf("dispatch on closed session = %v, want ErrSessionClosed", err)
	}
	if bs.hubDispatchPending.Load() {
		t.Fatal("命令失败后 hubDispatchPending 泄漏：会话会被永久读成忙")
	}
	m.mu.Lock()
	_, seen := m.hubMsgDedup[wsPath+"\x00"+bs.id+"\x00"+clientMsgID]
	m.mu.Unlock()
	if seen {
		t.Fatal("命令失败后 client_msg_id 占位没回滚：合法重试会被当成 duplicate")
	}
}
