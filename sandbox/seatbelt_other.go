//go:build !darwin

package sandbox

import (
	"context"
	"errors"
)

// errSeatbeltUnavailable Seatbelt 仅在 macOS 可用（Linux 计划用 bwrap，见 ROADMAP）。
var errSeatbeltUnavailable = errors.New("sandbox: Seatbelt 沙箱仅 macOS 可用")

// Available 非 darwin 恒 false。
func Available() bool { return false }

// NewSeatbelt 非 darwin 不可用（返回错误，装配方据此降级 NoSandbox）。
func NewSeatbelt(workspace string, opts ...SeatbeltOption) (*Seatbelt, error) {
	return nil, errSeatbeltUnavailable
}

// Run 防御性实现（构造不会成功，正常不可达）。
func (s *Seatbelt) Run(ctx context.Context, spec ExecSpec) (string, error) {
	return "", errSeatbeltUnavailable
}
