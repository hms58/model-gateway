package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"

	"model-gateway/internal/config"
)

// Prefix 是对外虚拟 key 的固定前缀，用于区分下游真实 key。
const Prefix = "mg-"

var ErrInvalidKey = errors.New("虚拟 Token 无效")

// Generate 返回明文、展示前缀与 SHA-256；明文只在创建时出现一次。
func Generate() (plain, prefix, sha string, err error) {
	buf := make([]byte, 24)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	plain = Prefix + hex.EncodeToString(buf)
	return plain, PrefixOf(plain), Hash(plain), nil
}

func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// PrefixOf 取前 8 位用于列表展示，不泄漏完整 key。
func PrefixOf(plain string) string {
	if len(plain) <= 8 {
		return plain
	}
	return plain[:8]
}

// Verify 用常数时间比对哈希，避免逐位比较泄漏信息。
func Verify(record config.Token, plain string) bool {
	if !record.Enabled || plain == "" || record.KeySHA256 == "" {
		return false
	}
	if !strings.HasPrefix(plain, Prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(Hash(plain)), []byte(record.KeySHA256)) == 1
}

// Allowed 判断 Token 是否可访问指定模型组；`*` 表示全组。
func Allowed(record config.Token, group string) bool {
	if !record.Enabled {
		return false
	}
	for _, allowed := range record.Groups {
		if allowed == "*" || allowed == group {
			return true
		}
	}
	return false
}

// Find 按明文匹配启用中的 Token 记录。
func Find(tokens []config.Token, plain string) (config.Token, error) {
	for _, record := range tokens {
		if Verify(record, plain) {
			return record, nil
		}
	}
	return config.Token{}, ErrInvalidKey
}
