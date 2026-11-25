package portaltls

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestDefaultTLSRejectsUntrustedCertificate(t *testing.T) {
	t.Setenv("PORTAL_TLS_PINS", "")
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer s.Close()
	base := &http.Client{Transport: &http.Transport{}}
	c, err := Client(base, s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get(s.URL); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}
func TestOriginPinAcceptsOnlyExactCertificate(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer s.Close()
	digest := sha256.Sum256(s.Certificate().Raw)
	u, _ := url.Parse(s.URL)
	for _, correct := range []bool{true, false} {
		pin := hex.EncodeToString(digest[:])
		if !correct {
			pin = "0000000000000000000000000000000000000000000000000000000000000000"
		}
		raw, _ := json.Marshal(map[string]string{u.Host: pin})
		t.Setenv("PORTAL_TLS_PINS", string(raw))
		c, err := Client(&http.Client{Transport: &http.Transport{}}, s.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Get(s.URL)
		if correct && err != nil {
			t.Fatal(err)
		}
		if !correct && err == nil {
			t.Fatal("wrong pin accepted")
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
}
func TestMalformedPinsFailClosed(t *testing.T) {
	for _, raw := range []string{"invalid", `{"example.com":"bad-pin"}`} {
		t.Setenv("PORTAL_TLS_PINS", raw)
		if _, err := Client(&http.Client{Transport: &http.Transport{}}, "https://example.com"); err == nil {
			t.Fatal("bad configuration accepted")
		}
	}
}

// 验证源站前缀键、大小写不敏感匹配、sha256: 前缀、默认 Transport 回退及 nil 客户端保护。
func TestClientSupportsOriginPrefixCaseAndDefaultTransport(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer s.Close()
	digest := sha256.Sum256(s.Certificate().Raw)
	u, _ := url.Parse(s.URL)

	// 以带协议前缀与大写键名及 sha256: 前缀配置指纹，并传入 Transport=nil 的基础客户端。
	originKey := "HTTPS://" + u.Host + "/"
	raw, _ := json.Marshal(map[string]string{originKey: "sha256:" + hex.EncodeToString(digest[:])})
	t.Setenv("PORTAL_TLS_PINS", string(raw))

	if _, err := Client(nil, s.URL); err == nil {
		t.Fatal("空基础客户端必须报错")
	}
	c, err := Client(&http.Client{}, s.URL)
	if err != nil {
		t.Fatalf("Transport 为空时应回退默认传输层，实际失败: %v", err)
	}
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatalf("匹配源站指纹应成功握手: %v", err)
	}
	resp.Body.Close()

	// 验证错误源站指纹同样生效并被拦截，防止源站键被静默跳过。
	badRaw, _ := json.Marshal(map[string]string{originKey: "sha256:0000000000000000000000000000000000000000000000000000000000000000"})
	t.Setenv("PORTAL_TLS_PINS", string(badRaw))
	cBad, err := Client(&http.Client{}, s.URL)
	if err != nil {
		t.Fatalf("构造校验客户端失败: %v", err)
	}
	if _, err := cBad.Get(s.URL); err == nil {
		t.Fatal("源站指纹不匹配必须被拒绝")
	}
}
