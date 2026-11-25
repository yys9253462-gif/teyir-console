package httpapi

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/geo"
)

// 2026-11-03：添加 3X-UI 面板时，用户手填的名字不能被识别结果覆盖。
func TestXUICreateKeepsTypedName(t *testing.T) {
	body := funcBlock(t, "xuihandlers.go", "func (s *Server) xuiCreatePanel(")
	if strings.Contains(body, "NameAuto:     true,") || !strings.Contains(body, `NameAuto:     strings.TrimSpace(input.Name) == "",`) {
		t.Fatal("xuiCreatePanel 必须只在名字留空时允许识别结果替换名字")
	}
	got := applyGeo(geoCurrent{Name: "HK-自建", NameAuto: false, RegionAuto: true, ProviderAuto: true},
		geo.Info{Country: "JP", Region: "Tokyo", Provider: "AWS"})
	if got.Name != "HK-自建" || got.NameAuto {
		t.Fatalf("手填名字被覆盖: %+v", got)
	}
}

// 2026-11-03：Telegram 封禁 / 解封必须两步确认，且确认带时效、执行后记审计。
func TestTelegramBanRequiresConfirmation(t *testing.T) {
	body := funcBlock(t, "telegram_control.go", "func (s *Server) telegramCallback(")
	first := body[strings.Index(body, `case strings.HasPrefix(data, "tg:banip:")`):]
	first = first[:strings.Index(first, "return")]
	if strings.Contains(first, "telegramBanIPAction(") {
		t.Fatal("tapping ban/unban must only show a confirmation, not execute")
	}
	if !strings.Contains(body, "s.auditTelegramBan(ctx, ip, isUnban)") {
		t.Fatal("confirmed ban/unban must be audited")
	}
	now := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		data string
		at   time.Time
		ok   bool
		ip   string
	}{
		{"tg:bnok:1800000000:1.2.3.4", now.Add(time.Minute), true, "1.2.3.4"},
		{"tg:ubok:1800000000:2001:db8::1", now.Add(time.Minute), true, "2001:db8::1"},
		{"tg:bnok:1800000000:1.2.3.4", now.Add(3 * time.Minute), false, ""},
		{"tg:bnok:1800000000:not-an-ip", now, false, ""},
		{"tg:bnok:garbage", now, false, ""},
	} {
		ip, ok := parseBanConfirm(c.data, c.at)
		if ok != c.ok || ip != c.ip {
			t.Errorf("parseBanConfirm(%q) = %q,%v want %q,%v", c.data, ip, ok, c.ip, c.ok)
		}
	}
	if n := len("tg:ubok:1800000000:ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"); n > 64 {
		t.Fatalf("callback data %d bytes exceeds Telegram's 64-byte limit", n)
	}
}

// 2026-11-03：批量纯净度检测不能再在外层套 purityCheckTimeout ——
// 那会让上游查询、落库与原生判定共用一份 12 秒，INSERT 与 RDAP 经常被截断。
func TestRefreshAllPurityDoesNotShareOneBudget(t *testing.T) {
	body := funcBlock(t, "ippurity.go", "func (s *Server) refreshAllIPPurity(")
	if strings.Contains(body, "context.WithTimeout(r.Context(), purityCheckTimeout)") {
		t.Fatal("batch refresh must not wrap each node in the same budget as the upstream lookup")
	}
	inner := funcBlock(t, "ippurity.go", "func (s *Server) checkAndStorePurity(")
	if !strings.Contains(inner, "context.WithTimeout(ctx, purityCheckTimeout)") {
		t.Fatal("the upstream lookup itself must stay bounded")
	}
}

// 2026-11-03：换 IP 一旦开始改 DNS，写 DNS 与提交都不能再跟随客户端连接。
func TestChangeIPCommitDoesNotFollowClient(t *testing.T) {
	body := funcBlock(t, "hostextras.go", "func (s *Server) changeServerIP(")
	update := strings.Index(body, "UPDATE servers SET host=$2")
	rest := body[update:]
	if strings.Contains(rest, "context.WithTimeout(r.Context(), 30*time.Second)") || strings.Contains(rest, "tx.Commit(r.Context())") {
		t.Fatal("DNS writes and commit must use a context detached from the client")
	}
	// 2026-11-06：不只要求脱离客户端，还要求整段有总预算（原来 commit 完全没有超时，
	// 主机行锁会一直持到提交为止；Cloudflare 卡住时 5 台节点最坏持锁 150 秒）。
	if !strings.Contains(rest, "context.WithTimeout(context.WithoutCancel(r.Context())") || !strings.Contains(rest, "tx.Commit(commitCtx)") {
		t.Fatal("expected a detached commitCtx with an overall budget for DNS writes and commit")
	}
}

