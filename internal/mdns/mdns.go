// Package mdns：把 Asset Hub 广告成 `_ha-img-server._tcp`（img-server.local）。
//
// iOS/Edge 是按 **服务类型** 发现资源服务器的（现网由 img-server/serve.py 经
// server/mdns_service.py 发布，macOS 上最终落到 `dns-sd -R`）。Go 侧不引第三方依赖，
// 直接用同一招：起一个 `dns-sd -R` 子进程，随进程退出而结束。
//
// 实例名/TXT 与现网保持一致，避免客户端按名字匹配时失效：
//
//	name="Home Agent img-server"  type="_ha-img-server._tcp"  port=<port>  txt=role=img-server
package mdns

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	DefaultServiceType = "_ha-img-server._tcp"
	DefaultInstance    = "Home Agent img-server"
	DefaultHostname    = "img-server.local"
)

// Options 广告参数。Enabled=false 时不发布（或 MAC_EDGE_IMG_MDNS=0）。
type Options struct {
	Enabled     bool
	ServiceType string
	Instance    string
	Hostname    string
	Port        int
}

// Publisher 一次广告（Close 结束）。
type Publisher struct {
	cmd *exec.Cmd
}

// Publish 起 `dns-sd -R`。找不到 dns-sd（非 macOS）时只告警、不失败。
func Publish(opts Options, logger *slog.Logger) (*Publisher, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !opts.Enabled {
		logger.Info("mdns publish disabled")
		return nil, nil
	}
	bin, err := exec.LookPath("dns-sd")
	if err != nil {
		logger.Warn("mdns publish skipped: dns-sd not found", "err", err)
		return nil, nil
	}
	svc := strings.TrimSpace(opts.ServiceType)
	if svc == "" {
		svc = DefaultServiceType
	}
	instance := strings.TrimSpace(opts.Instance)
	if instance == "" {
		instance = DefaultInstance
	}
	host := strings.TrimSpace(opts.Hostname)
	if host == "" {
		host = DefaultHostname
	}
	args := []string{"-R", instance, svc, "", strconv.Itoa(opts.Port), "role=img-server"}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mdns publish: %w", err)
	}
	logger.Info("mdns published", "instance", instance, "type", svc, "port", opts.Port, "pid", cmd.Process.Pid)
	return &Publisher{cmd: cmd}, nil
}

// Close 结束广告（进程退出时 `dns-sd` 会变孤儿，这里显式收掉）。
func (p *Publisher) Close() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
}
