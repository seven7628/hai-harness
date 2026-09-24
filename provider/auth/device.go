package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Device code 轮询（RFC 8628），对齐 pi oauth/device-code.ts pollOAuthDeviceCodeFlow。

const (
	// MinimumPollInterval 最小轮询间隔（pi MINIMUM_INTERVAL_MS = 1000）。
	MinimumPollInterval = time.Second
	// DefaultPollInterval RFC 8628 §3.2：服务端省略 interval 时默认 5 秒。
	DefaultPollInterval = 5 * time.Second
	// SlowDownIncrement RFC 8628 §3.5：slow_down 后间隔 +5 秒。
	SlowDownIncrement = 5 * time.Second
	// DeviceCancelMessage 取消文案。
	DeviceCancelMessage = "Login cancelled"
	// DeviceTimeoutMessage 纯超时文案。
	DeviceTimeoutMessage = "Device flow timed out"
	// DeviceSlowDownTimeoutMessage slow_down 后超时文案（常因 WSL/VM 时钟漂移）。
	DeviceSlowDownTimeoutMessage = "Device flow timed out after one or more slow_down responses. " +
		"This is often caused by clock drift in WSL or VM environments. Please sync or restart the VM clock and try again."
)

// DevicePollResult 单次轮询结果。
type DevicePollResult struct {
	Status string // "complete" | "pending" | "slow_down" | "failed"
	Value  string // complete 时的 token / authorizationCode
	// IntervalSeconds slow_down 时服务端建议的新间隔（0 = 采用客户端递增）。
	IntervalSeconds int
	Message         string // failed 时的错误
}

// DevicePollOptions 轮询配置。
type DevicePollOptions struct {
	// IntervalSeconds 服务端返回的轮询间隔；0 = 默认 5s。
	IntervalSeconds int
	// ExpiresInSeconds 总超时；0 = 不限时。
	ExpiresInSeconds int
	// WaitBeforeFirstPoll 首次轮询前先等一个间隔（GitHub 用）。
	WaitBeforeFirstPoll bool
	// Poll 单次轮询函数。
	Poll func(ctx context.Context) (DevicePollResult, error)
}

// PollDeviceFlow 通用 device code 轮询（对齐 pi pollOAuthDeviceCodeFlow）：
//   - 最小间隔 1s；slow_down 时优先采用服务端 interval，否则间隔 +5s；
//   - expiresIn 超时抛错（区分纯超时 / slow_down 后超时）；
//   - ctx 取消抛 ErrCancelled。
func PollDeviceFlow(ctx context.Context, opts DevicePollOptions) (string, error) {
	deadline := time.Time{}
	if opts.ExpiresInSeconds > 0 {
		deadline = time.Now().Add(time.Duration(opts.ExpiresInSeconds) * time.Second)
	}
	interval := time.Duration(opts.IntervalSeconds) * time.Second
	if interval < MinimumPollInterval {
		interval = DefaultPollInterval
	}
	slowDowns := 0

	sleep := func(d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ErrCancelled
		case <-t.C:
			return nil
		}
	}

	if opts.WaitBeforeFirstPoll {
		if deadline.IsZero() {
			if err := sleep(interval); err != nil {
				return "", err
			}
		} else if remaining := time.Until(deadline); remaining > 0 {
			d := interval
			if remaining < d {
				d = remaining
			}
			if err := sleep(d); err != nil {
				return "", err
			}
		}
	}

	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		if err := ctx.Err(); err != nil {
			return "", ErrCancelled
		}

		res, err := opts.Poll(ctx)
		if err != nil {
			return "", err
		}
		switch res.Status {
		case "complete":
			return res.Value, nil
		case "failed":
			return "", errors.New(res.Message)
		case "slow_down":
			slowDowns++
			if res.IntervalSeconds > 0 {
				// 服务端给出新间隔（GitHub 报告新的最小间隔）→ 信任服务端；
				// 只信客户端计时在 WSL/VM 时钟漂移下会一直提前轮询（pi 注释同款）。
				interval = time.Duration(res.IntervalSeconds) * time.Second
				if interval < MinimumPollInterval {
					interval = MinimumPollInterval
				}
			} else {
				interval += SlowDownIncrement
			}
		}

		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			d := interval
			if remaining < d {
				d = remaining
			}
			if err := sleep(d); err != nil {
				return "", err
			}
		} else if err := sleep(interval); err != nil {
			return "", err
		}
	}

	if slowDowns > 0 {
		return "", errors.New(DeviceSlowDownTimeoutMessage)
	}
	return "", fmt.Errorf("%s", DeviceTimeoutMessage)
}
