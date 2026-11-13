package agenthub

// store_profile_versions.go：Profile 版本留档与删除。
//
// 方案 §5.1：每次保存 `version++`，旧版本追加到 `agents/<id>/profile_versions.jsonl`
//（保留最近 MaxProfileVersions 版），用于「人格改坏了回滚」。群组与任务只引用
// `agent_id`，不引用版本 —— 版本只影响「这个智能体下一刻长什么样」。
//
// 删除分两档（§5.2）：软删 = `Deleted=true` 的普通保存（DM/群记录与记忆都保留）；
// `purge=true` 才走 DeleteAgent 物理删目录。两者都不在这里做「停会话/取消任务」——
// 那是 bridge 的运行态收尾（运行时关心会话，领域包不该知道 session）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MaxProfileVersions 每个智能体保留的历史版本数（超出丢最旧）。
const MaxProfileVersions = 50

// ProfileVersion 一次保存前的 Profile 快照。
type ProfileVersion struct {
	Version int           `json:"version"`
	SavedAt time.Time     `json:"saved_at"`
	Profile *AgentProfile `json:"profile"`
}

func (s *FileStore) profileVersionsPath(id AgentID) string {
	return filepath.Join(s.root, "agents", string(id), "profile_versions.jsonl")
}

// AppendProfileVersion 追加一份历史快照，并把文件裁剪到最近 MaxProfileVersions 行。
//
// 为什么整文件重写而不是就地裁剪：jsonl 只能追加，裁剪必须在行边界上做；
// 这份文件很小（≤50 × 一个 profile），重写一次比维护行索引简单且不会撕裂。
func (s *FileStore) AppendProfileVersion(ctx context.Context, p *AgentProfile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || !validID(string(p.ID)) {
		return fmt.Errorf("%w: profile version", ErrInvalid)
	}
	// 深拷贝（不是 `*p`）：切片的底层数组会共享，调用方随后的 patch 会写进归档。
	snapshot := CloneAgentProfile(p)
	v := ProfileVersion{Version: snapshot.Version, SavedAt: time.Now().UTC(), Profile: snapshot}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.profileVersionsPath(p.ID)
	versions, err := s.readProfileVersionsLocked(path)
	if err != nil {
		return err
	}
	// 幂等：上一次保存失败（rename 失败/掉电）会留下「当前版本已归档」的状态，
	// 重试时再追加一次就会出现两行同一个版本号（P1-06 复核 N4）。
	// 版本号单调、归档按保存前的版本追加，因此「最后一行同版本」= 已经归档过。
	if n := len(versions); n > 0 && versions[n-1].Version == v.Version {
		return nil
	}
	versions = append(versions, v)
	if len(versions) > MaxProfileVersions {
		versions = versions[len(versions)-MaxProfileVersions:]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".versions-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	for _, item := range versions {
		data, err := json.Marshal(item)
		if err != nil {
			cleanup()
			return err
		}
		if _, err := tmp.Write(append(data, '\n')); err != nil {
			cleanup()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ListProfileVersions 返回历史快照（最新在后）。limit<=0 = 全部（最多 MaxProfileVersions）。
func (s *FileStore) ListProfileVersions(ctx context.Context, id AgentID, limit int) ([]ProfileVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(string(id)) {
		return nil, fmt.Errorf("%w: agent id %q", ErrInvalid, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	versions, err := s.readProfileVersionsLocked(s.profileVersionsPath(id))
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(versions) > limit {
		versions = versions[len(versions)-limit:]
	}
	return versions, nil
}

// readProfileVersionsLocked 读 jsonl；文件不存在 = 空历史（不是错误：
// 从没改过人格的智能体只有当前版本）。坏行跳过 —— 归档文件不能把主流程带死。
func (s *FileStore) readProfileVersionsLocked(path string) ([]ProfileVersion, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]ProfileVersion, 0, 8)
	start := 0
	for i := 0; i <= len(data); i++ {
		if i < len(data) && data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var v ProfileVersion
		if json.Unmarshal(line, &v) != nil || v.Profile == nil {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// DeleteAgent 物理删除智能体目录（profile / 历史版本 / DM 记录）。
// 只有 `purge=true` 才调；软删是 SavedAgent(Deleted=true)。
//
// 幂等：目录不存在 = 成功（UI 连点两次删除不该报错）。
func (s *FileStore) DeleteAgent(ctx context.Context, id AgentID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(string(id)) {
		return fmt.Errorf("%w: agent id %q", ErrInvalid, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(filepath.Dir(s.agentPath(id))); err != nil {
		return err
	}
	return nil
}
