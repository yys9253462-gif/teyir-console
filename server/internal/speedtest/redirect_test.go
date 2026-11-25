package speedtest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 2026-11-03：portal 测速客户端的注释写着「拒绝重定向」，实际从未设置 CheckRedirect。
func TestPortalClientRefusesRedirects(t *testing.T) {
	if portalClient.CheckRedirect == nil {
		t.Fatal("portalClient must refuse redirects")
	}
	if err := portalClient.CheckRedirect(nil, nil); err == nil {
		t.Fatal("portalClient.CheckRedirect must return an error")
	}
}

// 2026-11-03：主机指纹未登记时测速不得拨号（Dial 在 pinned 为空时接受任何主机密钥，
// 随后会把保存的密码 / 私钥发给应答方）。
func TestRunOnServerRequiresPinnedHostKey(t *testing.T) {
	src, err := os.ReadFile("speedtest.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	guard := strings.Index(text, `if strings.TrimSpace(fingerprint) == "" {`)
	dial := strings.Index(text, "sshclient.Dial(ctx, host, port, user, cred, fingerprint)")
	if guard < 0 || dial < 0 || guard > dial {
		t.Fatal("RunOnServer must refuse to dial before a host fingerprint is pinned")
	}
}

// 2026-11-03：靶点读取失败必须整体报错，不能跳过某一行。
func TestLoadTargetsDoesNotSkipBadRows(t *testing.T) {
	src, err := os.ReadFile("speedtest.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "func (p *Prober) loadTargets(")
	body := text[start : start+strings.Index(text[start:], "\n}\n")]
	if strings.Contains(body, "\t\t\tcontinue\n") {
		t.Fatal("loadTargets must not silently skip rows that fail to scan")
	}
}

// 2026-11-03：探针脚本按「key:host」切分时，IPv6 地址本身含冒号，必须在第一个冒号处切。
func TestProbeScriptKeepsIPv6Host(t *testing.T) {
	cmd, err := BuildProbeCommand([]Target{{Key: "v6-test", Host: "2001:db8::1", Carrier: "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "host=${entry##*:}") || !strings.Contains(cmd, "host=${entry#*:}") {
		t.Fatal("probe script must split key:host on the first colon")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}
	out, err := exec.Command(sh, "-c", `entry=v6-test:2001:db8::1; echo "${entry#*:}"`).Output()
	if err != nil {
		t.Skip("sh not usable: ", err)
	}
	if strings.TrimSpace(string(out)) != "2001:db8::1" {
		t.Fatalf("sh split gave %q", out)
	}
}
