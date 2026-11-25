package httpapi

import (
	"strings"
	"testing"
)

func TestExpiredCloseoutRejectsIncompleteDatabaseEvidence(t *testing.T) {
	batch := funcBlock(t, "expired_closeout.go", "func (s *Server) closeExpiredSubscriptionBatchHandler(")
	for _, want := range []string{
		"if err := rows.Scan(&id); err != nil",
		"if err := rows.Err(); err != nil",
		"s.databaseError(w, r, err)",
		"if err != nil {\n\t\t\ts.logger.Warn(\"批量到期结案：读取账号映射失败\"",
		"_, gone, _, err := s.nodeIsGone(ctx, account.NodeID)",
	} {
		if !strings.Contains(batch, want) {
			t.Errorf("batch closeout must fail closed on incomplete evidence; missing %q", want)
		}
	}
	closeOne := funcBlock(t, "expired_closeout.go", "func (s *Server) closeExpiredSubscription(")
	if !strings.Contains(closeOne, "读取节点 %s 状态：%v") || !strings.Contains(closeOne, "return orphans, false, fmt.Errorf(\"%w：读取节点 %s 状态") {
		t.Fatal("single closeout must preserve the subscription when node-state reads fail")
	}
}

func TestTelegramObjectPickerRejectsPartialLists(t *testing.T) {
	body := funcBlock(t, "telegram_ui.go", "func (s *Server) telegramPick(")
	for _, want := range []string{
		"if err := rows.Scan(&id, &name); err != nil",
		"if err := rows.Err(); err != nil",
		"读取对象列表失败，请稍后重试。",
		"WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT 40",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Telegram picker must not present partial DB results as a complete list; missing %q", want)
		}
	}
}

func TestNewIPLoginAlertFailsSafeWhenHistoryCountFails(t *testing.T) {
	body := funcBlock(t, "loginsecurity.go", "func (s *Server) onLoginSuccess(")
	if !strings.Contains(body, "login history count failed") || !strings.Contains(body, "total = 2") {
		t.Fatal("an unreadable login history count must still notify about a newly seen IP")
	}
}

func TestScheduledComparisonsRejectUnreadableSnapshots(t *testing.T) {
	unlock := funcBlock(t, "scheduler.go", "func (s *Server) scheduledUnlock(")
	if !strings.Contains(unlock, "read previous unlock snapshot failed") || !strings.Contains(unlock, "read updated unlock snapshot failed") || !strings.Contains(unlock, "scheduleFailed(ctx)") {
		t.Fatal("unlock comparison must not treat unreadable snapshots as no change")
	}
	purity := funcBlock(t, "scheduler.go", "func (s *Server) scheduledPurity(")
	if !strings.Contains(purity, "read previous IP purity snapshot failed") || !strings.Contains(purity, "scheduleFailed(ctx)") {
		t.Fatal("purity comparison must not discard errors reading the previous state")
	}
}

func TestScheduleConfigurationAndHistoryErrorsAreNotDefaults(t *testing.T) {
	config := funcBlock(t, "scheduler.go", "func (s *Server) scheduleConfig(")
	if !strings.Contains(config, "if err != nil {\n\t\treturn nil, err") || !strings.Contains(config, "invalid schedule config") {
		t.Fatal("corrupt or unreadable schedule configuration must be returned as an error")
	}
	tick := funcBlock(t, "scheduler.go", "func (s *Server) schedulerTick(")
	if !strings.Contains(tick, "cfgErr != nil") || !strings.Contains(tick, "due = nil") {
		t.Fatal("scheduler must not run jobs using defaults when saved settings are unreadable")
	}
	list := funcBlock(t, "scheduler.go", "func (s *Server) listSchedules(")
	if !strings.Contains(list, "s.settingTime(r.Context()") || !strings.Contains(list, "s.databaseError(w, r, err)") {
		t.Fatal("schedule status endpoint must surface unreadable history instead of showing empty history")
	}
}
