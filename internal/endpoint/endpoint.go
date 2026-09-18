// Package endpoint：把「本服务现在在哪个地址上」写成一份**可被发现的端点文件**。
//
// 为什么要这个：端口应该由资源服务器自己决定（`ASSET_HUB_PORT`），但消费方
// （Brain / mac_edge / iOS / 部署平台）不该各自硬编码 8080 —— 实测这台机器上
// `dns-sd` 解析并不稳定（`-B` 列不出自家服务、`-L` 有时 5s 都不返回），所以
// **不能把「知道端口」这件事只压在 Bonjour 上**，也不能压在请求路径上等它。
//
// 于是把「我发现我」拆成三层，逐层降级、每层都能单独解释：
//
//  1. 本文件（确定性，零延迟）：服务启动时写下自己的 host:port + 端点清单
//  2. mDNS `_ha-img-server._tcp`（跨设备，iOS/电视那条路；Brain/Edge 也读，作兜底）
//  3. 默认 8080（现网行为，永远退得回去）
//
// 文件位置（两个都写；哪个能被读到用哪个）：
//
//	<runtime>/backend/endpoint.json     本服务 runtime 里（平台部署保留 backend/）
//	<discovery-dir>/<service>.json      共享发现目录（默认 <runtime>/../.discovery）
//
// 内容自带 `health` 路径与 `service` 名：读到文件的人先自校验再采信 —— 服务挂了/
// 换了端口时，旧文件不会把人带沟里（这是「文件当缓存」与「配置」的区别）。
package endpoint

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Payload 端点文件内容（稳定字段名，消费方按名读）。
type Payload struct {
	Service    string            `json:"service"`     // home-asset-hub
	Version    string            `json:"version"`     // 部署版本（排查用）
	Host       string            `json:"host"`        // 监听地址（0.0.0.0）
	Port       int               `json:"port"`        // 真实监听端口（权威）
	PublicBase string            `json:"public_base"` // 电视/小度能访问的地址
	HealthPath string            `json:"health_path"` // /health
	Endpoints  map[string]string `json:"endpoints"`   // 名字 → 路径
	Listeners  []string          `json:"listeners"`   // 全部监听地址（含附加健康检查口）
	PID        int               `json:"pid"`
	StartedAt  string            `json:"started_at"`
	WrittenAt  string            `json:"written_at"`
}

// Write 写端点文件：dir 下按 `<service>.json` 命名；目录不存在会创建。
func Write(dir, service string, p Payload, logger *slog.Logger) (string, error) {
	if dir == "" || service == "" {
		return "", fmt.Errorf("endpoint: dir 与 service 都不能为空")
	}
	return WritePath(filepath.Join(dir, service+".json"), service, p, logger)
}

// WritePath 写到指定路径（原子写：先 .tmp 再 rename，读的人不会读到半截 JSON）。
func WritePath(path, service string, p Payload, logger *slog.Logger) (string, error) {
	if path == "" || service == "" {
		return "", fmt.Errorf("endpoint: path 与 service 都不能为空")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	p.Service = service
	p.WrittenAt = time.Now().Format(time.RFC3339)
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	logger.Info("endpoint published", "path", path, "port", p.Port, "public_base", p.PublicBase)
	return path, nil
}
