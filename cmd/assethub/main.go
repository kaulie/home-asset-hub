// Command assethub：家庭资源托管服务器（home-asset-hub v1）。
//
// 第一步先把**小米电视/小度依赖的那条链路**接管过来：电视/音箱按
// `http://<mac-lan-ip>:8080/<key>` 拉字节，Edge 按 `POST /api/v1/photos/upload` 上传 ——
// 本服务保持同一组 URL 与响应字段，因此换实现不需要改 Edge/Brain/iOS 的任何配置。
//
// 环境变量（新名优先，兼容现网 img-server 的老名）：
//
//	ASSET_HUB_HOST / PHOTO_UPLOAD_HOST     监听地址，默认 0.0.0.0
//	ASSET_HUB_PORT / PHOTO_UPLOAD_PORT     监听端口，默认 8080（电视/小度 URL 里的那个口）
//	ASSET_HUB_DIR  / PHOTO_UPLOAD_DIR      资源目录，默认 <runtime>/backend/data/img
//	ASSET_HUB_PUBLIC_BASE / PHOTO_PUBLIC_BASE  对外地址（上传响应里的 url），默认按 LAN IP 探测
//	ASSET_HUB_MDNS / MAC_EDGE_IMG_MDNS     =0 关闭 `_ha-img-server._tcp` 广告
//
// 端口说明（重要）：本服务的**地址契约固定 8080**（电视/小度/Edge/Brain 的 URL 都钉在它上面），
// 所以 start.sh 刻意忽略平台注入的 SERVICE_PORT。为了让「平台登记填了别的口」也能通过健康检查，
// 可以把那个口开成附加监听（只绑 loopback）：
//
//	ASSET_HUB_EXTRA_PORTS   逗号分隔的附加端口（如 4236,9000）；off/-/none=不加；空=不加
//	ASSET_HUB_EXTRA_HOST    附加监听的绑定地址，默认 127.0.0.1（不对外）
//
// 资源管理 v2（默认值都按「不打扰现网」选，见 README）：
//
//	ASSET_HUB_DEDUPE              =1 上传同 sha256 复用已存在 key（默认 1；=0 关闭）
//	ASSET_HUB_BACKUP              =0 关闭定时快照（默认 1）
//	ASSET_HUB_BACKUP_DIR          快照根目录，默认 <资源目录>/../backup
//	ASSET_HUB_BACKUP_INTERVAL     快照间隔（24h / 30m / 秒数），默认 24h
//	ASSET_HUB_BACKUP_KEEP         保留几份快照，默认 7
//	ASSET_HUB_INDEX_BUILD_ON_START =0 关闭启动补建 sha256 索引（默认 1）
//	ASSET_HUB_RETENTION_DAYS      >0 才开保留策略（真删过期资源），默认 0=关
//	ASSET_HUB_RETENTION_INTERVAL  保留策略间隔，默认 24h
//	ASSET_HUB_RETENTION_MAX       单次最多删多少个，默认 200
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kaulie/home-asset-hub/internal/httpapi"
	"github.com/kaulie/home-asset-hub/internal/jobs"
	"github.com/kaulie/home-asset-hub/internal/mdns"
	"github.com/kaulie/home-asset-hub/internal/store"
)

