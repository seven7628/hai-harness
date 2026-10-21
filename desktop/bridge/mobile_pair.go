// mobile_pair.go —— 手机配对与设备凭据。
//
// 语义（TECH_PLAN §5）：
//   - 配对码 6 位数字、5 分钟、单次使用、每分钟最多 5 次尝试（防在线爆破）；
//   - 设备 token 32 字节随机，落盘只存 sha256（泄露文件不等于泄露 token）；
//   - 每台设备独立撤销；设备清单 0600 原子写。
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	mobileCodeTTL      = 5 * time.Minute
	mobileCodeAttempts = 5
	mobileCodeWindow   = time.Minute
	mobileTokenBytes   = 32
)

var (
	errMobileCodeInvalid = errors.New("配对码错误或已过期")
	errMobileCodeUsed    = errors.New("配对码已使用，请在桌面端重新生成")
	errMobileRateLimited = errors.New("尝试过于频繁，请稍后再试")
)

type mobileDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	TokenHash string `json:"token_hash"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen,omitempty"`
}

type mobilePairing struct {
	mu   sync.Mutex
	path string // devices.json

	code     string
	codeExp  time.Time
	attempts []time.Time

	devices map[string]mobileDevice
}

func newMobilePairing(path string) *mobilePairing {
	p := &mobilePairing{path: path, devices: map[string]mobileDevice{}}
	p.load()
	return p
}

func (p *mobilePairing) load() {
	b, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var doc struct {
		Devices []mobileDevice `json:"devices"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return
	}
	for _, d := range doc.Devices {
		if d.ID != "" && d.TokenHash != "" {
			p.devices[d.ID] = d
		}
	}
}

func (p *mobilePairing) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	list := make([]mobileDevice, 0, len(p.devices))
	for _, d := range p.devices {
		list = append(list, d)
	}
	b, err := json.MarshalIndent(map[string]any{"devices": list}, "", "  ")
	if err != nil {
		return err
	}
	return writeMobileAtomic(p.path, b)
}

// newCode 生成（或复用未过期的）配对码。桌面端设置页每次打开都调它，不能把码刷掉。
func (p *mobilePairing) newCode(now time.Time) (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code != "" && now.Before(p.codeExp) {
		return p.code, p.codeExp
	}
	p.code = randomDigits(6)
	p.codeExp = now.Add(mobileCodeTTL)
	p.attempts = nil
	return p.code, p.codeExp
}

// currentCode 只读当前码（不生成）。
func (p *mobilePairing) currentCode(now time.Time) (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code != "" && now.Before(p.codeExp) {
		return p.code, p.codeExp
	}
	return "", time.Time{}
}

func (p *mobilePairing) pair(code, name string, now time.Time) (token, deviceID string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 限流先于比对：否则爆破者能用响应差异区分「码错」与「限流」。
	cut := now.Add(-mobileCodeWindow)
	kept := p.attempts[:0]
	for _, t := range p.attempts {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	p.attempts = kept
	if len(p.attempts) >= mobileCodeAttempts {
		return "", "", errMobileRateLimited
	}
	p.attempts = append(p.attempts, now)

	if p.code == "" || now.After(p.codeExp) {
		return "", "", errMobileCodeInvalid
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) != 1 {
		return "", "", errMobileCodeInvalid
	}
	// 单次使用：消费掉，必须回桌面重新出码。
	p.code = ""
	p.codeExp = time.Time{}

	raw := make([]byte, mobileTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(raw)
	deviceID = "dev_" + hex.EncodeToString(raw[:8])
	if name == "" {
		name = "手机"
	}
	p.devices[deviceID] = mobileDevice{
		ID:        deviceID,
		Name:      name,
		TokenHash: hashMobileToken(token),
		CreatedAt: now.Unix(),
		LastSeen:  now.Unix(),
	}
	if err := p.saveLocked(); err != nil {
		return "", "", err
	}
	return token, deviceID, nil
}

// validate 校验 Bearer token，命中则返回设备 id。
func (p *mobilePairing) validate(token string, now time.Time) (string, bool) {
	if token == "" {
		return "", false
	}
	h := hashMobileToken(token)
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, d := range p.devices {
		if subtle.ConstantTimeCompare([]byte(h), []byte(d.TokenHash)) == 1 {
			// 只更新内存；落盘留给配对的低频路径（避免每条请求一次 fsync）。
			d.LastSeen = now.Unix()
			p.devices[id] = d
			return id, true
		}
	}
	return "", false
}

func (p *mobilePairing) revoke(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.devices[id]; !ok {
		return false
	}
	delete(p.devices, id)
	_ = p.saveLocked()
	return true
}

func (p *mobilePairing) list() []mobileDevice {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]mobileDevice, 0, len(p.devices))
	for _, d := range p.devices {
		out = append(out, d)
	}
	return out
}

func hashMobileToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randomDigits 生成 n 位十进制码（crypto/rand；拒绝采样避免模偏置）。
func randomDigits(n int) string {
	buf := make([]byte, n)
	out := make([]byte, n)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			// 熵源不可用是系统级故障；退回时间派生的码也比空串好（配对仍需桌面端在场）。
			return fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
		}
		if buf[i] < 250 { // 250 = 25*10，取模无偏
			out[i] = '0' + buf[i]%10
			i++
		}
	}
	return string(out)
}

// writeMobileAtomic 临时文件 + rename（0600）。settings.json 半写丢过键，这里不重蹈。
func writeMobileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mobile-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
