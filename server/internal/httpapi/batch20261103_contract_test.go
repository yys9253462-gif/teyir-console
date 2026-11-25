package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 2026-11-03 第一批修复的源码守卫。
//
// 到期订阅点「启用」：节点账号按 enabled && !expired 置成停用，
// 提示文案原来却按 enabled 选动词，报「已在 N 个节点上启用」。
func TestUpdateSubscriptionDescribesEffectiveNodeState(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) updateSubscriptionHandler(")
	if strings.Contains(body, "describeSubscriptionStateOutcome(stateOutcome, enabled)") {
		t.Fatal("node state wording must use the effective active state, not the raw enabled flag")
	}
	if !strings.Contains(body, "describeSubscriptionStateOutcome(stateOutcome, active)") {
		t.Fatal("expected describeSubscriptionStateOutcome(stateOutcome, active)")
	}
	i := strings.Index(body, "active := enabled && !rec.expired(time.Now())")
	j := strings.Index(body, "describeSubscriptionStateOutcome(stateOutcome, active)")
	if i < 0 || j < 0 || i > j {
		t.Fatal("active must be computed before the outcome is described")
	}
	if got := describeSubscriptionStateOutcome(subscriptionStateOutcome{Applied: []string{"A"}}, false); !strings.Contains(got, "停用") {
		t.Fatalf("inactive outcome must say 停用, got %q", got)
	}
}

// 2026-11-03：商城重放同一 renewal_id 时到期和启用状态都不变，
// 就不能作废待办 —— 否则退款回收失败后排的 revoke 会被一次重放删掉。
func TestReplayedRenewalKeepsQueuedRevoke(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) storeRenewOrder(")
	guard := strings.Index(body, "if !replayed {\n\t\tif _, err := tx.Exec(r.Context(), `DELETE FROM subscription_pending_node_actions")
	commit := strings.Index(body, "tx.Commit(r.Context())")
	if guard < 0 || commit < 0 || guard > commit {
		t.Fatal("pending node actions may only be cancelled by a renewal that actually applied, inside the transaction")
	}
	if strings.Count(body, "DELETE FROM subscription_pending_node_actions") != 1 {
		t.Fatal("exactly one guarded cancel is expected in storeRenewOrder")
	}
}

// 2026-11-03：手动删除订阅回收不完整时，行被置为停用、sweeper 不会再扫到它，
// 必须登记 revoke+删除 的待办，错误文案里说的「节点恢复后会自动补删」才成立。
func TestIncompleteManualDeleteIsQueued(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) deleteSubscriptionHandler(")
	fail := strings.Index(body, "if err := s.purgeSubscriptionOutcome(r.Context(), rec); err != nil {")
	queue := strings.Index(body, "s.queuePendingNodeAction(r.Context(), rec.ID, pendingActionRevoke, true,")
	reply := strings.Index(body, "http.StatusBadGateway")
	if fail < 0 || queue < 0 || reply < 0 || !(fail < queue && queue < reply) {
		t.Fatal("an incomplete manual delete must queue a revoke-then-delete before answering")
	}
}

// 2026-11-03：一台节点解绑 API（api_url 为空）不能让整轮被墙检测失败。
func TestBlockCheckToleratesNodeWithoutPortalURL(t *testing.T) {
	body := funcBlock(t, "blockcheck.go", "func (s *Server) runBlockChecks(")
	empty := strings.Index(body, `if strings.TrimSpace(apiURL) == "" {`)
	parse := strings.Index(body, "u, err := url.Parse(apiURL)")
	if empty < 0 || parse < 0 || empty > parse {
		t.Fatal("an empty api_url must be handled before strict URL validation")
	}
	if !strings.Contains(body[empty:parse], "targets = append(targets, t)") || !strings.Contains(body[empty:parse], "continue") {
		t.Fatal("a node without a portal URL is still probed on the default port")
	}
}

