package ippurity

import (
	"context"
	"os"
	"testing"
	"time"
)

// 联网测试，默认跳过：RDAP_LIVE=1 go test -run Live ./server/internal/ippurity/
func TestRegistrationCountryLive(t *testing.T) {
	if os.Getenv("RDAP_LIVE") == "" {
		t.Skip("set RDAP_LIVE=1 to hit real RDAP")
	}
	// 仅使用知名公开地址做联网注册国家探测验证，不硬编码任何私有节点地址。
	want := map[string]string{"8.8.8.8": "US", "9.9.9.9": "US"}
	for ip, w := range want {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		got, reg, err := RegistrationCountry(ctx, ip)
		cancel()
		if err != nil || got != w {
			t.Errorf("%s: want %s, got %q via %s err=%v", ip, w, got, reg, err)
		}
	}
}
