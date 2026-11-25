package httpapi

import (
	"strings"
	"testing"
)

func TestCapabilityInventoryRetainsNodesWithUnreadableCredentials(t *testing.T) {
	body := funcBlock(t, "capabilities.go", "func (s *Server) loadNodeCapabilities(")
	for _, want := range []string{
		"if err := rows.Scan(&r.id, &r.name, &r.baseURL, &cipher); err != nil",
		"if err := rows.Err(); err != nil",
		"unavailable = append(unavailable, nodeCapabilities{NodeID: r.id, NodeName: r.name",
		"服务端未配置凭据解密器",
		"节点 API 密钥解密失败",
		"append(unavailable, out...)",
		"item.Available = true",
		"for _, it := range all",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("capability inventory must not silently omit nodes; missing %q", want)
		}
	}
}

func TestOperationalInventoriesRejectPartialRows(t *testing.T) {
	checks := []struct {
		file, function string
		wants          []string
	}{
		{"hostextras.go", "func (s *Server) listServerBilling(", []string{"if err := rows.Err(); err != nil", "s.databaseError(w, r, err)"}},
		{"ippurity.go", "func (s *Server) listIPPurity(", []string{"if err := rows.Err(); err != nil", "s.databaseError(w, r, err)"}},
		{"unlock.go", "func (s *Server) listUnlock(", []string{"if err := rows.Err(); err != nil", "s.databaseError(w, r, err)"}},
	}
	for _, check := range checks {
		body := funcBlock(t, check.file, check.function)
		for _, want := range check.wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s must reject partial inventory rows; missing %q", check.file, want)
			}
		}
	}
}

func TestSpeedtestRunRejectsIncompleteTargetInventoryBeforeQueueing(t *testing.T) {
	body := funcBlock(t, "monitoring.go", "func (s *Server) speedtestRun(")
	errCheck := strings.Index(body, "if err := rows.Err(); err != nil")
	queue := strings.Index(body, "go func() {")
	if errCheck < 0 || queue < 0 || errCheck > queue {
		t.Fatal("manual speedtest must reject incomplete target reads before acknowledging work")
	}
	if !strings.Contains(body, "if err := rows.Scan(&serverID, &serverName, &nodeID, &nodeName, &baseURL, &ciphertext, &hasCred); err != nil") {
		t.Fatal("manual speedtest must fail instead of skipping an unscannable target")
	}
}

func TestScheduledProbeRoundsDoNotUseIncompleteTargetLists(t *testing.T) {
	delay := funcBlock(t, "monitoring_schedule.go", "func (s *Server) probeNetworkLatency(")
	if !strings.Contains(delay, "rows.Err(); err != nil") || !strings.Contains(delay, "跳过本轮延迟探测") {
		t.Fatal("scheduled latency probe must skip an incomplete host inventory")
	}
	speed := funcBlock(t, "monitoring_schedule.go", "func (s *Server) runSpeedtestRound(")
	if !strings.Contains(speed, "rows.Err(); err != nil") || !strings.Contains(speed, "scheduleFailed(ctx)") || !strings.Contains(speed, "跳过本轮测速") {
		t.Fatal("scheduled speedtest must record failure and not probe an incomplete target inventory")
	}
}

func TestTelegramHostAlertActionsRejectPartialNodeLists(t *testing.T) {
	body := funcBlock(t, "telegram_alert.go", "func (s *Server) serverAlertNodeButtons(")
	if !strings.Contains(body, "rows.Scan(&id, &name); err != nil") || !strings.Contains(body, "rows.Err(); err != nil") {
		t.Fatal("host alert must not send a partial set of node action buttons")
	}
}

func TestTelegramStatusListsRejectPartialRows(t *testing.T) {
	for _, function := range []string{"func (s *Server) telegramNodes(", "func (s *Server) telegramAlerts("} {
		body := funcBlock(t, "telegram.go", function)
		if !strings.Contains(body, "rows.Scan(") || !strings.Contains(body, "rows.Err()") {
			t.Errorf("%s must reject incomplete rows", function)
		}
	}
}
