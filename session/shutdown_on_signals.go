package session

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ShutdownOnSignals 注册信号监听：任一信号触发时调用 s.Shutdown 优雅收尾
// （Abort 当前轮 + 等待状态落盘），内部以 30s 超时兜底（防非协作工具挂死）。
// 返回 stop 解除监听（幂等）；可多次调用（每次注册自己的监听）。
//
// SDK 是纯库、不自动监听任何信号：宿主显式接入（推荐主程序一行：
//
//	defer session.ShutdownOnSignals(s)()
//
// 需要精细控制（自定义优雅窗口、联动关闭 store）时，用标准库直接驱动：
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	// Run 在其他 goroutine；信号到来时：
//	graceCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	if err := s.Shutdown(graceCtx); err != nil { /* 超时，强制退出 */ }
func ShutdownOnSignals(s *Session, sig ...os.Signal) (stop func()) {
	if len(sig) == 0 {
		sig = []os.Signal{os.Interrupt, syscall.SIGTERM}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), sig...)
	go func() {
		<-ctx.Done()
		graceCtx, graceCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer graceCancel()
		_ = s.Shutdown(graceCtx)
	}()
	return cancel
}
