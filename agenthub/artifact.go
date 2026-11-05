package agenthub

// artifact.go：产物版本化（P3a-07，§6.8）。
//
// 版本键是 (owner_agent_id, name)：同一智能体同名产物递增版本；不同智能体同名互不影响。
// 预览按 artifact id 寻址（不是路径），避免路径穿越与「同名被后写覆盖」。
// 索引扫描由调用方提供（FileStore.ListArtifacts）；本文件只做确定性版本计算与路径校验。

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// NextArtifactVersion 计算 (owner,name) 的下一个版本号（现有最大 +1；无则 1）。
func NextArtifactVersion(existing []*Artifact, owner AgentID, name string) int {
	name = strings.TrimSpace(name)
	max := 0
	for _, a := range existing {
		if a == nil || a.OwnerAgentID != owner || !strings.EqualFold(strings.TrimSpace(a.Name), name) {
			continue
		}
		if a.Version > max {
			max = a.Version
		}
	}
	return max + 1
}

// ValidateArtifactPath 拒绝空/绝对/包含 .. 的路径（产物路径是相对群工作区/产物的）。
func ValidateArtifactPath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("%w: artifact.path 不能为空", ErrInvalid)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%w: artifact.path 不能是绝对路径", ErrInvalid)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: artifact.path 不能越出根目录", ErrInvalid)
	}
	return nil
}

// NewArtifactVersion 用现有产物计算并填充版本字段（调用方负责落盘）。
func NewArtifactVersion(existing []*Artifact, a *Artifact) error {
	if a == nil {
		return fmt.Errorf("%w: artifact is nil", ErrInvalid)
	}
	if err := ValidateArtifactPath(a.Path); err != nil {
		return err
	}
	if strings.TrimSpace(a.Name) == "" {
		a.Name = filepath.Base(strings.TrimSpace(a.Path))
	}
	a.Visibility = a.Visibility.EffectiveVisibility()
	if err := a.Visibility.Validate(); err != nil {
		return err
	}
	a.Version = NextArtifactVersion(existing, a.OwnerAgentID, a.Name)
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	return nil
}

// ArtifactVisibleTo 预览/读取鉴权：owner 仅人类与产物 owner 可见；group 所有成员可见。
func ArtifactVisibleTo(a *Artifact, viewer AgentID, isHuman bool) bool {
	if a == nil {
		return false
	}
	if a.Visibility.EffectiveVisibility() == VisibilityGroup {
		return true
	}
	return isHuman || viewer == a.OwnerAgentID
}
