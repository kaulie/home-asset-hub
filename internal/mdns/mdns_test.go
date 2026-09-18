package mdns

import (
	"reflect"
	"strings"
	"testing"
)

func TestTXTRecordsAreSelfDescribing(t *testing.T) {
	got := txtRecords(Options{Port: 8080, Version: "d3093ac6", PublicBase: "http://192.168.3.84:8080/"})
	want := []string{
		"role=img-server",
		"service=home-asset-hub",
		"port=8080",
		"health=/health",
		"api=/api/v1/assets",
		"version=d3093ac6",
		"base=http://192.168.3.84:8080", // 末尾斜杠要去掉
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("txtRecords = %v\n want %v", got, want)
	}
}

func TestTXTRecordsDefaultsAndNoBase(t *testing.T) {
	got := txtRecords(Options{Port: 4236})
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "port=4236") {
		t.Fatalf("port 缺失: %v", got)
	}
	if !strings.Contains(joined, "version=-") {
		t.Fatalf("版本缺省应为 -: %v", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, "base=") {
			t.Fatalf("没配 public base 就不该有 base=: %v", got)
		}
	}
}

func TestArgvKeepsLegacyInstanceAndType(t *testing.T) {
	args := argv(Options{Port: 8080, Version: "v1"})
	if args[0] != "-R" || args[1] != DefaultInstance || args[2] != DefaultServiceType || args[3] != "" || args[4] != "8080" {
		t.Fatalf("argv 前缀不对（客户端按名字匹配，不能改）: %v", args)
	}
	if args[5] != "role=img-server" {
		t.Fatalf("TXT 首项应保持 role=img-server 向后兼容: %v", args)
	}
}
