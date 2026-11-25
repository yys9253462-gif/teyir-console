package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-11-06：解锁检测拿不到节点凭据时照实说原因：安装 / 迁移中不能说成「没有可用的节点 API」。
func TestUnlockCheckReportsBusyNodeHonestly(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	ctx := context.Background()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`CREATE TABLE IF NOT EXISTS node_unlock(node_id uuid PRIMARY KEY, results jsonb, error text, checked_at timestamptz, updated_at timestamptz)`, nil},
		{`INSERT INTO servers(id) VALUES($1::uuid)`, []any{fixHost}},
		{`INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'n','https://8.8.8.8:8443',''::bytea,'fixture.test',443,$2::uuid)`, []any{fixNode, fixHost}},
		{`INSERT INTO host_operation_jobs(id,payload,status,owner) VALUES('fixture-job','{}'::jsonb,'running','fixture')`, nil},
		{`INSERT INTO host_operation_leases(server_id,job_id) VALUES($1::uuid,'fixture-job')`, []any{fixHost}},
	} {
		if _, err := db.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.checkAndStoreUnlock(ctx, fixNode)
	if !errors.Is(err, errPortalNodeBusy) {
		t.Fatalf("安装 / 迁移中应如实报告，实际：%v", err)
	}
}

// 2026-11-06：机器人不能封内网 / 回环地址（代理链配错时告警里记下的是 docker 网关或 127.0.0.1）。
func TestTelegramRefusesToBanPrivateAddresses(t *testing.T) {
	for _, ip := range []string{"172.19.0.1", "127.0.0.1", "10.0.0.5", "::1", "fe80::1"} {
		if !isPrivateOrLoopbackIP(ip) {
			t.Fatalf("%s 应被识别为内网 / 回环", ip)
		}
	}
	confirm := funcBlock(t, "telegram_control.go", "func (s *Server) telegramBanConfirm(")
	if !strings.Contains(confirm, "!isUnban && isPrivateOrLoopbackIP(ip)") {
		t.Fatal("发起封禁确认前必须拒绝内网 / 回环地址")
	}
	cb := funcBlock(t, "telegram_control.go", "func (s *Server) telegramCallback(")
	at := strings.Index(cb, "parseBanConfirm(data, time.Now())")
	guard := strings.Index(cb, "!isUnban && isPrivateOrLoopbackIP(ip)")
	exec := strings.Index(cb, "s.telegramBanIPAction(ctx, chat, ip, isUnban)")
	if at < 0 || guard < at || exec < guard {
		t.Fatal("执行封禁前（确认回调里）也要再拦一次内网 / 回环地址：旧消息里的确认按钮仍可能带着它")
	}
}

// 2026-11-06：登录失败记在受信任反代地址上时不封禁（否则反代没带 XFF 时全体用户被锁在外面）。
func TestLoginFailuresOnTrustedProxyAreNotBanned(t *testing.T) {
	_, proxyNet, _ := net.ParseCIDR("172.19.0.0/16")
	s := &Server{trustedProxyCIDRs: []*net.IPNet{proxyNet}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	const proxyIP, realIP = "172.19.0.1", "198.51.100.79"
	reset := func() {
		loginGuardState.mu.Lock()
		for _, ip := range []string{proxyIP, realIP} {
			delete(loginGuardState.fails, ip)
			delete(loginGuardState.banned, ip)
		}
		loginGuardState.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	for i := 0; i < loginFailMax+2; i++ {
		s.onLoginFailure(proxyIP, "x@y")
		s.onLoginFailure(realIP, "x@y")
	}
	if !loginGuardState.bannedUntil(proxyIP, time.Now()).IsZero() {
		t.Fatal("受信任的反代地址不能被封禁")
	}
	if loginGuardState.bannedUntil(realIP, time.Now()).IsZero() {
		t.Fatal("真实来源地址照常封禁")
	}
}

// 2026-11-06：主机有安装 / 迁移任务时不能删 SSH 凭据。
func TestDeleteCredentialRefusedDuringHostJob(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 1000)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS server_credentials(server_id uuid, auth_type text, ciphertext bytea, created_at timestamptz DEFAULT now())`,
		`INSERT INTO servers(id) VALUES('` + fixHost + `')`,
		`INSERT INTO server_credentials(server_id,auth_type,ciphertext) VALUES('` + fixHost + `','password','\x01')`,
		`INSERT INTO host_operation_jobs(id,payload,status,owner) VALUES('fixture-job','{}'::jsonb,'running','fixture')`,
		`INSERT INTO host_operation_leases(server_id,job_id) VALUES('` + fixHost + `','fixture-job')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/servers/"+fixHost+"/credential", nil)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	req.SetPathValue("id", fixHost)
	rec := httptest.NewRecorder()
	s.deleteCredential(rec, asUser(req, adminA))
	if rec.Code != http.StatusConflict {
		t.Fatalf("主机有任务时应回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM server_credentials WHERE server_id=$1::uuid`, fixHost).Scan(&n); err != nil || n != 1 {
		t.Fatalf("凭据不能被删：n=%d err=%v", n, err)
	}
}
