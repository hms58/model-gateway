package token

import (
	"errors"
	"strings"
	"testing"

	"model-gateway/internal/config"
)

func TestGenerateAndVerify(t *testing.T) {
	plain, prefix, sha, err := Generate()
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if !strings.HasPrefix(plain, Prefix) {
		t.Fatalf("明文应以 %s 开头：%s", Prefix, plain)
	}
	if len(prefix) != 8 || !strings.HasPrefix(plain, prefix) {
		t.Fatalf("前缀应为前 8 位：%s / %s", prefix, plain)
	}
	record := config.Token{ID: "tk_01", Enabled: true, KeyPrefix: prefix, KeySHA256: sha}
	if !Verify(record, plain) {
		t.Fatal("正确 key 应当通过校验")
	}
	if Verify(record, plain+"x") {
		t.Fatal("错误 key 不应通过校验")
	}
}

func TestVerifyRejectsDisabledAndForeignPrefix(t *testing.T) {
	plain, prefix, sha, err := Generate()
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	disabled := config.Token{ID: "tk_01", Enabled: false, KeyPrefix: prefix, KeySHA256: sha}
	if Verify(disabled, plain) {
		t.Fatal("禁用 Token 不应通过")
	}
	foreign := config.Token{ID: "tk_02", Enabled: true, KeyPrefix: prefix, KeySHA256: sha}
	if Verify(foreign, "sk-"+strings.TrimPrefix(plain, Prefix)) {
		t.Fatal("非 mg- 前缀不应通过")
	}
}

func TestAllowedGroups(t *testing.T) {
	record := config.Token{Enabled: true, Groups: []string{"gpt-5"}}
	if !Allowed(record, "gpt-5") {
		t.Fatal("组权限内应放行")
	}
	if Allowed(record, "claude") {
		t.Fatal("组权限外应拒绝")
	}
	wildcard := config.Token{Enabled: true, Groups: []string{"*"}}
	if !Allowed(wildcard, "任意组") {
		t.Fatal("通配应当放行")
	}
	wildcard.Enabled = false
	if Allowed(wildcard, "gpt-5") {
		t.Fatal("禁用 Token 不应放行")
	}
}

func TestFindReturnsErrInvalidKey(t *testing.T) {
	if _, err := Find(nil, "mg-xxx"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("期望 ErrInvalidKey，实际 %v", err)
	}
}