// 2026-11-03：portal_used 是累计和，删掉一个用户会让它下降；24h 流量必须只累加正向增量。
// 用真实 PostgreSQL 跑与 /api/servers 同一段子查询，构造「涨 → 删用户掉 50 GB → 再涨」。
func TestPortalUsedDeltaIgnoresDrops(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE traffic_samples(node_id uuid, sampled_at timestamptz, rx_bytes bigint, tx_bytes bigint, source text, PRIMARY KEY(node_id, sampled_at, source))`,
		`INSERT INTO servers(id) VALUES('` + fixHost + `')`,
		`INSERT INTO hysteria_nodes(id,name,server_id) VALUES('` + fixNode + `','n','` + fixHost + `')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	gb := int64(1 << 30)
	for i, v := range []int64{100 * gb, 101 * gb, 51 * gb, 53 * gb} {
		if _, err := db.Exec(ctx, `INSERT INTO traffic_samples VALUES($1::uuid, now()-make_interval(mins => $2), 0, $3, 'portal_used')`, fixNode, 40-i*10, v); err != nil {
			t.Fatal(err)
		}
	}
	body := funcBlock(t, "server.go", "func (s *Server) servers(")
	start := strings.Index(body, "COALESCE((SELECT SUM(GREATEST(d.delta, 0)) FROM (")
	end := strings.Index(body, "AS portal_delta")
	if start < 0 || end < start {
		t.Fatal("portal_delta must sum positive deltas")
	}
	expr := body[start:end]
	var got int64
	if err := db.QueryRow(ctx, `SELECT `+expr+` FROM servers s WHERE s.id=$1::uuid`, fixHost).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := 3 * gb; got != want {
		t.Fatalf("24h portal delta = %d GB, want 3 GB (1 + 2, the 50 GB drop is a deleted user)", got/gb)
	}
	if strings.Contains(funcBlock(t, "monitoring.go", "func (s *Server) nodeMonitoring("), "MAX(ts.tx_bytes) FILTER (WHERE ts.source = 'portal_used')") {
		t.Fatal("monitoring must not use max-min on portal_used either")
	}
}

// 2026-11-03：公开交付页的 excluded 只能列这条订阅开了号的节点。
// （同日后续：过滤收敛进 excludedInScope，同时套用分类范围与「买家只给已上架」两条规则。）
func TestDeliveryExcludedIsScopedToSubscription(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) subscriptionView(")
	if !strings.Contains(body, "range s.excludedInScope(r.Context(), build.Excluded, rec)") {
		t.Fatal("excluded nodes on the public delivery page must go through excludedInScope")
	}
	wrapper := funcBlock(t, "subscription.go", "func (s *Server) excludedInScope(")
	if !strings.Contains(wrapper, "s.excludedInScopeChecked(ctx, excluded, rec)") {
		t.Fatal("delivery filtering must delegate to the checked scope implementation")
	}
	helper := funcBlock(t, "subscription.go", "func (s *Server) excludedInScopeChecked(")
	for _, want := range []string{"s.categoryScopeFor(ctx, rec)", "rec.nodeAccounts[e.NodeID]", "AND in_store", "scope != nil && !scope[e.NodeID]", "listed != nil && !listed[e.NodeID]"} {
		if !strings.Contains(helper, want) {
			t.Errorf("excludedInScopeChecked must apply %q", want)
		}
	}
}

// 2026-11-03：标签上限按字符数而不是字节数（22 个汉字就有 66 字节）。
func TestLabelLimitCountsCharacters(t *testing.T) {
	for _, c := range []struct{ file, sig string }{
		{"store.go", "func (s *Server) createSubscriptionHandler("},
		{"store.go", "func (s *Server) updateSubscriptionHandler("},
	} {
		body := funcBlock(t, c.file, c.sig)
		if strings.Contains(body, "len(label) > 64") {
			t.Errorf("%s: 标签长度按字节判断，中文标签会被误拒", c.sig)
		}
		if !strings.Contains(body, "utf8.RuneCountInString(label) > 64") {
			t.Errorf("%s: expected a rune-count limit", c.sig)
		}
	}
	if strings.Contains(readSourceFile(t, "subscription.go"), "len(label) > 64") {
		t.Error("subscription.go: 标签长度按字节判断")
	}
}