// 2026-11-03：迁移在取得租约后必须重读旧主机地址（失败回退 DNS 用它），
// 手动切 DNS 模式的回退必须明确提示操作者把 DNS 改回去。
func TestMigrationRollbackUsesFreshOldHostAndWarnsManualDNS(t *testing.T) {
	body := funcBlock(t, "migratenode.go", "func (s *Server) runMigrateNode(")
	reread := strings.Index(body, "WHERE id=$1::uuid AND deleted_at IS NULL`, oldServerID).Scan(&oldHost)")
	firstDial := strings.Index(body, "s.dialServer(ctx, oldServerID)")
	if reread < 0 || firstDial < 0 || reread > firstDial {
		t.Fatal("old host address must be re-read after the lease, before any remote work")
	}
	if !strings.Contains(body, "if cutover && !in.AutoDNS {") || !strings.Contains(body, "必须手工操作") {
		t.Fatal("manual-DNS rollback must tell the operator to point DNS back")
	}
}

// 2026-11-03：批量升级门户的预算必须覆盖最坏情况（20 台慢节点），超时后剩余节点要有明确结论。
func TestPortalBatchBudgetScalesWithNodes(t *testing.T) {
	if got := portalBatchBudget(20); got < 20*portalBatchNodeBudget {
		t.Fatalf("budget for 20 nodes = %v, below worst case %v", got, 20*portalBatchNodeBudget)
	}
	if portalBatchNodeBudget < 2*(120*time.Second+3*time.Minute) {
		t.Fatalf("per-node budget %v below two rounds of upgrade + verify", portalBatchNodeBudget)
	}
	body := funcBlock(t, "portalbatch.go", "func (s *Server) runPortalBatch(")
	if strings.Contains(body, "90*time.Minute") || !strings.Contains(body, "portalBatchBudget(len(b.Results))") {
		t.Fatal("runPortalBatch must size its deadline by node count")
	}
	if !strings.Contains(body, `"批次超时，未下发"`) {
		t.Fatal("nodes left when the budget runs out must get an explicit result")
	}
}

// 2026-11-03：「已填收入的订阅数」必须与计入收入的订阅同一口径（排除已到期）。
func TestProfitRevenueCountMatchesRevenueQuery(t *testing.T) {
	body := funcBlock(t, "reports.go", "func (s *Server) profitReport(")
	at := strings.Index(body, "Scan(&withRevenue)")
	if at < 0 {
		t.Fatal("withRevenue query not found")
	}
	query := body[strings.LastIndex(body[:at], "SELECT count(*)"):at]
	if !strings.Contains(query, "expires_at IS NULL OR expires_at > now()") {
		t.Fatal("subscriptions_with_revenue must exclude expired subscriptions like the revenue query does")
	}
}

// 2026-11-03：URI 列表里的 IPv6 主机必须加方括号，否则客户端解析失败、节点静默消失。
func TestHysteria2URIBracketsIPv6(t *testing.T) {
	uri := hysteria2URI(subscriptionNode{Name: "v6", Host: "2001:db8::1", Port: 443, Password: "pw"})
	if !strings.Contains(uri, "@[2001:db8::1]:443?") {
		t.Fatalf("IPv6 host must be bracketed: %s", uri)
	}
	if v4 := hysteria2URI(subscriptionNode{Name: "v4", Host: "1.2.3.4", Port: 443, Password: "pw"}); !strings.Contains(v4, "@1.2.3.4:443?") {
		t.Fatalf("IPv4 URI changed: %s", v4)
	}
	if u, err := url.Parse(uri); err != nil || u.Hostname() != "2001:db8::1" || u.Port() != "443" {
		t.Fatalf("URI not parseable as host+port: %v %v", u, err)
	}
	for _, sig := range []string{"func realityURI(", "func vlessEncURI("} {
		file := "subscription.go"
		if sig == "func vlessEncURI(" {
			file = "vlessenc.go"
		}
		if body := funcBlock(t, file, sig); !strings.Contains(body, "net.JoinHostPort(") {
			t.Errorf("%s must build host:port with net.JoinHostPort", sig)
		}
	}
}

// 2026-11-03：Clash 配置对带证书指纹的自签节点必须下发 fingerprint（与 URI 的 pinSHA256 同值）。
func TestClashProxyCarriesCertificatePin(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	proxy := clashProxy(subscriptionNode{Name: "self", Host: "1.2.3.4", Port: 443, Password: "pw", Insecure: true, PinSHA256: pin})
	if proxy["fingerprint"] != pin {
		t.Fatalf("clash proxy must pin the certificate: %+v", proxy)
	}
	if _, ok := clashProxy(subscriptionNode{Name: "ca", Host: "1.2.3.4", Port: 443, Password: "pw"})["fingerprint"]; ok {
		t.Fatal("nodes without a pin must not get an empty fingerprint")
	}
}

// 2026-11-03：已到期订阅不再参与设备上限（不再算超额、不再推拦截名单）。
func TestDeviceLimitSkipsExpiredSubscriptions(t *testing.T) {
	body := funcBlock(t, "devicelimit.go", "func (s *Server) checkDeviceLimits(")
	if !strings.Contains(body, "AND (t.expires_at IS NULL OR t.expires_at > now())") {
		t.Fatal("device limit query must exclude expired subscriptions")
	}
	// 退出名单的订阅必须走解除分支（!listed ⇒ observed），否则到期前拦下的 IP 会一直挂着。
	if !strings.Contains(body, "observed := !listed || !partial[subID]") {
		t.Fatal("subscriptions that leave the query must have their deny list cleared")
	}
}

