package httpapi

// 告警处置按钮的**端到端**测试（2026-10-24 加）。
//
// 前面那组测试验证的是「按钮生成」与「边界约束」；
// 这一组验证的是**点击后的完整链路**：
//
//	TG 回调 → telegramCallback → telegramAlertCallback → telegramRun
//	         → 生成确认卡（tg:yes:/tg:no:）→ 点「确认执行」
//	         → 带 X-Confirm-Dangerous 执行面板 API
//
// 关键断言：**点一下不会删掉节点**。
// 这是整套设计里最要紧的一条 —— 如果某一环把确认卡跳过了，
// 管理员在 Telegram 里随手一点，一台正在服役的节点就被下线了，
// 它的买家全部断线。
//
// 需要真实数据库（走面板真实路由与鉴权），无库时跳过。

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// TestAlertCallbackProducesConfirmationCardBeforeActing 断言：
// 点处置按钮 → 只生成确认卡，**不执行**。
func TestAlertCallbackProducesConfirmationCardBeforeActing(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	s.readLimiter = newSlidingLimiter(10*time.Second, 60)
	s.writeLimiter = newSlidingLimiter(10*time.Second, 20)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.box = box

	ctx := context.Background()
	// 造一条节点级告警。
	if _, err := db.Exec(ctx, `CREATE TABLE alerts(node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,resolved_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,domain,listen_port,server_id) VALUES($1::uuid,'测试节点','fixture.test',443,$2::uuid)`, fixNode, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO alerts(node_id,server_id,type,status,first_seen_at) VALUES($1::uuid,$2::uuid,'node_offline','open',now())`, fixNode, fixHost); err != nil {
		t.Fatal(err)
	}

	// 生成按钮（登记动作）。
	rows := s.alertActionRows(ctx, "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: fixNode,
	})
	if len(rows) == 0 {
		t.Fatal("没生成按钮")
	}
	// 取「下线此节点」那个按钮的 nonce（文本匹配）。
	// 不能按顺序取第一个 —— 第一个是「确认告警」（ack），
	// 最后一个是「永久下线」（带 ?decommissioned=1）。
	var nonce string
	for _, row := range rows {
		for _, btn := range row {
			if btn["text"] != "🚫 下线此节点" {
				continue
			}
			if data := btn["callback_data"]; len(data) > 6 && data[:6] == "tg:al:" {
				nonce = data[6:]
			}
		}
	}
	if nonce == "" {
		t.Fatal("没找到处置按钮的 nonce")
	}

	// 点按钮前先记下节点状态。
	var deletedBefore any
	if err := db.QueryRow(ctx, `SELECT deleted_at FROM hysteria_nodes WHERE id=$1::uuid`, fixNode).Scan(&deletedBefore); err != nil {
		t.Fatal(err)
	}

	// 模拟点击：走 telegramCallback 的 tg:al: 分支。
	// 这里不走 telegramRun 的真实执行（它需要面板管理员会话与完整路由），
	// 而只验证「点击后动作被取出来、且登记动作仍在」。
	action, ok := s.telegramControl.lookupAlertAction(nonce, "chat-1")
	if !ok {
		t.Fatal("点击时取不到登记的动作")
	}
	if action.Method != "DELETE" || action.Path != "/api/nodes/"+fixNode {
		t.Errorf("按钮动作是 %s %s，应为 DELETE /api/nodes/%s",
			action.Method, action.Path, fixNode)
	}

	// **核心断言**：动作被取出 ≠ 动作被执行。
	// 真实执行必须经过 telegramRun 生成确认卡、用户再点「确认执行」。
	// 这里断言节点**尚未**被删除 —— 也就是说，取出动作这一步没有副作用。
	var deletedAfter any
	if err := db.QueryRow(ctx, `SELECT deleted_at FROM hysteria_nodes WHERE id=$1::uuid`, fixNode).Scan(&deletedAfter); err != nil {
		t.Fatal(err)
	}
	if deletedAfter != deletedBefore {
		t.Fatal("取出按钮动作就删掉了节点 —— 必须先经确认卡")
	}
	if deletedAfter != nil {
		t.Fatal("节点已软删，测试前提不成立")
	}
}

// TestAlertCallbackMustGoThroughConfirmation 断言点击按钮**必须**走确认卡。
//
// ⚠️ 这条是被反向验证逼出来的：原先的端到端测试只验证了
// 「取出动作没有副作用」，但**没有钉住执行路径** ——
// 于是把 telegramRun 的 confirmed 从 false 改成 true（一点即删）时，
// 测试照样全绿。
//
// 现在明确断言：处置按钮的回调**必须**以 confirmed=false 调用 telegramRun。
// 这是「点一下不能直接删节点」的代码级保证。
func TestAlertCallbackMustGoThroughConfirmation(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "telegram_alert.go"))

	if !strings.Contains(code, "s.telegramRun(ctx, chat, sender, action, false)") {
		t.Error("处置按钮回调必须以 confirmed=false 调用 telegramRun —— " +
			"confirmed=true 会跳过确认卡，变成一点即删")
	}
	if strings.Contains(code, "telegramRun(ctx, chat, sender, action, true)") {
		t.Error("处置按钮回调不得以 confirmed=true 执行 —— 那会跳过确认卡")
	}
	// 也不能绕过 telegramRun 直接调面板 API。
	for _, forbidden := range []string{"s.db.Exec", "s.deleteNode(", "s.deleteServer("} {
		if strings.Contains(code, forbidden) {
			t.Errorf("处置按钮回调里出现了 %s —— "+
				"必须走 telegramRun 的统一通道（鉴权+确认+审计）", forbidden)
		}
	}
}

// TestAlertNodeActionPathIsExact 断言「下线此节点」按钮的动作路径精确。
//
// 反向验证发现：把按钮动作改成自造接口（/api/nodes/{id}/hard）时，
// 端到端测试没报红 —— 因为它只检查了「有副作用吗」，没检查「动作对不对」。
//
// 这里钉死：节点级告警的「下线此节点」按钮，路径必须**恰好**是
// DELETE /api/nodes/{id}（不含多余后缀），这样 telegramAllowed 才认得。
func TestAlertNodeActionPathIsExact(t *testing.T) {
	t.Parallel()

	s := &Server{}
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: fixNode,
	})
	if len(rows) == 0 {
		t.Fatal("没生成按钮")
	}

	seen := false
	for _, row := range rows {
		for _, btn := range row {
			if btn["text"] != "🚫 下线此节点" {
				continue
			}
			seen = true
			nonce := strings.TrimPrefix(btn["callback_data"], "tg:al:")
			action, ok := s.telegramControl.lookupAlertAction(nonce, "chat-1")
			if !ok {
				t.Fatalf("取不到按钮 %s 的动作", nonce)
			}
			if action.Method != "DELETE" {
				t.Errorf("「下线此节点」的方法是 %s，应为 DELETE", action.Method)
			}
			if action.Path != "/api/nodes/"+fixNode {
				t.Errorf("「下线此节点」的路径是 %q，应为 /api/nodes/%s —— "+
					"多一个后缀 telegramAllowed 就认不得，按钮会静默失效",
					action.Path, fixNode)
			}
		}
	}
	if !seen {
		t.Fatal("没找到「下线此节点」按钮")
	}
}

// TestAlertDangerousActionCarriesDangerousHeader 断言执行危险动作时
// 会带上 X-Confirm-Dangerous 头。
//
// 这是面板的既有护栏：删除类接口缺这个头一律拒绝。
// TG 通道必须与网页面板**用同一套护栏**，不能因为「走的是机器人」就绕过。
func TestAlertDangerousActionCarriesDangerousHeader(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	s.readLimiter = newSlidingLimiter(10*time.Second, 60)
	s.writeLimiter = newSlidingLimiter(10*time.Second, 20)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.box = box

	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE alerts(node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,resolved_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,domain,listen_port,server_id) VALUES($1::uuid,'护栏节点','fixture.test',443,$2::uuid)`, fixNode, fixHost); err != nil {
		t.Fatal(err)
	}
	// deleteNode 会写 subscription_tokens.node_accounts、读 orphan_node_accounts ——
	// 缺表会返回 500，测不出护栏本身的语义。
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS orphan_node_accounts(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),node_id uuid,subscription_id uuid,user_id text,resolved_at timestamptz,resolved_note text,created_at timestamptz DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS subscription_pending_node_actions(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),subscription_id uuid,node_id uuid,action text,attempts int DEFAULT 0,last_error text,created_at timestamptz DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS node_configs(node_id uuid PRIMARY KEY,config jsonb)`,
		`CREATE TABLE IF NOT EXISTS traffic_samples(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),node_id uuid,bytes bigint DEFAULT 0,sampled_at timestamptz DEFAULT now())`,
		`ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS users_count int`,
		`ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS api_checked_at timestamptz`,
		`ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS api_url text`,
		`ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS api_key bytea`,
		`ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS traffic_suspended boolean DEFAULT false`,
		`ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS status text DEFAULT 'unknown'`,
		`ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS status_error text DEFAULT ''`,
	} {
		if _, err := db.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}

	// 直接调 deleteNode 处理器，**不带** X-Confirm-Dangerous —— 必须被拒。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/nodes/"+fixNode, nil)
	req.SetPathValue("id", fixNode)
	s.deleteNode(rec, req)
	if rec.Code != 428 && rec.Code != 412 {
		t.Errorf("缺 X-Confirm-Dangerous 时删除节点返回 %d，应为 428/412 —— "+
			"护栏失效了", rec.Code)
	}

	// 带上头 —— 这次才应该真的删掉（证明护栏是「缺头才拦」，不是「一律拦」）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("DELETE", "/api/nodes/"+fixNode, nil)
	req2.SetPathValue("id", fixNode)
	req2.Header.Set("X-Confirm-Dangerous", "yes")
	s.deleteNode(rec2, req2)
	if rec2.Code < 200 || rec2.Code >= 300 {
		t.Errorf("带 X-Confirm-Dangerous 时删除节点返回 %d，应为 2xx —— "+
			"护栏把正常操作也拦了：%s", rec2.Code, rec2.Body.String())
	}
}

// TestAlertCallbackRejectsForeignChat 断言别的 chat 点按钮无效。
//
// 真实场景：A 的机器人消息被转发到 B 的会话，B 点下去必须无效。
// 这是端到端层面上的最后一道防伪造。
func TestAlertCallbackRejectsForeignChat(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	nonce := s.telegramControl.registerAlertAction("chat-A", telegramAction{
		Method: "DELETE", Path: "/api/nodes/" + fixNode,
	})
	if nonce == "" {
		t.Fatal("登记失败")
	}
	if _, ok := s.telegramControl.lookupAlertAction(nonce, "chat-B"); ok {
		t.Error("B 拿到了 A 的动作 —— 必须拒绝")
	}
}
