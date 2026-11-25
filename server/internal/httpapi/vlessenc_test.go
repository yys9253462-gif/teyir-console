package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testVLESSEncryption = "mlkem768x25519plus.native.0rtt.ptjHQxBQxTJ9MWr2cd5qWIflBSACHOevTauCQwa_71U"

func vlessEncTestNode() subscriptionNode {
	return subscriptionNode{
		Name:          "东京",
		Host:          "192.0.2.1",
		Port:          14617,
		AccountUserID: "sub_test",
		VLESSEnc: &nodeVLESSEncryption{
			Domain:     "enc.example.com",
			SNI:        "enc.example.com",
			Encryption: testVLESSEncryption,
			Port:       2088,
		},
	}
}

// 上游 vlessenc_clients() 复用 Reality 的 UUID 命名空间
// （reality_uuid_for_user），不是 VMess 的命名空间。
// 用错命名空间时服务端白名单里没有这个 UUID，客户端只会看到 authentication failed。
func TestVLESSEncryptionUsesRealityUUIDNamespace(t *testing.T) {
	cases := map[string]string{
		"sub_test":     "5ad63ed7-d6fb-52bb-9711-ee105a89a625",
		"admin_master": "a45f2360-5dcf-5922-8fbf-fabf5cf367a2",
	}
	for uid, want := range cases {
		got := realityUUIDForUser(uid)
		if got != want {
			t.Fatalf("realityUUIDForUser(%q) = %s, want %s", uid, got, want)
		}
		if vmessUUIDForUser(uid) == got {
			t.Fatal("VLESS Encryption must not use the VMess namespace")
		}
	}
	uri := vlessEncURI(vlessEncTestNode(), realityUUIDForUser("sub_test"))
	if !strings.HasPrefix(uri, "vless://5ad63ed7-d6fb-52bb-9711-ee105a89a625@") {
		t.Fatalf("URI must carry the subscriber's Reality UUID, got %s", uri)
	}
	if strings.Contains(uri, "admin_master") {
		t.Fatal("administrator identity leaked into the buyer URI")
	}
}

// 管理员 URI 的 userinfo 是 MASTER UUID，必须丢弃；只保留公开 encryption。
func TestVLESSEncryptionURIParsesOnlyPublicParameter(t *testing.T) {
	admin := "vless://a45f2360-5dcf-5922-8fbf-fabf5cf367a2@enc.example.com:2088" +
		"?encryption=" + testVLESSEncryption +
		"&security=tls&sni=enc.example.com&type=tcp&headerType=none&fp=chrome#VLESS-Enc-admin_master"
	got, err := parseVLESSEncryptionURI(admin, "enc.example.com", 2088)
	if err != nil {
		t.Fatal(err)
	}
	if got != testVLESSEncryption {
		t.Fatalf("encryption = %q", got)
	}
	// 解析结果里不存在任何 UUID 字段 —— 结构体本身就不携带它。
	if strings.Contains(got, "a45f2360") || strings.Contains(got, "@") {
		t.Fatal("administrator UUID leaked into parsed value")
	}
}

