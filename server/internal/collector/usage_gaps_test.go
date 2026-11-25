package collector

import (
	"encoding/json"
	"testing"
)

// 新旧两种 portal 的 users/list 都要读出每个用户的 measurement_gaps。
func TestEnvelopeGapsCompat(t *testing.T) {
	newer := `{"ok":true,"measurement_gaps":[{"source":"reality"}],"users":[{"user_id":"a"},{"user_id":"b","measurement_gaps":[]}]}`
	var env portalUsageEnvelope
	if err := json.Unmarshal([]byte(newer), &env); err != nil {
		t.Fatal(err)
	}
	applyEnvelopeGaps(env.Users, env.MeasurementGaps)
	if string(env.Users[0].MeasurementGaps) != `[{"source":"reality"}]` {
		t.Fatalf("新版 portal：用户没带 gaps 时应取信封里的: %s", env.Users[0].MeasurementGaps)
	}
	if string(env.Users[1].MeasurementGaps) != `[]` {
		t.Fatalf("用户自己带了（哪怕是空数组）就以用户的为准: %s", env.Users[1].MeasurementGaps)
	}

	older := `{"ok":true,"users":[{"user_id":"a","measurement_gaps":[{"source":"hysteria"}]}]}`
	env = portalUsageEnvelope{}
	if err := json.Unmarshal([]byte(older), &env); err != nil {
		t.Fatal(err)
	}
	applyEnvelopeGaps(env.Users, env.MeasurementGaps)
	if string(env.Users[0].MeasurementGaps) != `[{"source":"hysteria"}]` {
		t.Fatalf("旧版 portal 按用户读，不能被改动: %s", env.Users[0].MeasurementGaps)
	}
}

// 节点门户把 measurement_sources 也挪到信封后，面板必须读回来，
// 否则每个订阅都被判成「当前采集异常/未知」。
func TestEnvelopeSourcesCompat(t *testing.T) {
	raw := `{"ok":true,"measurement_sources":{"hysteria":{"ok":true}},"users":[{"user_id":"a","measurement_sources":{}},{"user_id":"b"},{"user_id":"c","measurement_sources":{"reality":{"ok":false}}}]}`
	var env portalUsageEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	applyEnvelopeSources(env.Users, env.MeasurementSources)
	want := `{"hysteria":{"ok":true}}`
	if string(env.Users[0].MeasurementSources) != want || string(env.Users[1].MeasurementSources) != want {
		t.Fatalf("用户没带或带空对象时应取信封里的: %s / %s", env.Users[0].MeasurementSources, env.Users[1].MeasurementSources)
	}
	if string(env.Users[2].MeasurementSources) != `{"reality":{"ok":false}}` {
		t.Fatal("旧门户按用户给的来源不能被覆盖")
	}
}
