package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// GeneratePKCE 生成 RFC 7636 PKCE 对（对齐 pi oauth/pkce.ts）：
//   - verifier：32 字节随机数 base64url（43 字符，符合 43-128 字符要求）；
//   - challenge：verifier 的 SHA-256 后 base64url（S256 方法）。
func GeneratePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}
