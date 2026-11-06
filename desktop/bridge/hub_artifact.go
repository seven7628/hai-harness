package main

// hub_artifact.go：产物登记与按 id 预览（P3a-07，§6.9）。
//
// 登记入口只有 agenthub.RegisterArtifact（版本分配在进程内串行）；预览只接受
// 已登记的 art_ id，不接收任意路径，并在读取前做群成员/owner-only/2MB 上限检查。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

const (
	artifactPreviewChunk = 64 << 10
	artifactPreviewMax   = 2 << 20
)

func registerHubArtifactTools(eng *tools.Engine, m *manager, agentID agenthub.AgentID) {
	if eng == nil || m == nil || agentID == "" {
		return
	}
	eng.RegisterTool(context.Background(), newHubArtifactReadTool(m, agentID))
}

// registerTaskArtifacts 把 AgentEnd 的产物登记到 Hub（按 owner/name 版本化）。
// workspace 任务没有 agent 归属，跳过（产物仍留在任务结果里）。
func registerTaskArtifacts(ctx context.Context, st agenthub.Store, task *agenthub.Task, arts []core.Artifact) error {
	if task == nil || len(arts) == 0 {
		return nil
	}
	// 生产任务目前全是 TargetWorkspace（service.go），owner 回退到派发者；
	// agent-target 任务（P3b-03）仍优先用执行者。
	owner := task.Target.AgentID
	if owner == "" {
		owner = task.Origin.AgentID
	}
	if owner == "" {
		return nil // 人类直发、无 agent 归属
	}
	gid := task.Target.GroupID
	if gid == "" {
		gid = task.Origin.GroupID
	}
	var firstErr error
	for _, a := range arts {
		path := strings.TrimSpace(a.Path)
		source := path
		if source != "" && !filepath.IsAbs(source) && strings.TrimSpace(task.Target.Workspace) != "" {
			source = filepath.Join(task.Target.Workspace, source)
		}
		displayPath := path
		if filepath.IsAbs(displayPath) {
			// core.Artifact 允许工作区外绝对路径；登记只存基名，真实文件进版本副本。
			displayPath = filepath.Base(displayPath)
		}
		rec := &agenthub.Artifact{
			Name: a.Name, Path: displayPath, SourcePath: source, Kind: a.Kind,
			Size: a.Size, Lines: a.Lines, SHA256: a.SHA256,
			OwnerAgentID: owner, GroupID: gid, Visibility: agenthub.VisibilityGroup,
		}
		if err := agenthub.RegisterArtifact(ctx, st, rec); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// —— artifact_read ——

type hubArtifactReadTool struct {
	tools.BaseTool
	m       *manager
	agentID agenthub.AgentID
}

func newHubArtifactReadTool(m *manager, agentID agenthub.AgentID) *hubArtifactReadTool {
	return &hubArtifactReadTool{BaseTool: tools.BaseTool{
		Name_: "artifact_read",
		Description_: "Read a registered Hub artifact by id (never by path). " +
			"Large files are paginated; preview is capped at 2 MB.",
		Params_: tools.Obj(map[string]any{
			"artifact_id": tools.Str("Artifact id (art_...)"),
			"offset":      tools.Int("Byte offset (default 0)"),
			"limit":       tools.Int("Bytes to read (default 64 KiB, max 2 MiB)"),
		}, "artifact_id"),
		CanParallel_: true, ReadOnly_: true,
	}, m: m, agentID: agentID}
}

func (t *hubArtifactReadTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a struct {
		ArtifactID string `json:"artifact_id"`
		Offset     int64  `json:"offset"`
		Limit      int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	st, err := t.m.hubStore()
	if err != nil {
		return "", err
	}
	out, err := previewArtifact(ctx, st, a.ArtifactID, t.agentID, false, a.Offset, a.Limit)
	if err != nil {
		return "", err
	}
	return marshalHubToolResult(out)
}

func (t *hubArtifactReadTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if strings.TrimSpace(a.ArtifactID) == "" {
		return fmt.Errorf("%w: artifact_id 不能为空", agenthub.ErrInvalid)
	}
	return nil
}

// hubArtifactGet 是人类预览命令；人类可读 owner-only，但不能读未登记路径。
func (m *manager) hubArtifactGet(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	out, err := previewArtifact(context.Background(), st,
		str(c.Payload, "artifact_id"), "", true,
		numOr(c.Payload, "offset", 0), int(numOr(c.Payload, "limit", 0)))
	if err != nil {
		m.resp(c, false, "读取产物失败: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", out)
}

func previewArtifact(ctx context.Context, st agenthub.Store, id string, viewer agenthub.AgentID, isHuman bool, offset int64, limit int) (map[string]any, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: store 为空", agenthub.ErrInvalid)
	}
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, agenthub.PrefixArtifact+"_") {
		return nil, fmt.Errorf("%w: artifact_id 必须是已登记的 %s_ id", agenthub.ErrInvalid, agenthub.PrefixArtifact)
	}
	a, err := st.LoadArtifact(ctx, agenthub.ArtifactID(id))
	if err != nil {
		return nil, err
	}
	if !isHuman {
		if a.GroupID != "" {
			g, err := st.LoadGroup(ctx, a.GroupID)
			if err != nil {
				return nil, err
			}
			if g.Deleted {
				return nil, fmt.Errorf("%w: 群组已删除", agenthub.ErrNotFound)
			}
			if _, ok := g.RoleOf(viewer); !ok {
				return nil, fmt.Errorf("%w: 不是该群成员", agenthub.ErrInvalid)
			}
		} else if viewer == "" || viewer != a.OwnerAgentID {
			return nil, fmt.Errorf("%w: 无权读取该产物", agenthub.ErrInvalid)
		}
		if !agenthub.ArtifactVisibleTo(a, viewer, false) {
			return nil, fmt.Errorf("%w: 无权读取该产物", agenthub.ErrInvalid)
		}
	}
	source := strings.TrimSpace(a.SourcePath)
	if source == "" && filepath.IsAbs(strings.TrimSpace(a.Path)) {
		source = a.Path
	}
	if source == "" {
		return nil, fmt.Errorf("%w: 产物没有可读源路径", agenthub.ErrInvalid)
	}
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: 产物不是普通文件", agenthub.ErrInvalid)
	}
	if offset < 0 {
		offset = 0
	}
	if offset > fi.Size() {
		offset = fi.Size()
	}
	if limit <= 0 {
		limit = artifactPreviewChunk
	}
	if limit > artifactPreviewMax {
		limit = artifactPreviewMax
	}
	buf := make([]byte, limit)
	n, readErr := f.ReadAt(buf, offset)
	if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}
	data := buf[:n]
	out := map[string]any{
		"artifact_id": string(a.ID), "name": a.Name, "version": a.Version,
		"visibility": a.Visibility.EffectiveVisibility(), "size": fi.Size(),
		"offset": offset, "next_offset": offset + int64(n),
		"truncated": offset+int64(n) < fi.Size(),
	}
	if bytes.IndexByte(data, 0) >= 0 {
		out["binary"] = true
		out["content"] = ""
		return out, nil
	}
	out["content"] = string(data)
	return out, nil
}
