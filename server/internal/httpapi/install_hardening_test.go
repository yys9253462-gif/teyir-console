package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"nodeops/server/internal/ippurity"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 证书邮箱改成白名单：制表符、引号、空白、shell 元字符都必须被拒。
func TestValidCertEmailWhitelist(t *testing.T) {
	for _, ok := range []string{"ops@example.com", "a.b+tag@sub.example.co", "x_y%z-1@a-b.io"} {
		if !validCertEmail(ok) {
			t.Errorf("合法邮箱被拒：%q", ok)
		}
	}
	for _, bad := range []string{"", "a@b", "a\t@example.com", "a@example.com\t", "a b@example.com", "a'@example.com",
		"a@example.com;id", "$(id)@example.com", "a`id`@example.com", "a@example.com\n", "a\\@example.com",
		strings.Repeat("a", 120) + "@example.com", ".a@example.com", "a.@example.com", "a..b@example.com",
		"a@-example.com", "a@example-.com", "a@example..com", "a@" + strings.Repeat("a", 64) + ".com", strings.Repeat("a", 65) + "@example.com"} {
		if validCertEmail(bad) {
			t.Errorf("非法邮箱被放行：%q", bad)
		}
	}
}

// 环境变量值逐个单引号转义；整条命令作为一个参数交给 bash -c。
func TestInstallScriptCommandQuotesEnv(t *testing.T) {
	cmd := installScriptCommand("sudo -n ", [][2]string{{"HY2_DOMAIN", "a.example.com"}, {"HY2_EMAIL", "x'; touch /tmp/pwn; '"}})
	if !strings.HasPrefix(cmd, "bash -c '") {
		t.Fatalf("外层必须是 bash -c '<单个参数>'：%s", cmd)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("没有 bash，跳过实际解析校验")
	}
	// 用真实 bash 解析外层：拿到 bash -c 的那一个参数，确认带引号的恶意值仍是一个完整的赋值。
	inner := strings.TrimPrefix(cmd, "bash -c ")
	out, err := exec.Command(bash, "-c", "printf '%s' "+inner).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `HY2_EMAIL='x'\''; touch /tmp/pwn; '\'''`) {
		t.Fatalf("内层命令里的邮箱没有被单引号包住：%s", out)
	}
	// 再解析内层的 env 赋值部分：值必须原样还原，且不产生额外命令。
	got, err := exec.Command(bash, "-c", "printf '%s' "+shellQuote("x'; touch /tmp/pwn; '")).Output()
	if err != nil || string(got) != "x'; touch /tmp/pwn; '" {
		t.Fatalf("shellQuote 没有原样还原：%q %v", got, err)
	}
}

func TestInstallJobPasswordShownOnceAndEvicted(t *testing.T) {
	now := time.Now()
	ended := now.Add(-time.Minute)
	st := &installJobStore{jobs: map[string]*installJob{}, byServer: map[string]string{}, owned: map[string]bool{}}
	st.jobs["run"] = &installJob{ID: "run", Status: "running", Access: &installAccess{URL: "u", Password: "p1"}}
	st.jobs["done"] = &installJob{ID: "done", Status: "success", EndedAt: &ended, Access: &installAccess{URL: "u", Password: "p2"}}

	// 运行中：可以反复拿到密码。
	for i := 0; i < 2; i++ {
		if j, _ := st.snapshotForClient("run", now); j.Access.Password != "p1" {
			t.Fatal("运行中的任务不应抹掉密码")
		}
	}
	// 已结束：第一次返回密码，之后抹掉。
	if j, _ := st.snapshotForClient("done", now); j.Access == nil || j.Access.Password != "p2" {
		t.Fatal("任务结束后第一次读取应带出密码")
	}
	if j, _ := st.snapshotForClient("done", now); j.Access.Password != "" {
		t.Fatal("密码在结束后被读取过一次就必须清除")
	}

	// 没人来取的，30 分钟后抹掉；1 小时后整条任务淘汰。
	old := now.Add(-45 * time.Minute)
	st.jobs["stale"] = &installJob{ID: "stale", Status: "failed", EndedAt: &old, Access: &installAccess{Password: "p3"}}
	st.evictLocked(now)
	if st.jobs["stale"].Access.Password != "" {
		t.Fatal("超过 30 分钟未读取的密码必须清除")
	}
	older := now.Add(-2 * time.Hour)
	st.jobs["gone"] = &installJob{ID: "gone", Status: "success", EndedAt: &older}
	st.owned["gone"] = true
	if _, ok := st.snapshotForClient("gone", now); ok {
		t.Fatal("结束超过 1 小时的任务应从内存淘汰")
	}
	if st.owned["gone"] {
		t.Fatal("淘汰时 owned 标记也要清掉")
	}
	if _, ok := st.jobs["run"]; !ok {
		t.Fatal("运行中的任务不能被淘汰")
	}
}

