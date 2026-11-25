package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTelegramBottomKeyboardPersistent(t *testing.T) {
	m := telegramBottomKeyboard()
	if m["is_persistent"] != true || m["one_time_keyboard"] != false || m["resize_keyboard"] != true {
		t.Fatal("not persistent bottom keyboard")
	}
	if _, ok := m["keyboard"]; !ok {
		t.Fatal("reply keyboard missing")
	}
	if _, ok := m["inline_keyboard"]; ok {
		t.Fatal("inline menu instead of bottom keyboard")
	}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), "节点管理") {
		t.Fatal("missing Chinese menu")
	}
}
func TestTelegramFriendlyPurityNoRawJSON(t *testing.T) {
	raw := []byte(`{"count":1,"items":[{"node_id":"private-uuid","node_name":"日本-东京","host":"1.2.3.4","purity":{"fraud_score":3,"connection_type":"hosting","asn":"AS16509","raw":{"provider":"raw-data-secret"}}}]}`)
	text := telegramFriendlyResult(telegramAction{Method: "GET", Path: "/api/ip-purity"}, 200, raw)
	for _, want := range []string{"日本-东京", "风险分：3", "线路类型：机房"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing", want, text)
		}
	}
	for _, bad := range []string{"{", "node_id", "private-uuid", "raw-data-secret", "AS16509"} {
		if strings.Contains(text, bad) {
			t.Fatal("raw technical data", bad)
		}
	}
}
func TestTelegramFriendlyStatusAndLimits(t *testing.T) {
	out := telegramFriendlyResult(telegramAction{}, 403, []byte(`{"error":"需要管理员角色"}`))
	if !strings.Contains(out, "需要管理员角色") {
		t.Fatal(out)
	}
	out = telegramFriendlyResult(telegramAction{}, 200, []byte(`{"password":"never-show","clash":"sensitive-content","raw":{"secret":"hidden"}}`))
	if strings.Contains(out, "never-show") || strings.Contains(out, "sensitive-content") {
		t.Fatal(out)
	}
	out = telegramFriendlyResult(telegramAction{}, 200, []byte(`{"items":[]}`))
	if out != "暂无记录。" {
		t.Fatal(out)
	}
}

func TestTelegramHideKeyboard(t *testing.T) {
	b, _ := json.Marshal(telegramBottomKeyboard())
	if !strings.Contains(string(b), "收起菜单") {
		t.Fatal("hide button missing")
	}
	m := telegramRemoveKeyboard()
	if m["remove_keyboard"] != true {
		t.Fatal("keyboard not removed")
	}
	if _, ok := m["keyboard"]; ok {
		t.Fatal("remove still contains keyboard")
	}
}
