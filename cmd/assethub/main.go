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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kaulie/home-asset-hub/internal/httpapi"
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

	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port)),
		Handler:           httpapi.New(st, publicBase, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	pub, err := mdns.Publish(mdns.Options{
		Enabled:     cfg.mdns,
		ServiceType: mdns.DefaultServiceType,
		Instance:    mdns.DefaultInstance,
		Hostname:    mdns.DefaultHostname,
		Port:        cfg.port,
	}, logger)
	if err != nil {
		logger.Warn("mdns publish failed", "err", err)
	}
	defer pub.Close()

	logger.Info("home-asset-hub starting",
		"version", version,
		"addr", srv.Addr,
		"dir", st.Dir(),
		"public_base", publicBase,
		"health", fmt.Sprintf("http://127.0.0.1:%d/health", cfg.port),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	logger.Info("home-asset-hub stopped")
}

type config struct {
	host       string
	port       int
	dir        string
	publicBase string
	mdns       bool
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
	return config{
		host:       strings.TrimSpace(host),
		port:       port,
		dir:        abs,
		publicBase: strings.TrimRight(strings.TrimSpace(publicBase), "/"),
		mdns:       mdnsRaw != "0" && !strings.EqualFold(mdnsRaw, "false"),
	}, nil
}

func envFirst(keys ...string) string {
	for i, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		_ = i
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