// 2026-11-03：登录时账号不存在也要跑一次同成本的口令比较，防按响应时间枚举账号。
func TestLoginBurnsPasswordCheckForUnknownAccount(t *testing.T) {
	body := funcBlock(t, "server.go", "func (s *Server) login(")
	lookup := strings.Index(body, "FROM users WHERE lower(email) = lower($1)")
	burn := strings.Index(body, "auth.BurnPasswordCheck(input.Password)")
	reject := strings.Index(body[lookup:], `"邮箱或密码错误"`)
	if lookup < 0 || burn < lookup || reject < 0 || burn > lookup+reject {
		t.Fatal("unknown-account branch must burn a bcrypt comparison before replying 401")
	}
}

// 2026-11-03：到期回收的扫描循环必须检查 rows.Err()。
func TestSweeperLapseLoopChecksRowsErr(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	at := strings.Index(text, "for lapseRows.Next() {")
	if at < 0 || !strings.Contains(text[at:at+1200], "lapseRows.Err()") {
		t.Fatal("lapse loop must check lapseRows.Err()")
	}
}

// 2026-11-03：建用户的邮箱必须先去空白再校验格式。
func TestNormalizeUserEmail(t *testing.T) {
	for in, want := range map[string]string{
		"  Bob@Example.com ": "bob@example.com",
		"a.b+tag@x.io":       "a.b+tag@x.io",
	} {
		if got, ok := normalizeUserEmail(in); !ok || got != want {
			t.Errorf("normalizeUserEmail(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "   ", "no-at-sign", "Bob <bob@x.io>", "a@b\x07.io", strings.Repeat("a", 250) + "@x.io"} {
		if _, ok := normalizeUserEmail(bad); ok {
			t.Errorf("normalizeUserEmail(%q) should be rejected", bad)
		}
	}
	if body := funcBlock(t, "manage.go", "func (s *Server) createUser("); !strings.Contains(body, "normalizeUserEmail(input.Email)") {
		t.Fatal("createUser must validate the email through normalizeUserEmail")
	}
}

// 2026-11-03：所有对用户可填地址的出站拨号都走 portaltls.PublicIP（含 CGNAT / NAT64 拒绝）。
func TestOutboundDialersUseSharedPublicIPCheck(t *testing.T) {
	for _, f := range []struct{ path, want string }{
		{"nodeapi.go", "portaltls.PublicIP(ip.IP)"},
		{"migration_safe.go", "portaltls.PublicIP(p)"},
		{"../collector/nodeapi_probe.go", "portaltls.PublicIP(ip.IP)"},
		{"../speedtest/portal.go", "portaltls.PublicIP(ip)"},
	} {
		src, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), f.want) {
			t.Errorf("%s must check dial targets with %s", f.path, f.want)
		}
	}
}

// 2026-11-03：换绑到不同地址时必须清掉计量基线，并与计量共用一把锁。
func TestBindNodeAPIResetsTrafficBaselineOnNewEndpoint(t *testing.T) {
	body := funcBlock(t, "nodeapi.go", "func (s *Server) bindNodeAPI(")
	for _, want := range []string{
		"lockTrafficMeterForRequest(w, r, nodeID)",
		"traffic_counter_rx = CASE WHEN api_url IS DISTINCT FROM $1 THEN NULL",
		"traffic_boot_id    = CASE WHEN api_url IS DISTINCT FROM $1 THEN NULL",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bindNodeAPI missing %q", want)
		}
	}
}

// 2026-11-04：封禁 / 解封的确认按钮只能执行一次。
func TestBanConfirmIsSingleUse(t *testing.T) {
	now := time.Now()
	data := "tg:bnok:" + strconv.FormatInt(now.Unix(), 10) + ":203.0.113.9"
	if !consumeBanConfirm(data, now) {
		t.Fatal("first use must be accepted")
	}
	if consumeBanConfirm(data, now.Add(time.Second)) {
		t.Fatal("the same confirmation must not run twice")
	}
	body := funcBlock(t, "telegram_control.go", "func (s *Server) telegramCallback(")
	if !strings.Contains(body, "consumeBanConfirm(data, time.Now())") {
		t.Fatal("the confirm callback must consume the confirmation before acting")
	}
}

// 2026-11-04：共享检测与设备上限同一口径，排除已到期订阅。
func TestSharingDetectionSkipsExpiredSubscriptions(t *testing.T) {
	body := funcBlock(t, "reports.go", "func (s *Server) detectSharing(")
	if !strings.Contains(body, "AND (t.expires_at IS NULL OR t.expires_at > now())") {
		t.Fatal("sharing detection must exclude expired subscriptions")
	}
}
