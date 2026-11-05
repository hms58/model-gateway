package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

// 业务配置没有任何兜底默认值：flag 与环境变量都为空时必须报错，由入口提示用户。
func TestConfigPathRequiresExplicitValue(t *testing.T) {
	if _, err := ConfigPath("", noEnv); !errors.Is(err, ErrPathRequired) {
		t.Fatalf("期望 ErrPathRequired，实际 %v", err)
	}
}

func TestConfigPathFlagWinsOverEnv(t *testing.T) {
	env := func(key string) string {
		if key == EnvConfig {
			return "/env/conf.json"
		}
		return ""
	}
	if got, err := ConfigPath("/flag/conf.json", env); err != nil || got != "/flag/conf.json" {
		t.Fatalf("flag 应优先于环境变量：%s %v", got, err)
	}
	if got, err := ConfigPath("", env); err != nil || got != "/env/conf.json" {
		t.Fatalf("未给 flag 时应取环境变量：%s %v", got, err)
	}
}

// state 路径允许回落到 <data_dir>/state.json，data_dir 为空时用 ./data。
func TestStatePathFallsBackToDataDir(t *testing.T) {
	if got := StatePath("", "", noEnv); got != filepath.Join("data", "state.json") {
		t.Fatalf("默认 state 路径错误：%s", got)
	}
	if got := StatePath("", "/srv/modelgw", noEnv); got != filepath.Join("/srv/modelgw", "state.json") {
		t.Fatalf("data_dir 未生效：%s", got)
	}
}

func TestStatePathFlagAndEnvWinOverDataDir(t *testing.T) {
	env := func(key string) string {
		if key == EnvState {
			return "/env/state.json"
		}
		return ""
	}
	if got := StatePath("/flag/state.json", "/srv/modelgw", env); got != "/flag/state.json" {
		t.Fatalf("flag 应优先：%s", got)
	}
	if got := StatePath("", "/srv/modelgw", env); got != "/env/state.json" {
		t.Fatalf("环境变量应优先于 data_dir：%s", got)
	}
}

// 管理口令以 AdminToken 为第一优先：启动参数 / 环境变量给的是明文，必须立即转摘要。
func TestAdminTokenSHA256PrefersExplicitToken(t *testing.T) {
	env := func(key string) string {
		if key == EnvAdminToken {
			return "env-secret-token"
		}
		return ""
	}
	configSHA := HashAdminToken("file-secret-token")

	sha, source := AdminTokenSHA256("flag-secret-token", configSHA, env)
	if sha != HashAdminToken("flag-secret-token") {
		t.Fatalf("启动参数应最优先：%s", sha)
	}
	if sha == configSHA || sha == HashAdminToken("env-secret-token") {
		t.Fatalf("不应取到次优先来源：%s", sha)
	}
	if !strings.Contains(source, "admin-token") {
		t.Fatalf("来源说明应指明启动参数：%s", source)
	}

	sha, source = AdminTokenSHA256("", configSHA, env)
	if sha != HashAdminToken("env-secret-token") {
		t.Fatalf("环境变量应优先于配置文件：%s", sha)
	}
	if !strings.Contains(source, EnvAdminToken) {
		t.Fatalf("来源说明应指明环境变量：%s", source)
	}
}

// 前两者都没给时才回落到配置文件里的摘要（已经是摘要，不能再哈希一次）。
func TestAdminTokenSHA256FallsBackToConfigHash(t *testing.T) {
	configSHA := HashAdminToken("file-secret-token")
	sha, source := AdminTokenSHA256("", configSHA, noEnv)
	if sha != configSHA {
		t.Fatalf("应原样使用配置里的摘要：%s", sha)
	}
	if !strings.Contains(source, "admin_token_sha256") {
		t.Fatalf("来源说明应指明配置文件：%s", source)
	}
}

func TestAdminTokenSHA256EmptyWhenNothingSet(t *testing.T) {
	sha, source := AdminTokenSHA256("", "", noEnv)
	if sha != "" || source != "" {
		t.Fatalf("三者都为空时应返回空串，由管理面决定初始化模式：%q %q", sha, source)
	}
}

// 明文口令不得出现在来源说明里（它会被打进启动日志）。
func TestAdminTokenSourceNeverLeaksPlaintext(t *testing.T) {
	const plain = "super-secret-plain-token"
	_, source := AdminTokenSHA256(plain, "", noEnv)
	if strings.Contains(source, plain) {
		t.Fatalf("来源说明泄漏了明文口令：%s", source)
	}
}
