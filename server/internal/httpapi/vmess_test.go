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

func TestVMessUUIDMatchesPortalPython(t *testing.T) {
	for uid, want := range map[string]string{"sub_test": "35e2c621-41bb-511b-9a38-b11dc714431c", "admin_master": "101c0865-7ece-5645-83d2-f6f7519fe8b0", "用户": "2e4faa28-7c2e-55f2-850f-ee7f9c0139a9"} {
		if got := vmessUUIDForUser(uid); got != want {
			t.Errorf("UUID %q = %s, want %s", uid, got, want)
		}
		if vmessUUIDForUser(uid) == realityUUIDForUser(uid) {
			t.Fatal("protocol namespaces collided")
		}
	}
}
func TestVMessFormatsUseSubscriberAndTransport(t *testing.T) {
	n := subscriptionNode{Name: "东京", Host: "192.0.2.1", Port: 1234, AccountUserID: "sub_test", VMess: &nodeVMess{Domain: "vm.example.com", SNI: "tls.example.com", WSPath: "/ws-test", Port: 8443}}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(vmessURI(n), "vmess://"))
	if err != nil {
		t.Fatal(err)
	}
	var uri map[string]string
	if err = json.Unmarshal(raw, &uri); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"id": "35e2c621-41bb-511b-9a38-b11dc714431c", "add": "vm.example.com", "sni": "tls.example.com", "path": "/ws-test", "port": "8443", "net": "ws", "tls": "tls", "aid": "0"} {
		if uri[key] != want {
			t.Errorf("URI %s=%q want %q", key, uri[key], want)
		}
	}
	clash := clashVMessProxy(n)
	if clash["uuid"] != uri["id"] || clash["server"] != uri["add"] || clash["servername"] != uri["sni"] || clash["tls"] != true {
		t.Fatal(clash)
	}
	sing := singBoxVMessOutbound(n)
	if sing["uuid"] != uri["id"] || sing["server"] != uri["add"] {
		t.Fatal(sing)
	}
	for _, account := range []string{"", "sub_test"} {
		n.AccountUserID = account
		for _, format := range []string{subscriptionFormatURI, subscriptionFormatClash, subscriptionFormatSingBox} {
			body, _, err := renderSubscription([]subscriptionNode{n}, format)
			if err != nil {
				t.Fatal(err)
			}
			if format == subscriptionFormatURI {
				body, err = base64.StdEncoding.DecodeString(string(body))
				if err != nil {
					t.Fatal(err)
				}
			}
			has := strings.Contains(string(body), "vmess")
			if has != (account != "") {
				t.Errorf("format %s account=%q vmess=%v", format, account, has)
			}
		}
	}
}
func TestVMessDiscoveryRejectsDisabledAndMalformed(t *testing.T) {
	for _, input := range []string{`{}`, `{"installed":true,"active":false}`, `{"installed":true,"active":true,"domain":"x","port":65536}`, `{"installed":true,"active":true,"configured":false,"domain":"x","port":443}`, `{"installed":true,"active":true,"domain":"x","port":443.5}`, `{"installed":true,"active":true,"domain":"x","port":443,"ws_path":"relative"}`} {
		var cap map[string]any
		json.Unmarshal([]byte(input), &cap)
		if vmessFromCapability(cap) != nil {
			t.Errorf("accepted %s", input)
		}
	}
	var cap map[string]any
	json.Unmarshal([]byte(`{"installed":true,"active":true,"configured":true,"domain":"example.com","port":443}`), &cap)
	got := vmessFromCapability(cap)
	if got == nil || got.SNI != "example.com" || got.WSPath != "/" {
		t.Fatal(got)
	}
}
func TestVMessActionsCannotBeTurnedIntoToggle(t *testing.T) {
	for _, action := range []string{"enable", "disable"} {
		spec := portalActions["vmess_"+action]
		form := buildForm(spec, map[string]string{"action": "toggle", "domain": "evil", "port": "22"})
		if spec.Path != "manage-vmess" || form["action"] != action || len(form) != 1 {
			t.Fatal(spec, form)
		}
		if err := validateActionForm(spec, form); err != nil {
			t.Fatal(err)
		}
	}
	if validateActionForm(portalActionSpec{Path: "manage-vmess"}, map[string]string{"action": "toggle"}) == nil {
		t.Fatal("toggle accepted")
	}
}
func TestRealitySharedXrayNeedsInboundEvidence(t *testing.T) {
	for _, configured := range []bool{false, true} {
		r := map[string]any{"active": true, "installed": true, "configured": configured}
		verifyRealityCapability(context.Background(), r, nil)
		if capabilityBool(r, "active") {
			t.Fatal("shared Xray claimed Reality is active", r)
		}
		if configured && r["state_source"] != "unknown" {
			t.Fatal(r)
		}
	}
	r := map[string]any{"active": true, "installed": true, "inbound": true}
	verifyRealityCapability(context.Background(), r, nil)
	if !capabilityBool(r, "active") {
		t.Fatal("explicit inbound was rejected")
	}
}

func TestRealityCapabilityUsesBasicInboundStatus(t *testing.T) {
	active := false
	endpointMissing := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/reality-status") && !strings.HasSuffix(r.URL.Path, "/vmess-status") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Error("missing Basic auth")
		}
		if endpointMissing {
			w.WriteHeader(404)
			w.Write([]byte(`{"ok":false,"error":"API endpoint not found"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "installed": true, "active": active})
	}))
	defer server.Close()
	old := portalClient
	portalClient = server.Client()
	t.Cleanup(func() { portalClient = old })
	target := &portalWebTarget{Origin: server.URL, PathToken: strings.Repeat("a", 64), Username: "u", Password: "p"}
	for _, want := range []bool{false, true} {
		active = want
		r := map[string]any{"installed": true, "active": true, "configured": true}
		verifyRealityCapability(context.Background(), r, target)
		if capabilityBool(r, "active") != want || r["state_source"] != "reality-status" {
			t.Fatal(r)
		}
	}
	endpointMissing = true
	res := (&Server{}).probePortalEndpoint(context.Background(), target, "vmess-status", func() any { return &portalVMessInfo{} })
	if res.State != "unsupported" {
		t.Fatal(res)
	}
}
