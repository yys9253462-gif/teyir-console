package httpapi

import (
	"strings"
	"testing"
)

func TestPortalBatchMeterVerdict(t *testing.T) {
	type src = struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	mk := func(init, ok bool, sources map[string]src) portalBundle {
		var b portalBundle
		b.MeterInitialized = init
		b.Measurement.OK = ok
		b.Measurement.Sources = sources
		return b
	}
	if w, _ := portalBatchMeterVerdict(mk(false, false, nil)); !strings.Contains(w, "未初始化") {
		t.Errorf("uninitialized meter must warn, got %q", w)
	}
	if w, _ := portalBatchMeterVerdict(mk(true, false, map[string]src{"xray": {OK: false, Error: "stats api down"}})); !strings.Contains(w, "xray stats api down") {
		t.Errorf("a currently failing source must warn and be named, got %q", w)
	}
	// 马尼拉②：当前都在采，只有历史缺口 → 成功 + 提示，不能告警。
	w, note := portalBatchMeterVerdict(mk(true, false, map[string]src{"hysteria": {OK: true}}))
	if w != "" || !strings.Contains(note, "历史统计缺口") {
		t.Errorf("historical gap only must be a note, got warning=%q note=%q", w, note)
	}
	if w, note := portalBatchMeterVerdict(mk(true, true, map[string]src{"hysteria": {OK: true}})); w != "" || note != "" {
		t.Errorf("healthy meter: want no warning/note, got %q / %q", w, note)
	}
}
