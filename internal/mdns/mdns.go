// Package mdns：把 Asset Hub 广告成 `_ha-img-server._tcp`（img-server.local）。
//
// iOS/Edge 是按 **服务类型** 发现资源服务器的（现网由 img-server/serve.py 经
// server/mdns_service.py 发布，macOS 上最终落到 `dns-sd -R`）。Go 侧不引第三方依赖，
// 直接用同一招：起一个 `dns-sd -R` 子进程，随进程退出而结束。
//
// **这里是「端口权威」**：广告里的 port 就是本服务真实监听的那个口
// （`ASSET_HUB_PORT`，默认 8080）。消费方（iOS 已如此）只要按名字发现，就不需要知道端口；
// 改端口只改本服务的一处配置。
//
// 实例名/TXT 与现网保持一致，避免客户端按名字匹配时失效；TXT 额外带上自描述字段，
// 让发现一次就够（不用再猜路径）：
//
//	name="Home Agent img-server"  type="_ha-img-server._tcp"  port=<port>
//	txt: role=img-server service=home-asset-hub version=<ver> health=/health api=/api/v1/assets
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

	// HealthPath / APIPath 放进 TXT：消费方发现后能直接拼健康检查与清单地址。
	HealthPath = "/health"
	APIPath    = "/api/v1/assets"
)

// Options 广告参数。Enabled=false 时不发布（或 MAC_EDGE_IMG_MDNS=0）。
type Options struct {
	Enabled     bool
	ServiceType string
	Instance    string
	Hostname    string
	Port        int
	Version     string // 部署版本（TXT 里给排查用；空则用 -）
	PublicBase  string // 对外地址（TXT 里给需要 LAN URL 的消费方；空则省略）
}

// Publisher 一次广告（Close 结束）。
type Publisher struct {
	cmd *exec.Cmd
}

// txtRecords 组装 TXT（顺序稳定，便于测试与肉眼比对）。
func txtRecords(opts Options) []string {
	rec := []string{
		"role=img-server",
		"service=home-asset-hub",
		"port=" + strconv.Itoa(opts.Port),
		"health=" + HealthPath,
		"api=" + APIPath,
	}
	v := strings.TrimSpace(opts.Version)
	if v == "" {
		v = "-"
	}
	rec = append(rec, "version="+v)
	if base := strings.TrimRight(strings.TrimSpace(opts.PublicBase), "/"); base != "" {
		rec = append(rec, "base="+base)
	}
	return rec
}

// argv 组装 `dns-sd -R` 参数（纯函数：单测直接比对）。
func argv(opts Options) []string {
	svc := strings.TrimSpace(opts.ServiceType)
	if svc == "" {
		svc = DefaultServiceType
	}
	instance := strings.TrimSpace(opts.Instance)
	if instance == "" {
		instance = DefaultInstance
	}
	args := []string{"-R", instance, svc, "", strconv.Itoa(opts.Port)}
	return append(args, txtRecords(opts)...)
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
	cmd := exec.Command(bin, argv(opts)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mdns publish: %w", err)
	}
	args := argv(opts)
	instance := args[1]
	logger.Info("mdns published",
		"instance", instance, "type", args[2], "port", opts.Port,
		"txt", strings.Join(txtRecords(opts), ","), "pid", cmd.Process.Pid)
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