// 2026-11-03：旧的 PATCH /api/subscription 停用原来只改库，节点账号照旧 active。
func TestLegacySubscriptionToggleSyncsNodes(t *testing.T) {
	body := funcBlock(t, "subscription.go", "func (s *Server) updateSubscription(")
	update := strings.Index(body, "UPDATE subscription_tokens SET ")
	lock := strings.Index(body, "s.lockSubscriptionRequest(w, r, rec)")
	if lock < 0 || update < lock {
		t.Fatal("legacy mutation must lock and re-read before updating the stable subscription ID")
	}
	for _, want := range []string{"if input.Enabled != nil {", "if input.Label != nil {", "WHERE id=$%d::uuid AND deleted_at IS NULL", "tag.RowsAffected() != 1", "s.queueSubscriptionSyncBounded(r.Context(), rec.ID)"} {
		if !strings.Contains(body, want) {
			t.Errorf("legacy toggle missing protection %q", want)
		}
	}
	apply := strings.Index(body, "s.applySubscriptionNodeStatus(r.Context(), rec, active)")
	queue := strings.Index(body, "s.queuePendingNodeAction(r.Context(), rec.ID, pendingActionDisable")
	if update < 0 || apply < 0 || queue < 0 || !(update < apply && apply < queue) {
		t.Fatal("legacy toggle must push the new state to node accounts and queue a retry on failure")
	}
	if !strings.Contains(body, `resp["node_state_ok"] = outcome.allOK()`) {
		t.Fatal("legacy toggle must report incomplete node sync")
	}
}

// 2026-11-03：定时测速也必须走 portal 通道，否则只接 API 的节点永远没有自动数据，
// node_china_loss（1 小时内至少 2 轮）对它们永不触发。
func TestScheduledSpeedtestCoversAPIOnlyNodes(t *testing.T) {
	body := funcBlock(t, "monitoring_schedule.go", "func (s *Server) runSpeedtestRound(")
	if strings.Contains(body, "JOIN LATERAL (\n    SELECT 1 FROM server_credentials") {
		t.Fatal("定时测速不能再把没有主机 SSH 凭据的节点整个排除")
	}
	for _, want := range []string{"s.prober.RunOnServer(ctx, j.serverID, j.nodeIDs)", "s.prober.RunOnPortalAndStore(ctx, j.nodeID, j.serverID, j.baseURL, j.apiKey)", "if hasCred {"} {
		if !strings.Contains(body, want) {
			t.Errorf("runSpeedtestRound missing %q", want)
		}
	}
}

// 2026-11-03：上传凭据不带 username 时不能把已设好的 ssh_user（如 admin）改回 root；
// 只有占位符 api-only 或空值才填 root。用真实 PostgreSQL 跑同一条 UPDATE。
func TestCredentialUploadKeepsConfiguredSSHUser(t *testing.T) {
	body := funcBlock(t, "server.go", "func (s *Server) uploadCredential(")
	start := strings.Index(body, "UPDATE servers\nSET ssh_user = CASE")
	end := strings.Index(body, "WHERE id=$2 AND deleted_at IS NULL")
	if start < 0 || end < start {
		t.Fatal("uploadCredential must only replace a placeholder ssh_user")
	}
	update := body[start:end] + "WHERE id=$2 AND deleted_at IS NULL"
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE servers ADD COLUMN ssh_user text, ADD COLUMN updated_at timestamptz`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ before, given, want string }{
		{"admin", "", "admin"}, {"api-only", "", "root"}, {"", "", "root"}, {"admin", "ubuntu", "ubuntu"},
	} {
		if _, err := db.Exec(ctx, `DELETE FROM servers; INSERT INTO servers(id, ssh_user) VALUES('`+fixHost+`', NULLIF('`+c.before+`',''))`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, update, c.given, fixHost); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := db.QueryRow(ctx, `SELECT ssh_user FROM servers WHERE id=$1::uuid`, fixHost).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("ssh_user %q + upload username %q → %q, want %q", c.before, c.given, got, c.want)
		}
	}
}

// 2026-11-03：建用户时邮箱按小写存，登录必须不区分大小写，令牌里用库中的邮箱。
func TestLoginEmailIsCaseInsensitive(t *testing.T) {
	body := funcBlock(t, "server.go", "func (s *Server) login(")
	if strings.Contains(body, "FROM users WHERE email = $1`, input.Email") {
		t.Fatal("login must not compare the raw input email: createUser stores it lowercased")
	}
	if !strings.Contains(body, "WHERE lower(email) = lower($1)") {
		t.Fatal("login must match email case-insensitively")
	}
	if !strings.Contains(body, "s.signer.IssueVersion(id, storedEmail, role, version)") {
		t.Fatal("the token must carry the stored email, not the typed casing")
	}
	create := funcBlock(t, "manage.go", "func (s *Server) createUser(")
	if !strings.Contains(create, "normalizeUserEmail(input.Email)") {
		t.Fatal("createUser is expected to store emails lowercased")
	}
	if got, ok := normalizeUserEmail(" Bob@X.io "); !ok || got != "bob@x.io" {
		t.Fatalf("normalizeUserEmail must lowercase and trim, got %q %v", got, ok)
	}
}

