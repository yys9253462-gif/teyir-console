package httpapi

import "testing"

func TestValidateOutboundForm(t *testing.T) {
	ok := []map[string]string{
		{"action": "save", "name": "us_exit", "type": "socks5", "addr": "1.2.3.4:1080", "username": "u", "password": "p"},
		{"action": "save", "name": "hk_http", "type": "http", "url": "http://5.6.7.8:3128"},
		{"action": "save", "name": "v4only", "type": "direct", "mode": "4"},
		{"action": "delete", "name": "us_exit"},
		{"action": "mode", "mode": "global"},
		{"action": "rule-add", "domain": "openai.com", "outbound": "us_exit"},
		{"action": "rule-del", "domain": ".netflix.com"},
	}
	for _, f := range ok {
		if err := validateOutboundForm(f); err != nil {
			t.Errorf("%v should pass: %v", f, err)
		}
	}
	bad := []map[string]string{
		{"action": "save", "name": "my-proxy", "type": "socks5", "addr": "1.2.3.4:1080"},
		{"action": "save", "name": "x", "type": "socks5", "addr": "1.2.3.4"},
		{"action": "save", "name": "x", "type": "vmess"},
		{"action": "save", "name": "x", "type": "http", "url": "ftp://a"},
		{"action": "save", "name": "x", "type": "socks5", "addr": "1.2.3.4:1080", "password": "a\nb"},
		{"action": "mode", "mode": "all"},
		{"action": "rule-add", "domain": "a b.com"},
		{"action": "drop"},
	}
	for _, f := range bad {
		if err := validateOutboundForm(f); err == nil {
			t.Errorf("%v should be rejected", f)
		}
	}
}

func TestRedactProxyURL(t *testing.T) {
	if got := redactProxyURL("http://user:secret@1.2.3.4:3128"); got != "http://user@1.2.3.4:3128" {
		t.Fatalf("password must be stripped, got %s", got)
	}
}