var version = "dev" // -ldflags -X main.version=<hash>

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config invalid", "err", err)
		os.Exit(2)
	}
	if err := os.MkdirAll(cfg.dir, 0o755); err != nil {
		logger.Error("cannot create asset dir", "dir", cfg.dir, "err", err)
		os.Exit(2)
	}

	st, err := store.New(cfg.dir)
	if err != nil {
		logger.Error("store init failed", "err", err)
		os.Exit(2)
	}
	publicBase := cfg.publicBase
	if publicBase == "" {
		publicBase = fmt.Sprintf("http://%s:%d", detectLANIPv4(), cfg.port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 后台作业：快照备份 / 启动补索引 / （可选）保留策略
	runner := jobs.New(st, jobs.Config{
		BackupEnabled:      cfg.backup,
		BackupDir:          cfg.backupDir,
		BackupInterval:     cfg.backupInterval,
		BackupKeep:         cfg.backupKeep,
		RetentionEnabled:   cfg.retentionDays > 0,
		RetentionDays:      cfg.retentionDays,
		RetentionInterval:  cfg.retentionInterval,
		RetentionMaxPerRun: cfg.retentionMax,
		IndexBuildOnStart:  cfg.indexBuildOnStart,
	}, logger)
	runner.Start(ctx)

	// 监听地址：第 0 个是契约口（电视/小度/Edge/Brain 用的那个），后面是附加的健康检查口。
	listeners := []string{net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))}
	for _, p := range cfg.extraPorts {
		listeners = append(listeners, net.JoinHostPort(cfg.extraHost, strconv.Itoa(p)))
	}

	handler := httpapi.NewWithOptions(st, httpapi.Options{
		PublicBase: publicBase,
		Dedupe:     cfg.dedupe,
		Jobs:       runner,
		Listeners:  listeners,
	}, logger).Handler()

	srv := &http.Server{
		Addr:              listeners[0],
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	var extraServers []*http.Server

	pub, err := mdns.Publish(mdns.Options{
		Enabled:     cfg.mdns,
		ServiceType: mdns.DefaultServiceType,
		Instance:    mdns.DefaultInstance,
		Hostname:    mdns.DefaultHostname,
		Port:        cfg.port, // mDNS 只广告契约口：iOS 靠它发现「电视那条链路」的地址
	}, logger)
	if err != nil {
		logger.Warn("mdns publish failed", "err", err)
	}
	defer pub.Close()

	// 附加监听：同一个 handler，只绑 loopback —— 平台登记填了别的口也能健康检查通过，
	// 而外部消费方依赖的契约口一动不动（附加口不对外，也不进 mDNS）。
	for _, p := range cfg.extraPorts {
		extra := &http.Server{
			Addr:              net.JoinHostPort(cfg.extraHost, strconv.Itoa(p)),
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		extraServers = append(extraServers, extra)
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// 附加口只是「健康检查有位子」，绑不上不至于让主服务起不来
				logger.Warn("extra listener stopped", "addr", s.Addr, "err", err)
			}
		}(extra)
		logger.Info("extra listener started", "addr", extra.Addr,
			"why", "健康检查口（只绑 loopback，不对外；契约口仍是"+listeners[0]+"）")
	}

	logger.Info("home-asset-hub starting",
		"version", version,
		"addr", srv.Addr,
		"listeners", listeners,
		"dir", st.Dir(),
		"public_base", publicBase,
		"dedupe", cfg.dedupe,
		"backup", cfg.backup,
		"backup_dir", cfg.backupDir,
		"backup_interval", cfg.backupInterval.String(),
		"backup_keep", cfg.backupKeep,
		"retention_days", cfg.retentionDays,
		"index", st.IndexPath(),
		"health", fmt.Sprintf("http://127.0.0.1:%d/health", cfg.port),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("serve failed", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown failed", "err", err)
	}
	for _, extra := range extraServers {
		_ = extra.Shutdown(shutdownCtx)
	}
	logger.Info("home-asset-hub stopped")
}

type config struct {
	host       string
	port       int
	dir        string
	publicBase string
	mdns       bool

	dedupe            bool
	backup            bool
	backupDir         string
	backupInterval    time.Duration
	backupKeep        int
	indexBuildOnStart bool

	// extraPorts 附加监听：契约口（8080）之外的「健康检查口」。
	// 平台登记的服务端口会被注入成 SERVICE_PORT，start.sh 把它转成 ASSET_HUB_EXTRA_PORTS
	// （只绑 extraHost=127.0.0.1）—— 这样平台健康检查填哪个口都有位子应答，
	// 而电视/小度/Edge 依赖的 8080 契约一动不动。
	extraPorts []int
	extraHost  string

	retentionDays     int
	retentionInterval time.Duration
	retentionMax      int
}

