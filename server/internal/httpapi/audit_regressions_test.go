package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCurrentHealthIsIndependentOfHistoricalGaps(t *testing.T) {
	sources := json.RawMessage(`{"hysteria":{"ok":true,"sampled_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}}`)
	healthy, gaps := measurementHealth(sources, json.RawMessage(`[{"reason":"restart_tail"}]`), time.Now(), true)
	if !healthy || gaps != 1 {
		t.Fatal(healthy, gaps)
	}
	if ok, _ := measurementHealth(sources, json.RawMessage(`[]`), time.Now().Add(-2*time.Minute), true); ok {
		t.Fatal("stale data healthy")
	}
	if ok, _ := measurementHealth(sources, json.RawMessage(`[]`), time.Now(), false); ok {
		t.Fatal("uninitialized healthy")
	}
	if ok, gaps := measurementHealth(sources, json.RawMessage(`{malformed`), time.Now(), true); !ok || gaps == 0 {
		t.Fatalf("malformed historical gaps must stay visible without affecting current health: healthy=%v gaps=%d", ok, gaps)
	}
}
func TestPortalUpgradeHasLongWriteBudget(t *testing.T) {
	// 2026-11-06：重型 root 操作的预算必须盖住门户自己的死线（do-upgrade / install-gost 600 秒、
	// install-xray 300 秒、install-warp 240 秒、manage-amneziawg 480 秒），否则面板先超时判失败，
	// 节点上却还在跑。
	if got := portalWebRequestBudget(http.MethodPost, "do-upgrade"); got != portalHeavyInstallTimeout || got <= 600*time.Second {
		t.Fatalf("do-upgrade budget must cover the portal's 600s deadline, got %v", got)
	}
	if got := portalWebRequestBudget(http.MethodPost, "install-gost"); got != portalHeavyInstallTimeout {
		t.Fatalf("install-* budget, got %v", got)
	}
	if got := portalWebRequestBudget(http.MethodPost, "manage-amneziawg"); got != portalAwgWriteTimeout || got <= 480*time.Second {
		t.Fatalf("manage-amneziawg budget must cover the portal's 480s deadline, got %v", got)
	}
	if got := portalWebRequestBudget(http.MethodPost, "manage-warp"); got != portalActionTimeout {
		t.Fatalf("light writes keep the short budget, got %v", got)
	}
	if portalWebRequestBudget(http.MethodGet, "check-version") != portalWebCallTimeout {
		t.Fatal("wrong read timeout")
	}
}
func TestPortalErrorsRedactCredentials(t *testing.T) {
	target := &portalWebTarget{PathToken: strings.Repeat("a", 64), Password: "private-password"}
	message := redactPortalError("url /"+target.PathToken+"/ failed "+target.Password, target)
	if strings.Contains(message, target.PathToken) || strings.Contains(message, target.Password) {
		t.Fatal("credential leaked")
	}
}
func TestPinnedUpdateForm(t *testing.T) {
	ref := strings.Repeat("a", 40)
	form := portalUpdateForm(map[string]string{"_commit": ref})
	if form["ref"] != ref || form["target"] != "portal" {
		t.Fatal(form)
	}
	for _, bad := range []string{"main", "../bad", strings.Repeat("x", 40)} {
		if validBundleSHA(bad) {
			t.Fatal("invalid ref", bad)
		}
	}
}
