package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrPlainAdminToken 表示配置里仍在用明文字段 admin_token。
var ErrPlainAdminToken = errors.New("admin_token 不允许明文：请在配置页设置口令（只保存 SHA-256），或改用 admin_token_sha256")

// HashAdminToken 返回管理面口令的 SHA-256（小写十六进制）。
func HashAdminToken(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// MatchAdminToken 用常数时间比对，避免逐位比较泄漏信息。
func MatchAdminToken(plain, expectedSHA256 string) bool {
	expected := strings.TrimSpace(expectedSHA256)
	if plain == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashAdminToken(plain)), []byte(expected)) == 1
}

// validSHA256 判断字符串是否是合法的 64 位小写十六进制摘要。
func validSHA256(raw string) bool {
	raw = strings.TrimSpace(raw)
	if len(raw) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil
}
