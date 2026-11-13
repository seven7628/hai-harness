package main

// hub_task_artifact_preview.go：任务卡产物的「人类只读预览」命令（P4-UI-12）。
//
// 与 hub_artifact_get 的区别：任务结果里的 agenthub.TaskArtifact 没有 artifact_id
// （登记失败/旧任务/agent-target 任务都可能没有回流），所以这里以 **task_id + path**
// 寻址。安全边界不靠「任意路径可读」，而是：
//  1. path 必须精确命中 task.Result.Artifacts 里的某一项（path 为空才按 name 唯一匹配）；
//  2. 相对路径锚定任务根（workspace，否则执行者/发起者的 agent home）；
//  3. 绝对路径必须仍在任务根内；canonical + SameFile 拦住 .. 与软链逃逸；
//  4. 只读普通文件，响应不回绝对路径/目录。
//
// 该命令只给本地人类 UI 用，**不要**加进 mobile_server 白名单。

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/seven7628/hai-harness/agenthub"
)

// hubArtifactImageMax 图片预览上限：base64 后约 5.3MB，前端 <img> 能接受；
// 超过必须明确报错而不是截断成一张坏图。
const hubArtifactImageMax = 4 << 20

// hubArtifactImageMIME 图片扩展名 → mime。只认这些扩展名，避免把任意二进制
// 伪装成图片塞进 <img>。
var hubArtifactImageMIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".bmp":  "image/bmp",
}

func (m *manager) hubTaskArtifactPreview(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	out, err := previewTaskArtifact(context.Background(), st,
		str(c.Payload, "task_id"), str(c.Payload, "path"), str(c.Payload, "name"))
	if err != nil {
		m.resp(c, false, "读取任务产物失败: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", out)
}

// previewTaskArtifact 校验并读取一个任务产物。返回值不含绝对路径：
// path 是展示用路径（绝对产物只回基名），content/data_url 是内容本身。
func previewTaskArtifact(ctx context.Context, st agenthub.Store, taskID, rawPath, rawName string) (map[string]any, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: store 为空", agenthub.ErrInvalid)
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("%w: task_id 不能为空", agenthub.ErrInvalid)
	}
	task, err := st.LoadTask(ctx, agenthub.TaskID(taskID))
	if err != nil {
		return nil, fmt.Errorf("%w: 任务 %s 不存在: %v", agenthub.ErrNotFound, taskID, err)
	}
	if task == nil {
		return nil, fmt.Errorf("%w: 任务 %s 不存在", agenthub.ErrNotFound, taskID)
	}
	if task.Result == nil || len(task.Result.Artifacts) == 0 {
		return nil, fmt.Errorf("%w: 任务没有产物", agenthub.ErrNotFound)
	}

	art, err := matchTaskArtifact(task.Result.Artifacts, rawPath, rawName)
	if err != nil {
		return nil, err
	}

	root, err := taskArtifactRoot(task)
	if err != nil {
		return nil, err
	}
	source := strings.TrimSpace(art.Path)
	if source == "" {
		source = strings.TrimSpace(art.Name)
	}
	if source == "" {
		return nil, fmt.Errorf("%w: 产物没有路径", agenthub.ErrInvalid)
	}
	var abs string
	if filepath.IsAbs(source) {
		abs = filepath.Clean(source)
	} else {
		abs = filepath.Clean(filepath.Join(root, source))
	}
	// canonical + hubPathWithin：..、软链、macOS 大小写不敏感都走同一份判定。
	if !hubPathWithin(hubCanonicalPath(abs), hubCanonicalPath(root)) {
		return nil, fmt.Errorf("%w: 产物路径不在任务根目录内", agenthub.ErrInvalid)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法读取产物: %v", agenthub.ErrNotFound, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: 产物不是普通文件", agenthub.ErrInvalid)
	}

	displayPath := strings.TrimSpace(art.Path)
	if displayPath == "" {
		displayPath = strings.TrimSpace(art.Name)
	}
	// 绝对产物路径不回流给前端（可能暴露工作区外目录）。
	if filepath.IsAbs(displayPath) {
		displayPath = filepath.Base(displayPath)
	}
	displayPath = filepath.ToSlash(displayPath)
	name := strings.TrimSpace(art.Name)
	if name == "" {
		name = filepath.Base(abs)
	}
	out := map[string]any{
		"ok":        true,
		"path":      displayPath,
		"name":      name,
		"size":      info.Size(),
		"lines":     0,
		"truncated": false,
		"binary":    false,
	}

	if mime, ok := hubArtifactImageMIME[strings.ToLower(filepath.Ext(source))]; ok {
		if info.Size() > hubArtifactImageMax {
			return nil, fmt.Errorf("%w: 图片超过 %d MiB，无法预览", agenthub.ErrInvalid, hubArtifactImageMax>>20)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("%w: 无法读取产物: %v", agenthub.ErrNotFound, err)
		}
		out["binary"] = true
		out["mime"] = mime
		out["data_url"] = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		return out, nil
	}

	// 文本走既有 readFilePreview：1MiB 截断、行数、二进制判定都是同一份逻辑。
	// 传相对路径而不是绝对路径 —— resolveWorkspacePath 的绝对分支会拒绝
	// ~/.go-code 下的 agent home（hubAgentHome），相对分支只锚定 root。
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return nil, fmt.Errorf("%w: 产物路径不在任务根目录内", agenthub.ErrInvalid)
	}
	content, lines, truncated, binary, rerr := readFilePreview(root, rel)
	if rerr != nil {
		return nil, fmt.Errorf("%w: 无法读取产物: %v", agenthub.ErrNotFound, rerr)
	}
	out["lines"] = lines
	out["truncated"] = truncated
	out["binary"] = binary
	if !binary {
		out["content"] = content
	}
	return out, nil
}

