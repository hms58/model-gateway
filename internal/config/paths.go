package config

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvConfig / EnvState 是覆盖配置文件与运行态文件位置的环节变量名。
const (
	EnvConfig = "MODELGW_CONFIG"
	EnvState  = "MODELGW_STATE"
	// EnvAdminToken 是启动时以明文给出的管理口令：只在内存里转成摘要，不落盘。
	EnvAdminToken = "MODELGW_ADMIN_TOKEN"
)

// AdminTokenSHA256 解析管理面口令摘要，优先级从高到低：
//
//	命令行 -admin-token  >  环境变量 MODELGW_ADMIN_TOKEN  >  配置文件的 admin_token_sha256
//
// 前两者给的是明文，这里立即转成 SHA-256，明文不进入配置也不进日志；
// source 只用于启动日志说明来源，不包含任何口令内容。三者都没有时返回空串，
// 由管理面决定是否进入「初始化模式」。
func AdminTokenSHA256(flagValue, configSHA string, getenv func(string) string) (sha, source string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if plain := strings.TrimSpace(flagValue); plain != "" {
		return HashAdminToken(plain), "启动参数 -admin-token"
	}
	if plain := strings.TrimSpace(getenv(EnvAdminToken)); plain != "" {
		return HashAdminToken(plain), "环境变量 " + EnvAdminToken
	}
	if stored := strings.TrimSpace(configSHA); stored != "" {
		return stored, "配置文件 admin_token_sha256"
	}
	return "", ""
}

// ConfigPath 返回业务配置文件位置：命令行 flag 优先，其次环境变量。
// 网关不做「无配置」降级，两者都没有就返回 ErrPathRequired。
func ConfigPath(flagValue string, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if path := strings.TrimSpace(flagValue); path != "" {
		return path, nil
	}
	if path := strings.TrimSpace(getenv(EnvConfig)); path != "" {
		return path, nil
	}
	return "", ErrPathRequired
}

// StatePath 返回运行态文件位置：命令行 flag 优先，其次环境变量，最后落到
// 配置里的 data_dir（留空按 ./data）。state.json 允许首次启动不存在。
func StatePath(flagValue, dataDir string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if path := strings.TrimSpace(flagValue); path != "" {
		return path
	}
	if path := strings.TrimSpace(getenv(EnvState)); path != "" {
		return path
	}
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		dir = "data"
	}
	return filepath.Join(dir, "state.json")
}
