package httpapi

import (
	"strings"
	"testing"
)

func TestChinaLossAlertTexts(t *testing.T) {
	msg := describeAlert("node_china_loss", "东京", "1.2.3.4", "日本-东京", 443)
	if !strings.Contains(msg, "日本-东京") || !strings.Contains(msg, "30%") || strings.Contains(msg, "%!") {
		t.Fatalf("故障文案不对：%s", msg)
	}
	rec := describeAlertRecovery("node_china_loss", "东京", "1.2.3.4", "日本-东京", 443)
	if !strings.Contains(rec, "15%") || strings.Contains(rec, "%!") {
		t.Fatalf("恢复文案不对：%s", rec)
	}
}

func TestChinaLossConditionsSQLContract(t *testing.T) {
	sql := chinaLossConditionsSQL
	for _, want := range []string{
		"carrier IN ('telecom','unicom','mobile')", // 公共 DNS 不代表回国线路
		"n.status = 'online'",                      // 离线已有 node_offline
		"a.type = 'node_blocked'",                  // 被墙已是 critical，不重复推送
		"a.type = 'node_china_loss'",               // 已开告警按恢复线判定（滞回）
		"r.rounds >= $3",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL 缺少 %q", want)
		}
	}
	if chinaLossRecoverPct >= chinaLossTriggerPct {
		t.Fatal("恢复线必须低于触发线，否则没有滞回")
	}
}

func TestChinaQualityRangesBounded(t *testing.T) {
	for key, spec := range chinaQualityRanges {
		if n := int(spec.window / spec.bucket); n < 50 || n > 250 {
			t.Errorf("%s 点数 %d 不在合理范围", key, n)
		}
	}
}