// matchTaskArtifact 在任务结果白名单里定位产物。
//   - path 非空：必须与某一项的 path 精确相等（不按 basename/子串匹配）；
//   - path 为空：按 name 唯一匹配，0 个或多个都拒绝（歧义不猜）。
func matchTaskArtifact(arts []agenthub.TaskArtifact, rawPath, rawName string) (agenthub.TaskArtifact, error) {
	wantPath := strings.TrimSpace(rawPath)
	if wantPath != "" {
		for _, a := range arts {
			if strings.TrimSpace(a.Path) == wantPath {
				return a, nil
			}
		}
		return agenthub.TaskArtifact{}, fmt.Errorf("%w: 路径不在任务结果产物清单中", agenthub.ErrInvalid)
	}
	wantName := strings.TrimSpace(rawName)
	if wantName == "" {
		return agenthub.TaskArtifact{}, fmt.Errorf("%w: path 不能为空", agenthub.ErrInvalid)
	}
	var hit *agenthub.TaskArtifact
	for i := range arts {
		if strings.TrimSpace(arts[i].Name) != wantName {
			continue
		}
		if hit != nil {
			return agenthub.TaskArtifact{}, fmt.Errorf("%w: 同名产物不唯一，请用 path 指定", agenthub.ErrInvalid)
		}
		hit = &arts[i]
	}
	if hit == nil {
		return agenthub.TaskArtifact{}, fmt.Errorf("%w: 名称不在任务结果产物清单中", agenthub.ErrInvalid)
	}
	return *hit, nil
}

// taskArtifactRoot 任务产物的锚定根：
//   - workspace 任务 → Target.Workspace；
//   - agent-target 任务 → 执行者的 hubAgentHome；
//   - 其余（旧任务/人派）→ 发起者的 hubAgentHome。
//
// 没有根一律拒绝：绝不允许相对路径落到 bridge 进程 CWD。
func taskArtifactRoot(task *agenthub.Task) (string, error) {
	if task == nil {
		return "", fmt.Errorf("%w: 任务为空", agenthub.ErrInvalid)
	}
	if ws := strings.TrimSpace(task.Target.Workspace); ws != "" {
		return filepath.Clean(ws), nil
	}
	if id := strings.TrimSpace(string(task.Target.AgentID)); id != "" {
		return hubAgentHome(id), nil
	}
	if id := strings.TrimSpace(string(task.Origin.AgentID)); id != "" {
		return hubAgentHome(id), nil
	}
	return "", fmt.Errorf("%w: 任务没有 workspace 或 agent 归属，无法定位产物", agenthub.ErrInvalid)
}