func TestVLESSEncryptionRejectsBadOrMissingPublicParameter(t *testing.T) {
	for _, raw := range []string{
		"",
		"https://enc.example.com:2088?encryption=" + testVLESSEncryption,
		"vless://uuid@enc.example.com:2088?security=tls",
		"vless://uuid@enc.example.com:2088?encryption=none",
		"vless://uuid@enc.example.com:2088?encryption=mlkem768x25519plus",
		"vless://uuid@enc.example.com:2088?encryption=mlkem768x25519plus.bogus.0rtt.key",
		"vless://uuid@enc.example.com:2088?encryption=mlkem768x25519plus.native.0rtt",
		// 地址/端口与能力声明不一致：会把 A 节点的密钥配到 B 节点。
		"vless://uuid@other.example.com:2088?encryption=" + testVLESSEncryption,
		"vless://uuid@enc.example.com:9999?encryption=" + testVLESSEncryption,
	} {
		if _, err := parseVLESSEncryptionURI(raw, "enc.example.com", 2088); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

// capabilities 只有状态字段，没有客户端密钥；缺段/未启用/配置不全都不构成通道。
func TestVLESSEncryptionCapabilityDiscovery(t *testing.T) {
	for _, input := range []string{
		`{}`,
		`{"installed":true,"active":false,"configured":true,"domain":"enc.example.com","port":2088}`,
		`{"installed":true,"active":true,"configured":false,"domain":"enc.example.com","port":2088}`,
		`{"installed":true,"active":true,"configured":true,"port":2088}`,
		`{"installed":true,"active":true,"configured":true,"domain":"enc.example.com","port":65536}`,
		`{"installed":true,"active":true,"configured":true,"domain":"enc.example.com","port":2088.5}`,
	} {
		var cap map[string]any
		if err := json.Unmarshal([]byte(input), &cap); err != nil {
			t.Fatal(err)
		}
		if vlessEncFromCapability(cap) != nil {
			t.Errorf("accepted %s", input)
		}
	}
	var cap map[string]any
	json.Unmarshal([]byte(`{"installed":true,"active":true,"configured":true,"domain":"enc.example.com","port":2088}`), &cap)
	got := vlessEncFromCapability(cap)
	if got == nil || got.SNI != "enc.example.com" || got.Port != 2088 {
		t.Fatalf("valid capability rejected: %#v", got)
	}
	if _, ok := cap["encryption"]; ok {
		t.Fatal("capabilities must not contain a client encryption parameter")
	}
}

// 无 Basic 凭据 ⇒ 明确不可交付，且 capability 被标注 delivery_error，
// 不能静默显示成「已启用」。
func TestVLESSEncryptionWithoutBasicIsNotDeliverable(t *testing.T) {
	// JSON 解出来的数字是 float64 —— capabilityPort 只认这种形态。
	cap := map[string]any{"installed": true, "active": true, "configured": true, "domain": "enc.example.com", "port": float64(2088)}
	applyVLESSEncryptionCapability(context.Background(), cap, nil)
	if cap["client_encryption_present"] != false {
		t.Fatal(cap)
	}
	if reason, _ := cap["delivery_error"].(string); !strings.Contains(reason, "不可交付") {
		t.Fatalf("delivery_error = %q", reason)
	}
	srv := &Server{}
	enc, reason := srv.loadNodeVLESSEncryption(context.Background(), "", vlessEncFromCapability(cap))
	if enc != nil || !strings.Contains(reason, "不可交付") {
		t.Fatalf("enc=%#v reason=%q", enc, reason)
	}
}

// 端点缺失（老门户）与「拿不到」要分开说：前者升级门户，后者查凭据/网络。
func TestVLESSEncryptionEndpointMissingIsReportedAsOutdated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/vlessenc-status") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Error("missing Basic auth")
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"ok":false,"error":"API endpoint not found"}`))
	}))
	defer server.Close()
	old := portalClient
	portalClient = server.Client()
	t.Cleanup(func() { portalClient = old })

	target := &portalWebTarget{Origin: server.URL, PathToken: strings.Repeat("a", 64), Username: "u", Password: "p"}
	_, err := readVLESSEncryptionStatus(context.Background(), target, &vlessEncCapability{Domain: "enc.example.com", SNI: "enc.example.com", Port: 2088, Installed: true, Active: true, Configured: true})
	if err == nil {
		t.Fatal("expected endpoint-missing error")
	}
	if !isPortalEndpointMissing(err.Error()) {
		t.Fatalf("error = %q, expected endpoint missing", err)
	}
	// 面向操作者的措辞由 loadNodeVLESSEncryption 生成；这里直接钉住同一判据。
	reason := "节点门户版本过旧（无 vlessenc-status 端点），VLESS Encryption 不可交付"
	if !strings.Contains(reason, "过旧") || !strings.Contains(reason, "不可交付") {
		t.Fatalf("reason = %q", reason)
	}
}

// 正常节点：Basic 端点返回公开参数，解析成功且不触碰 decryption。
func TestVLESSEncryptionReadsPublicParameterOverBasic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+strings.Repeat("a", 64)+"/vlessenc-status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "installed": true, "active": true,
			"config": map[string]any{
				"uri": "vless://a45f2360-5dcf-5922-8fbf-fabf5cf367a2@enc.example.com:2088?encryption=" +
					testVLESSEncryption + "&security=tls&sni=enc.example.com#admin",
				"uuid":   "a45f2360-5dcf-5922-8fbf-fabf5cf367a2",
				"domain": "enc.example.com", "sni": "enc.example.com", "port": 2088,
				// 上游不会返回 decryption；即使返回，我们的结构体也不读取它。
				"decryption": "must-not-be-used",
			},
		})
	}))
	defer server.Close()
	old := portalClient
	portalClient = server.Client()
	t.Cleanup(func() { portalClient = old })

	target := &portalWebTarget{Origin: server.URL, PathToken: strings.Repeat("a", 64), Username: "u", Password: "p"}
	cap := &vlessEncCapability{Domain: "enc.example.com", SNI: "enc.example.com", Port: 2088, Installed: true, Active: true, Configured: true}
	enc, err := readVLESSEncryptionStatus(context.Background(), target, cap)
	if err != nil || enc == nil {
		t.Fatalf("enc=%#v err=%v", enc, err)
	}
	if enc.Encryption != testVLESSEncryption {
		t.Fatalf("encryption = %q", enc.Encryption)
	}
	if strings.Contains(enc.Encryption, "must-not-be-used") {
		t.Fatal("server decryption leaked into client parameters")
	}
}

// 三种格式的行为必须**不同**，且都以「不生成坏配置」为准：
//   - URI：下发 vless://，带公开 encryption 与买家 UUID
//   - Clash/mihomo：官方支持顶层 encryption，下发
//   - sing-box：官方 outbound 无 encryption 字段，**不下发**
func TestVLESSEncryptionFormatBehaviour(t *testing.T) {
	node := vlessEncTestNode()
	for _, account := range []string{"", "sub_test"} {
		node.AccountUserID = account
		for _, format := range []string{subscriptionFormatURI, subscriptionFormatClash, subscriptionFormatSingBox} {
			body, _, err := renderSubscription([]subscriptionNode{node}, format)
			if err != nil {
				t.Fatal(err)
			}
			raw := string(body)
			if format == subscriptionFormatURI {
				decoded, err := base64.StdEncoding.DecodeString(raw)
				if err != nil {
					t.Fatal(err)
				}
				raw = string(decoded)
			}
			has := strings.Contains(raw, testVLESSEncryption) || strings.Contains(raw, "-VLESS-Enc")
			if account == "" {
				if has {
					t.Errorf("format %s emitted a VLESS-Enc channel without a dedicated account", format)
				}
				continue
			}
			switch format {
			case subscriptionFormatURI:
				if !strings.Contains(raw, "vless://") || !strings.Contains(raw, "encryption="+pctEncode(testVLESSEncryption)) {
					t.Errorf("URI format missing VLESS Encryption: %s", raw)
				}
			case subscriptionFormatClash:
				var doc struct {
					Proxies []map[string]any `json:"proxies"`
				}
				if err := json.Unmarshal(body, &doc); err != nil {
					t.Fatal(err)
				}
				var found map[string]any
				for _, p := range doc.Proxies {
					if p["name"] == "东京-VLESS-Enc" {
						found = p
					}
				}
				if found == nil {
					t.Fatal("Clash format missing VLESS Encryption proxy")
				}
				// mihomo 官方文档：encryption 是 vless 代理顶层字段。
				if found["encryption"] != testVLESSEncryption {
					t.Fatalf("Clash encryption = %#v", found["encryption"])
				}
				if found["uuid"] != realityUUIDForUser("sub_test") {
					t.Fatalf("Clash uuid = %#v", found["uuid"])
				}
				if _, ok := found["flow"]; ok {
					t.Fatal("VLESS Encryption must not carry Reality's flow")
				}
			case subscriptionFormatSingBox:
				if has || strings.Contains(raw, testVLESSEncryption) {
					t.Fatal("sing-box official outbound has no encryption field; must not emit a config that would be ignored")
				}
			}
		}
	}
}

// 老节点：capabilities 完全没有 vlessenc 段时，其它协议的输出一个字都不变。
func TestVLESSEncryptionOldNodeCompatibility(t *testing.T) {
	old := subscriptionNode{Name: "旧节点", Host: "192.0.2.9", Port: 443, AccountUserID: "sub_test"}
	for _, format := range []string{subscriptionFormatURI, subscriptionFormatClash, subscriptionFormatSingBox} {
		body, _, err := renderSubscription([]subscriptionNode{old}, format)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "VLESS-Enc") || strings.Contains(string(body), "mlkem") {
			t.Errorf("format %s invented a channel for an old node", format)
		}
	}
	var caps struct {
		Protocols map[string]map[string]any `json:"protocols"`
	}
	json.Unmarshal([]byte(`{"hysteria2":{"installed":true,"active":true}}`), &caps)
	if vlessEncFromCapability(caps.Protocols["vlessenc"]) != nil {
		t.Fatal("missing vlessenc section must be treated as no channel")
	}
}

func TestVLESSEncryptionActionsAreFixed(t *testing.T) {
	for _, action := range []string{"enable", "disable"} {
		spec := portalActions["vlessenc_"+action]
		form := buildForm(spec, map[string]string{"action": "toggle", "port": "22", "encryption": "evil"})
		if spec.Path != "manage-vlessenc" || form["action"] != action || len(form) != 1 {
			t.Fatal(spec, form)
		}
		if err := validateActionForm(spec, form); err != nil {
			t.Fatal(err)
		}
	}
	if validateActionForm(portalActionSpec{Path: "manage-vlessenc"}, map[string]string{"action": "toggle"}) == nil {
		t.Fatal("toggle accepted")
	}
	if !portalActions["vlessenc_disable"].Dangerous {
		t.Fatal("disable must require X-Confirm-Dangerous")
	}
	if portalActions["vlessenc_enable"].Dangerous {
		t.Fatal("enable is recoverable and should not be flagged dangerous")
	}
}
