// Package version 暴露编译期注入的版本信息。
//
// 默认值 dev：本地 go build / go test 都不带版本号，面板显示 dev 便于区分。
// 发布构建用 ldflags 注入，形如 v2026.11.6.1207（上海时间 + 时分）：
//
//	go build -ldflags "-X model-gateway/internal/version.Version=v2026.11.6.1207"
package version

// Version 是构建版本号，由发布脚本通过 ldflags 覆盖。
var Version = "dev"
