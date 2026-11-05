package config

import (
	"errors"
	"testing"
)

func TestHashAdminTokenIsStableSHA256(t *testing.T) {
	const plain = "change-me"
	const want = "e2186dbdb1bb4193608605e84f33208765b5693b55edd4f730a719a100eeea6f"
	if got := HashAdminToken(plain); got != want {
		t.Fatalf("SHA-256 不正确：%s", got)
	}
	if got := HashAdminToken("  " + plain + "  "); got != want {
		t.Fatalf("两端空白应被裁剪：%s", got)
	}
}

func TestMatchAdminToken(t *testing.T) {
	sha := HashAdminToken("change-me")
	if !MatchAdminToken("change-me", sha) {
		t.Fatal("正确口令应匹配")
	}
	if MatchAdminToken("wrong", sha) {
		t.Fatal("错误口令不应匹配")
	}
	if MatchAdminToken("", sha) || MatchAdminToken("change-me", "") {
		t.Fatal("空口令或空摘要一律不匹配")
	}
}

// 明文口令不允许写盘：只要配置里出现 admin_token 就整份拒绝。
func TestValidateRejectsPlainAdminToken(t *testing.T) {
	cfg := validConfig()
	cfg.AdminToken = "plain-secret"
	if err := Validate(&cfg); !errors.Is(err, ErrPlainAdminToken) {
		t.Fatalf("期望 ErrPlainAdminToken，实际 %v", err)
	}
}

func TestValidateRejectsMalformedHash(t *testing.T) {
	cfg := validConfig()
	cfg.AdminTokenSHA256 = "not-a-sha256"
	if err := Validate(&cfg); err == nil {
		t.Fatal("非法摘要应当被拒")
	}

	cfg.AdminTokenSHA256 = HashAdminToken("ok")
	if err := Validate(&cfg); err != nil {
		t.Fatalf("合法摘要应通过：%v", err)
	}
}

// 默认配置不带任何口令：管理面在配置页设好口令前不可用。
func TestDefaultHasNoAdminCredential(t *testing.T) {
	cfg := Default()
	if cfg.AdminToken != "" || cfg.AdminTokenSHA256 != "" {
		t.Fatalf("默认配置不应带凭据：%+v", cfg)
	}
}
