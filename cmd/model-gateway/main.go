// Command model-gateway 是多产品模型网关：把多个下游 OpenAI 兼容实例收敛成
// 一个入口，按权重降级、软冷却、熔断半开，并提供本地管理面板。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"model-gateway/internal/config"
	"model-gateway/internal/panel"
	"model-gateway/internal/pkg/dataplane"
	"model-gateway/internal/pkg/prober"
	"model-gateway/internal/pkg/reqlog"
	"model-gateway/internal/pkg/store"
	"model-gateway/internal/pkg/usage"
	"model-gateway/internal/version"
)

func main() {
	if err := run(); err != nil {
		log.Printf("model-gateway 退出：%v", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("c", "", "业务配置文件（JSON）路径，必填；也可用环境变量 MODELGW_CONFIG")
	statePath := flag.String("state", "", "运行态文件路径；留空则用环境变量 MODELGW_STATE 或 <data_dir>/state.json")
	adminToken := flag.String("admin-token", "",
		"管理面口令（明文，仅本进程内存使用，不落盘）；留空则用环境变量 "+
			config.EnvAdminToken+" 或配置文件的 admin_token_sha256")
	flag.Parse()

	resolved, err := config.ConfigPath(*cfgPath, nil)
	if err != nil {
		flag.Usage()
		return err
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		return fmt.Errorf("加载业务配置 %s 失败：%w", resolved, err)
	}

	state := config.StatePath(*statePath, cfg.DataDir, nil)
	st, err := store.Open(resolved, state)
	if err != nil {
		return fmt.Errorf("初始化运行态失败：%w", err)
	}

	snapshot := st.Snapshot()
	logger := reqlog.New(snapshot.Logging.Dir, snapshot.Logging.RetentionDays)
	defer logger.Close()

	// 用量累计器独立于请求日志：日志受保留期限制会删文件，这里折叠后长期保留。
	usageRecorder := usage.New(filepath.Join(snapshot.DataDir, "usage.json"))
	usageRecorder.Start()
	defer usageRecorder.Stop()

	stop := make(chan struct{})
	go st.RunStateFlusher(stop, 5*time.Second)
	go logger.RunCleanup(stop, 24*time.Hour)
	go prober.New(st).Run(stop)

	// 管理口令优先级：-admin-token > MODELGW_ADMIN_TOKEN > 配置文件的 admin_token_sha256。
	// 前两者是明文入参，这里立刻转摘要，明文不进配置、不进日志。
	adminSHA, adminSource := config.AdminTokenSHA256(*adminToken, snapshot.AdminTokenSHA256, nil)

	gateway := dataplane.New(st).WithLogger(logger).WithUsage(usageRecorder)
	management := panel.New(st, adminSHA).
		WithLogger(logger).
		WithLogs(snapshot.Logging.Dir).
		WithUsage(usageRecorder).
		WithModelsPath(snapshot.DataDir).
		Handler()

	server := &http.Server{
		Addr:              snapshot.Listen,
		Handler:           routes(gateway, management),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 退出前把运行态与日志刷盘：冷却 / 熔断依赖 state.json 恢复。
	defer func() {
		_ = st.FlushIfDirty()
		close(stop)
		logger.Flush()
	}()

	listener, err := net.Listen("tcp", snapshot.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败：%w", snapshot.Listen, err)
	}
	if strings.TrimSpace(adminSHA) == "" {
		log.Printf("警告：未设置管理口令，管理面进入初始化模式——免登录但仅允许本机访问；" +
			"请打开 http://127.0.0.1" + snapshot.Listen + "/panel/settings 设置口令后重启")
	} else {
		log.Printf("管理面口令来源：%s", adminSource)
	}
	log.Printf(
		"model-gateway %s 启动：listen=%s config=%s 产品=%d 模型组=%d Token=%d dataDir=%s 日志=%s 用量=%s",
		version.Version, snapshot.Listen, resolved, len(snapshot.Products), len(snapshot.ModelGroups),
		len(snapshot.Tokens), snapshot.DataDir, snapshot.Logging.Dir, usageRecorder.Describe())

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-quit:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(ctx)
}