func TestChangeServerIPRequiresDangerousConfirm(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/servers/x/change-ip", strings.NewReader(`{"ip":"1.2.3.4"}`))
	s.changeServerIP(rec, req)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("缺少 X-Confirm-Dangerous 时必须 428，得到 %d", rec.Code)
	}
}

// 下面几条依赖数据库/SSH，没有测试库时钉住源码形状。
func TestChangeServerIPChecksLeasesAndReportsDNSFailure(t *testing.T) {
	body := funcBlock(t, "hostextras.go", "func (s *Server) changeServerIP(")
	for _, want := range []string{"serverHasActiveHostJob", "StatusConflict", "rows.Err()", "dns_ok", "Failed"} {
		if !strings.Contains(body, want) {
			t.Errorf("changeServerIP 缺少 %s", want)
		}
	}
	if strings.Contains(body, "if err == nil {") {
		t.Error("节点查询失败不能再被静默忽略")
	}
	lease := funcBlock(t, "hostextras.go", "func (s *Server) serverHasActiveHostJob(")
	if !strings.Contains(lease, "host_operation_leases") || !strings.Contains(lease, "installJobs.byServer") {
		t.Error("换 IP 必须同时检查内存任务与持久化租约")
	}
}

func TestInstallRefusesDomainOfOtherLiveNodeBeforeDNS(t *testing.T) {
	body := funcBlock(t, "installhy2.go", "func (s *Server) startInstallHy2(")
	i := strings.Index(body, "lower(rtrim(domain,'.')) = lower(rtrim($1,'.')) AND server_id <> $2::uuid")
	if i < 0 || !strings.Contains(body, "deleted_at IS NULL AND lower(rtrim(domain") {
		t.Fatal("一键安装必须拒绝已被其他主机在用节点占用的域名")
	}
	if j := strings.Index(body, "go s.runInstallHy2("); j < 0 || j < i {
		t.Fatal("域名占用检查必须在启动任务（自动 DNS）之前")
	}
}

func TestHostJobOutputMasksPasswordAndSecrets(t *testing.T) {
	for _, secret := range []string{"密　码：abc123", "密  码: abc123", "password=abc123", `{"api_key":"abc123"}`, "vless://abc123", "private_key=abc123", "hy2_sec_abc123"} {
		if out := maskHostJobOutput("普通状态\n" + secret); strings.Contains(out, "abc123") || !strings.Contains(out, "普通状态") {
			t.Errorf("secret leaked or useful log lost: %q", out)
		}
	}
}

