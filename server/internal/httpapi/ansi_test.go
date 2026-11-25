package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAnsiEscapeStripped(t *testing.T) {
	if got := ansiEscape.ReplaceAllString("\x1b[0;32mok\x1b[0m", ""); got != "ok" {
		t.Fatalf("got %q", got)
	}
}

func TestCreateNodeAcceptsPortalTokenURL(t *testing.T) {
	got, err := normalizePortalURLWithToken("https://node.example.com:11781/0123456789abcdef0123456789abcdef/")
	if err != nil || got != "https://node.example.com:11781" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestInstallSecretsMasked(t *testing.T) {
	for in, want := range map[string]string{
		"密　码：examplePassw0rd": "密　码：examplePassw0rd",
		"密  码: abcdefgh":      "密  码: abcdefgh",
		"  hy2_sec_0123abcd":  "  hy2_sec_********",
		"TCP 11781 私密信息页":     "TCP 11781 私密信息页",
	} {
		if got := maskInstallSecrets(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestTransientJoinError(t *testing.T) {
	if !transientJoinError(errors.New("连接节点 API 失败: dial tcp 1.2.3.4:4997: connect: connection refused")) {
		t.Fatal("connection refused should be retried")
	}
	if transientJoinError(errors.New("api_key 无效")) {
		t.Fatal("auth errors must not be retried")
	}
}

func TestNormalizeDeliveryBase(t *testing.T) {
	for in, want := range map[string]string{
		"sub.example.com":          "https://sub.example.com",
		"https://Sub.Example.com/": "https://sub.example.com",
		"":                         "",
	} {
		if got, err := normalizeDeliveryBase(in); err != nil || got != want {
			t.Errorf("%q -> %q %v, want %q", in, got, err, want)
		}
	}
	// 2026-11-12：主机名里的 %25 会被解码成字面 %，拼回 URL 后不再合法，必须在这里就拒绝。
	for _, bad := range []string{"http://sub.example.com", "https://sub.example.com/sub", "https://a.com?x=1", "localhost", "a%25b.example.com", "https://a%25b.example.com"} {
		if _, err := normalizeDeliveryBase(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestSubLinkCarriesRemarks(t *testing.T) {
	subscriptionNameCache.Store("")
	if got := subLink("https://sub.example.com", "tok", "clash"); got != "https://sub.example.com/sub/tok?format=clash&remarks=Teyir" {
		t.Fatalf("got %q", got)
	}
	subscriptionNameCache.Store("示例订阅")
	defer subscriptionNameCache.Store("")
	if got := subLink("https://sub.example.com", "tok", ""); got != "https://sub.example.com/sub/tok?remarks=%E7%A4%BA%E4%BE%8B%E8%AE%A2%E9%98%85" {
		t.Fatalf("got %q", got)
	}
	if validSubscriptionName(`a"b`) || validSubscriptionName("") || !validSubscriptionName("Teyir 节点") {
		t.Fatal("name validation")
	}
}

func TestNodeIDsScopeOverridesCategory(t *testing.T) {
	s := &Server{}
	cat := "11111111-1111-1111-1111-111111111111"
	rec := &subscriptionRecord{CategoryID: &cat, NodeIDs: []string{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}
	scope, err := s.categoryScopeFor(context.Background(), rec)
	if err != nil || len(scope) != 1 || !scope["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"] {
		t.Fatalf("node_ids should win over category: %v %v", scope, err)
	}
	all := []subscriptionCandidate{{id: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}, {id: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}}
	got, err := s.filterProvisionCandidates(context.Background(), rec, all)
	if err != nil || len(got) != 1 || got[0].id != all[0].id {
		t.Fatalf("provision must only open accounts on chosen nodes: %v %v", got, err)
	}
}

func TestSubscriptionCacheInvalidateBlocksStaleWrite(t *testing.T) {
	InvalidateSubscriptionCache()
	subCache.mu.Lock()
	startGen := subCache.gen
	subCache.mu.Unlock()
	InvalidateSubscriptionCache() // 后台刷新期间发生失效
	subCache.mu.Lock()
	defer subCache.mu.Unlock()
	if subCache.gen == startGen {
		t.Fatal("invalidate must bump generation so an in-flight refresh cannot write back")
	}
}

func TestClientOptionsCacheServesStaleOnError(t *testing.T) {
	node := subscriptionNode{ID: "n1", AccountUserID: "u1", Password: "p1", Host: "h", Port: 1}
	sum := sha256.Sum256([]byte(node.ID + "\x00" + node.AccountUserID + "\x00" + node.Password + "\x00" + node.Host + "\x00" + "1"))
	key := hex.EncodeToString(sum[:])
	cached := node
	cached.ObfsPass = "from-cache"
	clientOptionsCache.Lock()
	clientOptionsCache.m[key] = clientOptionsEntry{node: cached, storedAt: time.Now().Add(-10 * time.Minute)}
	clientOptionsCache.Unlock()
	defer func() { clientOptionsCache.Lock(); delete(clientOptionsCache.m, key); clientOptionsCache.Unlock() }()

	s := &Server{} // 无数据库：实时读取必然失败
	got, err := s.cachedSubscriptionClientOptions(context.Background(), node)
	if err != nil || got.ObfsPass != "from-cache" {
		t.Fatalf("should fall back to the last good result within an hour: %+v %v", got, err)
	}
}

func TestNormalizeProbeQuery(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3.4":                          "1.2.3.4",
		" HY2.Example.com ":                "hy2.example.com",
		"https://jp2.example.com:14617/x/": "jp2.example.com",
		"hysteria2://pw@1.2.3.4:443?sni=a": "1.2.3.4",
		"[2001:db8::1]:443":                "2001:db8::1",
		"example.com.":                     "example.com",
	} {
		if got := normalizeProbeQuery(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestSubscriptionInfoEntries(t *testing.T) {
	exp := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	rec := &subscriptionRecord{ExpiresAt: &exp}
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	info := subscriptionInfoLines(rec, now)
	if len(info) != 3 || info[0] != updateHintLine || info[1] != "📅 套餐到期：2026-11-05" || info[2] != "⏳ 剩余 29 天" {
		t.Fatalf("info lines: %v", info)
	}
	if got := subscriptionInfoLines(&subscriptionRecord{}, now); len(got) != 2 || got[0] != updateHintLine || got[1] != "📅 套餐：永久有效" {
		t.Fatalf("perpetual: %v", got)
	}
	if subscriptionInfoLines(nil, now) != nil {
		t.Fatal("admin preview must not get info entries")
	}

	nodes := []subscriptionNode{sampleNode()}
	body, err := renderClashWithInfo(nodes, info)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if first := doc["proxies"].([]any)[0].(map[string]any); first["name"] != info[0] || first["server"] != "127.0.0.1" {
		t.Fatalf("info entry must come first: %v", first)
	}
	for _, raw := range doc["proxy-groups"].([]any) {
		g := raw.(map[string]any)
		members := g["proxies"].([]any)
		switch g["name"] {
		case groupSelect:
			if members[0] != groupAuto {
				t.Errorf("default pick must stay 自动选择, got %v", members[0])
			}
			if members[2] != info[0] || members[3] != info[1] {
				t.Errorf("info should follow 自动选择/故障转移: %v", members)
			}
		default:
			for _, m := range members {
				if m == info[0] || m == info[1] {
					t.Errorf("info entry leaked into %v", g["name"])
				}
			}
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(string(renderURIListWithInfo(nodes, info)))
	if !strings.HasPrefix(string(raw), "hysteria2://info@127.0.0.1:1") {
		t.Fatalf("URI list must start with info entry: %.60s", raw)
	}
}
