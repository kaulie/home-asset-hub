package main

import (
	"reflect"
	"testing"
	"time"
)

func TestParseExtraPorts(t *testing.T) {
	cases := []struct {
		raw     string
		primary int
		want    []int
		wantErr bool
	}{
		{"", 8080, nil, false},
		{"   ", 8080, nil, false},
		{"off", 8080, nil, false},
		{"none", 8080, nil, false},
		{"-", 8080, nil, false},
		{"4236", 8080, []int{4236}, false},
		{"9000,4236", 8080, []int{4236, 9000}, false}, // 排序稳定
		{" 4236 , 9000 ", 8080, []int{4236, 9000}, false},
		{"8080,4236", 8080, []int{4236}, false}, // 与契约口重复 → 剔除
		{"4236,4236", 8080, []int{4236}, false}, // 自身重复 → 去重
		{"4236,", 8080, []int{4236}, false},     // 尾随逗号不炸
		{"0", 8080, nil, true},                  // 非法端口
		{"70000", 8080, nil, true},              // 越界
		{"abc", 8080, nil, true},                // 非数字
		{"4236,abc", 8080, nil, true},           // 部分非法也整体报错（不静默丢配置）
	}
	for _, c := range cases {
		got, err := parseExtraPorts(c.raw, c.primary)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseExtraPorts(%q) 期望报错，实际 %v", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseExtraPorts(%q) 报错：%v", c.raw, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseExtraPorts(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("ASSET_HUB_TEST_BOOL", "0")
	if envBool([]string{"ASSET_HUB_TEST_BOOL"}, true) {
		t.Fatal(`"0" 应判 false`)
	}
	t.Setenv("ASSET_HUB_TEST_BOOL", "no")
	if envBool([]string{"ASSET_HUB_TEST_BOOL"}, true) {
		t.Fatal(`"no" 应判 false`)
	}
	if !envBool([]string{"ASSET_HUB_TEST_MISSING"}, true) {
		t.Fatal("未设置应取默认值 true")
	}
	t.Setenv("ASSET_HUB_TEST_INT", "12")
	if n, err := envInt([]string{"ASSET_HUB_TEST_INT"}, 7); err != nil || n != 12 {
		t.Fatalf("envInt = %d, %v", n, err)
	}
	t.Setenv("ASSET_HUB_TEST_INT", "-3")
	if _, err := envInt([]string{"ASSET_HUB_TEST_INT"}, 7); err == nil {
		t.Fatal("负数应报错")
	}
	t.Setenv("ASSET_HUB_TEST_DUR", "30m")
	if d, err := envDuration([]string{"ASSET_HUB_TEST_DUR"}, time.Hour); err != nil || d != 30*time.Minute {
		t.Fatalf("envDuration = %v, %v", d, err)
	}
	t.Setenv("ASSET_HUB_TEST_DUR", "90") // 纯数字=秒
	if d, err := envDuration([]string{"ASSET_HUB_TEST_DUR"}, time.Hour); err != nil || d != 90*time.Second {
		t.Fatalf("envDuration(秒) = %v, %v", d, err)
	}
	t.Setenv("ASSET_HUB_TEST_DUR", "nonsense")
	if _, err := envDuration([]string{"ASSET_HUB_TEST_DUR"}, time.Hour); err == nil {
		t.Fatal("非法时长应报错")
	}
}

func TestLoadConfigExtraPortsAndBackupDirDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSET_HUB_DIR", dir+"/img")
	t.Setenv("ASSET_HUB_PORT", "8080")
	t.Setenv("ASSET_HUB_EXTRA_PORTS", "4236")
	t.Setenv("ASSET_HUB_BACKUP_DIR", "") // 不配 → 默认 <资源目录>/../backup
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.extraPorts) != 1 || cfg.extraPorts[0] != 4236 {
		t.Fatalf("extraPorts = %v", cfg.extraPorts)
	}
	if cfg.extraHost != "127.0.0.1" {
		t.Fatalf("extraHost = %q（附加口默认只绑 loopback）", cfg.extraHost)
	}
	if cfg.backupDir != dir+"/backup" {
		t.Fatalf("backupDir = %q", cfg.backupDir)
	}
	if cfg.retentionDays != 0 || !cfg.backup || !cfg.dedupe {
		t.Fatalf("默认值不对：%+v", cfg)
	}
}