// 2026-11-03：有整机网卡计量（node_iface，已含 Hysteria）的节点不能再叠加 hysteria_traffic。
func TestTrafficDoesNotDoubleCountIfaceAndHysteria(t *testing.T) {
	servers := funcBlock(t, "server.go", "func (s *Server) servers(")
	if strings.Contains(servers, "FILTER (WHERE ts.source <> 'portal_used')::bigint AS tx_bytes") {
		t.Fatal("/api/servers 不能把 node_iface 与 hysteria_traffic 一起相加")
	}
	if !strings.Contains(servers, "(ts.source = 'node_iface' OR NOT n.has_iface)") {
		t.Fatal("/api/servers 必须每节点只取一种来源")
	}
	hourly := funcBlock(t, "manage.go", "func (s *Server) trafficHourly(")
	if !strings.Contains(hourly, "(ts.source = 'node_iface' OR NOT (n.traffic_sampled_at > now() - interval '24 hours' IS TRUE))") {
		t.Fatal("/api/traffic/hourly 必须每节点只取一种来源")
	}
}

// 2026-11-03：notified_at 只能在至少一个渠道送达后写；投递失败时保持 NULL，
// 「持续未送达升级重推」才会生效。
func TestAlertNotifiedOnlyAfterDelivery(t *testing.T) {
	admit := funcBlock(t, "alertnotify.go", "func (s *Server) admitTrigger(")
	if strings.Contains(admit, "notified_at") || strings.Contains(admit, "persist(") {
		t.Fatal("admitTrigger must not write notified_at before delivery")
	}
	body := funcBlock(t, "manage.go", "func (s *Server) notifyAlertFor(")
	if !strings.Contains(body, "if s.admitTrigger(ctx, alertID, ev) && s.deliverAlert(ev) {\n\t\t\ts.markDelivered(ctx, alertID)") {
		t.Fatal("notified_at must be recorded only when deliverAlert reports success")
	}
	s := monitoringServer()
	persisted := map[string]bool{}
	s.alertNotifier.persist = func(_ context.Context, id string) error { persisted[id] = true; return nil }
	ev := alertNotification{Event: "triggered", AlertType: "node_offline", Target: "fixture"}
	if !s.admitTrigger(context.Background(), "a1", ev) || persisted["a1"] {
		t.Fatal("admission alone must not persist delivery")
	}
	wh := funcBlock(t, "alertnotify.go", "func (s *Server) deliverWebhook(")
	if !strings.Contains(wh, "if url == \"\" {\n\t\treturn false") || !strings.Contains(wh, "return true\n}") {
		t.Fatal("deliverWebhook must report false when nothing was delivered and true only after a 2xx")
	}
}

// 2026-11-03：读服务状态不能替主机做首次信任，也要受读限流。
func TestServiceStatusRequiresPinnedHostKey(t *testing.T) {
	body := funcBlock(t, "serviceControl.go", "func (s *Server) serviceStatus(")
	limit := strings.Index(body, "s.enforceReadLimit(w, claims.Subject)")
	pin := strings.Index(body, `if fingerprint == "" {`)
	dial := strings.Index(body, "sshclient.Dial(ctx, host, port, user, cred, fingerprint)")
	if limit < 0 || pin < 0 || dial < 0 || !(limit < pin && pin < dial) {
		t.Fatal("serviceStatus must rate-limit and refuse to dial without an enrolled host key")
	}
}

// 2026-11-03：节点写锁在被长时间占用时，交互请求必须能随 ctx 放弃等待。
func TestNodeWriteLockHonorsContext(t *testing.T) {
	s := &Server{}
	release := s.lockNodeWrite("n1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.lockNodeWriteCtx(ctx, "n1"); err == nil {
		t.Fatal("a held node lock must not be acquired once the context expires")
	}
	release()
	again, err := s.lockNodeWriteCtx(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	again()
	if !strings.Contains(funcBlock(t, "portalaction.go", "func (s *Server) portalNodeAction("), "s.lockNodeWriteCtx(lockCtx, nodeID)") {
		t.Fatal("interactive portal actions must use the context-aware lock")
	}
}