func loadConfig() (config, error) {
	// 默认目录：<runtime>/backend/data/img —— 与平台部署保留位一致（backend/）。
	// 生产实际用 ASSET_HUB_DIR 指到代码与 runtime 之外（见 scripts/start.sh 与 .env）。
	defaultDir := filepath.Join("data", "img")
	host := envFirst("ASSET_HUB_HOST", "PHOTO_UPLOAD_HOST", "0.0.0.0")
	portRaw := envFirst("ASSET_HUB_PORT", "PHOTO_UPLOAD_PORT", "8080")
	dir := envFirst("ASSET_HUB_DIR", "PHOTO_UPLOAD_DIR", defaultDir)
	publicBase := envFirst("ASSET_HUB_PUBLIC_BASE", "PHOTO_PUBLIC_BASE", "")
	mdnsRaw := envFirst("ASSET_HUB_MDNS", "MAC_EDGE_IMG_MDNS", "1")

	port, err := strconv.Atoi(portRaw)
	if err != nil || port <= 0 || port > 65535 {
		return config{}, fmt.Errorf("invalid port %q", portRaw)
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return config{}, errors.New("asset dir required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return config{}, err
	}

	// 备份目录默认落在资源目录的兄弟位（<dir>/../backup）：不配置也能用，
	// 且与资源目录同盘 —— 目标是「误删/误覆盖能回滚」，异地容灾不在本版范围。
	backupDir := strings.TrimSpace(envFirst("ASSET_HUB_BACKUP_DIR", "", ""))
	if backupDir == "" {
		backupDir = filepath.Join(filepath.Dir(abs), "backup")
	} else if backupDir, err = filepath.Abs(backupDir); err != nil {
		return config{}, err
	}
	if backupDir == abs {
		return config{}, errors.New("backup dir 不能等于资源目录")
	}

	extraPorts, err := parseExtraPorts(envFirst("ASSET_HUB_EXTRA_PORTS", "", ""), port)
	if err != nil {
		return config{}, err
	}

	backupInterval, err := envDuration([]string{"ASSET_HUB_BACKUP_INTERVAL"}, 24*time.Hour)
	if err != nil {
		return config{}, err
	}
	retentionInterval, err := envDuration([]string{"ASSET_HUB_RETENTION_INTERVAL"}, 24*time.Hour)
	if err != nil {
		return config{}, err
	}
	backupKeep, err := envInt([]string{"ASSET_HUB_BACKUP_KEEP"}, 7)
	if err != nil {
		return config{}, err
	}
	retentionDays, err := envInt([]string{"ASSET_HUB_RETENTION_DAYS"}, 0)
	if err != nil {
		return config{}, err
	}
	retentionMax, err := envInt([]string{"ASSET_HUB_RETENTION_MAX"}, 200)
	if err != nil {
		return config{}, err
	}

	return config{
		host:              strings.TrimSpace(host),
		port:              port,
		dir:               abs,
		publicBase:        strings.TrimRight(strings.TrimSpace(publicBase), "/"),
		mdns:              mdnsRaw != "0" && !strings.EqualFold(mdnsRaw, "false"),
		dedupe:            envBool([]string{"ASSET_HUB_DEDUPE"}, true),
		backup:            envBool([]string{"ASSET_HUB_BACKUP"}, true),
		backupDir:         backupDir,
		backupInterval:    backupInterval,
		backupKeep:        backupKeep,
		indexBuildOnStart: envBool([]string{"ASSET_HUB_INDEX_BUILD_ON_START"}, true),
		extraPorts:        extraPorts,
		extraHost:         envFirst("ASSET_HUB_EXTRA_HOST", "", "127.0.0.1"),
		retentionDays:     retentionDays,
		retentionInterval: retentionInterval,
		retentionMax:      retentionMax,
	}, nil
}

func envBool(keys []string, def bool) bool {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v != "0" && !strings.EqualFold(v, "false") && !strings.EqualFold(v, "no")
		}
	}
	return def
}

// parseExtraPorts 解析附加监听口（健康检查口）。
//
//	空          → 无附加口
//	off/-/none  → 显式关掉（连 SERVICE_PORT 也不加）
//	4236,9000   → 逗号分隔的端口列表
//
// 主口（8080）会被自动剔除：同一个口不重复监听。
func parseExtraPorts(raw string, primary int) ([]int, error) {
	spec := strings.TrimSpace(raw)
	switch strings.ToLower(spec) {
	case "", "-", "off", "none":
		return nil, nil
	}
	seen := map[int]bool{primary: true}
	var out []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("env ASSET_HUB_EXTRA_PORTS=%q 里有非法端口 %q（要 1..65535，逗号分隔）", raw, part)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

func envInt(keys []string, def int) (int, error) {
	for _, key := range keys {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("env %s=%q 不是非负整数", key, v)
		}
		return n, nil
	}
	return def, nil
}

// envDuration 接受 Go 时长（24h/30m）或纯秒数。
func envDuration(keys []string, def time.Duration) (time.Duration, error) {
	for _, key := range keys {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil {
			if n < 0 {
				return 0, fmt.Errorf("env %s=%q 不能为负", key, v)
			}
			return time.Duration(n) * time.Second, nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("env %s=%q 不是合法时长（示例 24h / 30m / 秒数）", key, v)
		}
		return d, nil
	}
	return def, nil
}

func envFirst(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return keys[len(keys)-1]
}

// detectLANIPv4：默认路由出口 IP（与现网 img-server 同思路；loopback 兜底）。
func detectLANIPv4() string {
	conn, err := net.DialTimeout("udp", "8.8.8.8:80", 2*time.Second)
	if err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil && !addr.IP.IsLoopback() {
			return addr.IP.String()
		}
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
					return ipnet.IP.String()
				}
			}
		}
	}
	return "127.0.0.1"
}