func TestChangeServerIPLeaseConflictAndPartialDNS(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	for _, query := range []string{
		`ALTER TABLE servers ADD COLUMN updated_at timestamptz DEFAULT now()`,
		`INSERT INTO servers(id,host) VALUES('` + fixHost + `','8.8.8.8')`,
		`INSERT INTO hysteria_nodes(id,server_id,name,domain) VALUES('` + fixNode + `','` + fixHost + `','fixture','hy2.example.com')`,
	} {
		if _, err := db.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	s.purityKeys = ippurity.NewKeyStore(nil)
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/servers/"+fixHost+"/change-ip", strings.NewReader(`{"ip":"1.1.1.1","update_dns":true}`))
		r.SetPathValue("id", fixHost)
		r.Header.Set("X-Confirm-Dangerous", "yes")
		w := httptest.NewRecorder()
		s.changeServerIP(w, r)
		return w
	}
	installJobs.mu.Lock()
	oldBusy, hadBusy := installJobs.byServer[fixHost]
	installJobs.byServer[fixHost] = "test-running"
	installJobs.mu.Unlock()
	t.Cleanup(func() {
		installJobs.mu.Lock()
		if hadBusy {
			installJobs.byServer[fixHost] = oldBusy
		} else {
			delete(installJobs.byServer, fixHost)
		}
		installJobs.mu.Unlock()
	})
	if w := call(); w.Code != http.StatusConflict {
		t.Fatalf("memory job: %d %s", w.Code, w.Body)
	}
	installJobs.mu.Lock()
	delete(installJobs.byServer, fixHost)
	installJobs.mu.Unlock()
	if _, err := db.Exec(ctx, `INSERT INTO host_operation_jobs(id,payload,status,owner) VALUES('test-lease','{}','interrupted','test'); INSERT INTO host_operation_leases(server_id,job_id) VALUES('`+fixHost+`','test-lease')`); err != nil {
		t.Fatal(err)
	}
	if w := call(); w.Code != http.StatusConflict {
		t.Fatalf("durable lease: %d %s", w.Code, w.Body)
	}
	if _, err := db.Exec(ctx, `DELETE FROM host_operation_leases`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('00000000-0000-0000-0000-000000000104'); INSERT INTO hysteria_nodes(id,server_id,name,domain) VALUES('00000000-0000-0000-0000-000000000105','00000000-0000-0000-0000-000000000104','other','HY2.EXAMPLE.COM.')`); err != nil {
		t.Fatal(err)
	}
	if w := call(); w.Code != http.StatusConflict {
		t.Fatalf("domain conflict: %d %s", w.Code, w.Body)
	}
	var unchanged string
	if err := db.QueryRow(ctx, `SELECT host FROM servers WHERE id=$1::uuid`, fixHost).Scan(&unchanged); err != nil || unchanged != "8.8.8.8" {
		t.Fatalf("conflict changed host: %s %v", unchanged, err)
	}
	if _, err := db.Exec(ctx, `UPDATE hysteria_nodes SET deleted_at=now() WHERE id='00000000-0000-0000-0000-000000000105'`); err != nil {
		t.Fatal(err)
	}
	w := call() // 未配置 Cloudflare，地址写成功，DNS 必须如实报失败。
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("partial DNS: %d %s", w.Code, w.Body)
	}
	var body struct {
		DNSOK bool `json:"dns_ok"`
		DNS   []struct {
			Failed bool `json:"failed"`
		} `json:"dns"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.DNSOK || len(body.DNS) != 1 || !body.DNS[0].Failed {
		t.Fatalf("bad response: %s %v", w.Body, err)
	}
	var host string
	if err := db.QueryRow(ctx, `SELECT host FROM servers WHERE id=$1::uuid`, fixHost).Scan(&host); err != nil || host != "1.1.1.1" {
		t.Fatalf("host update lost: %s %v", host, err)
	}
}

func TestMigrationProbesUseSudoAndMaskLogs(t *testing.T) {
	body := funcBlock(t, "migratenode.go", "func (s *Server) runMigrateNode(")
	for _, want := range []string{
		`oldSudo+"test -f /etc/hysteria/xray.json`,
		`newSudo+"test -e /etc/hysteria/config.yaml`,
		"maskInstallSecrets(ansiEscape.ReplaceAllString(",
		"installScriptCommand(newSudo, env)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runMigrateNode 缺少 %s", want)
		}
	}
	start := funcBlock(t, "migratenode.go", "func (s *Server) startMigrateNode(")
	if !strings.Contains(start, "validCertEmail(in.Email)") {
		t.Error("迁移的邮箱必须走白名单校验")
	}
}

// 2026-11-04：面板自己写的修复提示不能被当成凭据整行屏蔽；混进来的真凭据仍要屏蔽。
func TestHostJobOutputKeepsServerWrittenHints(t *testing.T) {
	for _, hint := range []string{
		"SSH 用户 ubuntu 不是 root，且没有免密 sudo。请改用 root 登录，或在主机上执行 echo 'ubuntu ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/90-ubuntu 后重试",
		"自动配置 DNS 失败：Cloudflare 拒绝访问（403）：Token 无效或缺少 Zone.DNS 编辑权限",
		"自动配置 DNS 失败：未配置 Cloudflare API Token",
	} {
		if out := maskHostJobOutput(hint); out != hint {
			t.Errorf("server-written hint was masked: %q -> %q", hint, out)
		}
	}
	if out := maskHostJobOutput("NOPASSWD 提示 password=abc123"); strings.Contains(out, "abc123") {
		t.Errorf("a real secret on the same line must still be masked: %q", out)
	}
}
