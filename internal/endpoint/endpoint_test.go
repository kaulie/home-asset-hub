package endpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWritePublishesAtomicJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "discovery")
	path, err := Write(dir, "home-asset-hub", Payload{
		Version:    "d3093ac6",
		Host:       "0.0.0.0",
		Port:       8080,
		PublicBase: "http://192.168.3.84:8080",
		HealthPath: "/health",
		Endpoints:  map[string]string{"assets": "/api/v1/assets", "upload": "/api/v1/photos/upload"},
		Listeners:  []string{"0.0.0.0:8080", "127.0.0.1:4236"},
		PID:        123,
		StartedAt:  "2026-09-18T22:53:52+08:00",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "home-asset-hub.json") {
		t.Fatalf("path = %q", path)
	}
	// 原子写：不该留下 .tmp
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp 残留：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("端点文件不是合法 JSON：%v", err)
	}
	if got["service"] != "home-asset-hub" || got["port"].(float64) != 8080 {
		t.Fatalf("payload = %v", got)
	}
	if got["written_at"] == "" || got["health_path"] != "/health" {
		t.Fatalf("written_at/health_path 缺失：%v", got)
	}
	// 消费方靠这两个字段自校验（服务名 + 健康路径）
	if got["public_base"] != "http://192.168.3.84:8080" {
		t.Fatalf("public_base = %v", got["public_base"])
	}
}

func TestWriteRequiresDirAndService(t *testing.T) {
	if _, err := Write("", "home-asset-hub", Payload{}, nil); err == nil {
		t.Fatal("空目录应报错")
	}
	if _, err := Write(t.TempDir(), "", Payload{}, nil); err == nil {
		t.Fatal("空 service 应报错")
	}
}

func TestWriteOverwritesOldPort(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, "home-asset-hub", Payload{Port: 8080}, nil); err != nil {
		t.Fatal(err)
	}
	// 换端口后重写：文件里必须是新端口（不是追加）
	if _, err := Write(dir, "home-asset-hub", Payload{Port: 18099}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "home-asset-hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Payload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Port != 18099 {
		t.Fatalf("port = %d, want 18099", got.Port)
	}
}
