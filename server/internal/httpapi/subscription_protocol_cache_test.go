package httpapi

import (
	"strings"
	"testing"
)

// 关掉某协议后，买家订阅不能再拿到它：协议开关动作必须作废订阅缓存，
// 且订阅只收「installed 且 active」的协议。
func TestProtocolSwitchInvalidatesSubscriptionCache(t *testing.T) {
	for _, verb := range []string{"reality_toggle", "reality_reset", "install_xray", "vmess_enable", "vmess_disable",
		"vlessenc_enable", "vlessenc_disable", "anytls_enable", "anytls_disable"} {
		if !subscriptionAffectingVerbs[verb] {
			t.Fatalf("%s 改变协议开关，必须作废订阅缓存", verb)
		}
		if _, ok := portalActions[verb]; !ok {
			t.Fatalf("%s 不在动作白名单里", verb)
		}
	}
	body := funcBlock(t, "portalaction.go", "func (s *Server) portalNodeAction(")
	if !strings.Contains(body, "subscriptionAffectingVerbs[input.Verb]") || !strings.Contains(body, "InvalidateSubscriptionCache()") {
		t.Fatal("下发动作后没有作废订阅缓存")
	}
	for file, sig := range map[string]string{"vmess.go": "func vmessFromCapability(", "vlessenc.go": "func vlessEncFromCapability("} {
		if !strings.Contains(funcBlock(t, file, sig), `capabilityBool(m, "active")`) {
			t.Fatalf("%s 必须只收正在运行（active）的协议", sig)
		}
	}
}
