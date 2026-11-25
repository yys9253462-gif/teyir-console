package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/ippurity"
	"nodeops/server/internal/secrets"
)

// 2026-11-04：机器人可以改主机名称 / 标签，但不能改 SSH 地址 / 端口 / 用户或重置指纹 ——
// 那会清空指纹，采集器按首次信任把已存的 SSH 凭据发给新地址。
func TestTelegramCannotChangeSSHEndpoint(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret"})
	path := "/api/servers/00000000-0000-0000-0000-000000000001"
	for _, body := range []string{`{"host":"203.0.113.9"}`, `{"ssh_port":2222}`, `{"ssh_user":"x"}`, `{"reset_host_key":true}`, `{"name":"a","host":"evil.example"}`, `not json`,
		// 2026-11-06：updateServer 解码字段名不分大小写，守卫也必须一样。
		`{"Host":"1.2.3.4"}`, `{"host":null,"HOST":"1.2.3.4"}`, `{"SSH_PORT":22}`, `{"Reset_Host_Key":true}`} {
		if !telegramTouchesSSHEndpoint(s, telegramAction{Method: "PATCH", Path: path, Body: []byte(body)}) {
			t.Errorf("机器人不能提交 %s", body)
		}
	}
	for _, body := range []string{`{"name":"东京"}`, `{"tags":["a"],"region":"jp"}`, `{"reset_host_key":false,"host":null}`, ``} {
		if telegramTouchesSSHEndpoint(s, telegramAction{Method: "PATCH", Path: path, Body: []byte(body)}) {
			t.Errorf("日常字段应可在机器人里修改: %s", body)
		}
	}
	if telegramTouchesSSHEndpoint(s, telegramAction{Method: "PATCH", Path: "/api/nodes/00000000-0000-0000-0000-000000000001", Body: []byte(`{"host":"x"}`)}) {
		t.Error("只针对主机接口")
	}
}

func TestTelegramRunTimeoutCoversPortalActions(t *testing.T) {
	// 2026-11-06：最慢的可达接口是重型 root 操作（620 秒），不是普通写操作（120 秒）。
	if telegramRunTimeout <= portalHeavyInstallTimeout+15e9 {
		t.Fatalf("机器人执行时限 %v 必须盖住重型 root 操作预算 %v + 等锁 15s", telegramRunTimeout, portalHeavyInstallTimeout)
	}
}

func TestTelegramVerbChoicesAreRealVerbs(t *testing.T) {
	s := &Server{}
	choices := s.telegramWizardOptions(context.Background(), &telegramWizard{}, telegramFormField{Name: "verb", Type: "string", Location: "body"})
	if len(choices) != len(portalActions) {
		t.Fatalf("应列出全部 %d 个节点操作，实际 %d", len(portalActions), len(choices))
	}
	for _, c := range choices {
		spec, ok := portalActions[c.Value]
		if !ok || c.Label == "" || c.Label != spec.Label {
			t.Errorf("按钮必须是可提交的操作键并带说明: %+v", c)
		}
	}
}

func TestPortalTargetBusyIsConflictAndMalformedIDIsNotFound(t *testing.T) {
	s := fixServer(fixDB(t))
	if _, err := s.loadPortalTarget(context.Background(), "not-a-uuid"); !errors.Is(err, errPortalNodeNotFound) {
		t.Fatalf("畸形 ID 应是「节点不存在」: %v", err)
	}
	if _, err := s.loadPortalWebTarget(context.Background(), "not-a-uuid"); !errors.Is(err, errPortalNodeNotFound) {
		t.Fatalf("畸形 ID 应是「节点不存在」: %v", err)
	}
	rec := httptest.NewRecorder()
	s.writePortalWebTargetError(rec, errPortalNodeBusy)
	if rec.Code != http.StatusConflict {
		t.Fatalf("安装 / 迁移中应回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 2026-11-04：同时提交多个密钥、其中一个格式不对时，一个都不能存（原来按 map 随机顺序逐个写库）。
func TestPurityKeysPutIsAllOrNothing(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 1000)
	s.box, _ = secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.purityKeys = ippurity.NewKeyStore(nil)
	for i := 0; i < 20; i++ { // map 顺序随机：多跑几次，覆盖「合法的那个先被遍历」
		body := `{"` + ippurity.KeyIPAPIIs + `":"validkey12345","` + ippurity.KeyIPQS + `":"bad key!"}`
		req := asUser(httptest.NewRequest(http.MethodPut, "/api/settings/purity-keys", strings.NewReader(body)), adminA)
		rec := httptest.NewRecorder()
		s.purityKeysPut(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("格式错误应回 400，实际 %d %s", rec.Code, rec.Body.String())
		}
		var n int
		if err := db.QueryRow(context.Background(), `SELECT count(*) FROM system_settings WHERE key LIKE 'ip_purity_key_%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 || s.purityKeys.Get(ippurity.KeyIPAPIIs) != "" {
			t.Fatalf("第 %d 次：有字段格式错误时不能保存任何密钥（库里 %d 条）", i+1, n)
		}
	}
}

// 2026-11-06：确认卡要带上查询参数与对象名；「永久下线」要明确写出来，不能与普通下线长得一样。
func TestTelegramConfirmCardShowsQueryAndTarget(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret"})
	text := s.telegramActionText(context.Background(), telegramAction{Method: "DELETE", Path: "/api/nodes/00000000-0000-0000-0000-000000000001?decommissioned=1"})
	if !strings.Contains(text, "永久下线") || !strings.Contains(text, "decommissioned=1") {
		t.Fatalf("the confirmation card must spell out a permanent decommission, got %q", text)
	}
	if !strings.Contains(text, "对象：") {
		t.Fatalf("the target line must survive a query string, got %q", text)
	}
}

// 2026-11-06：菜单按钮必须带接口短哈希，下标移位或旧按钮不能打开别的接口。
func TestTelegramRouteButtonsCarryPatternTag(t *testing.T) {
	a, b := telegramRouteTag("GET /api/nodes"), telegramRouteTag("GET /api/servers")
	if len(a) != 8 || a == b {
		t.Fatalf("route tags must be 8 hex chars and differ per pattern: %q %q", a, b)
	}
	menu := funcBlock(t, "telegram_control.go", "func (s *Server) telegramRouteMenu(")
	if !strings.Contains(menu, `"tg:r:"+strconv.Itoa(idx)+":"+telegramRouteTag(r.Pattern)`) {
		t.Fatal("menu buttons must carry the route tag")
	}
	cb := funcBlock(t, "telegram_control.go", "func (s *Server) telegramCallback(")
	start := strings.Index(cb, `case strings.HasPrefix(data, "tg:r:"):`)
	wizard := strings.Index(cb[start:], "s.telegramWizardStart(ctx, chat, index)")
	if start < 0 || wizard < 0 || !strings.Contains(cb[start:start+wizard], "tag != telegramRouteTag(") {
		t.Fatal("the callback must verify the tag before starting the wizard")
	}
}
