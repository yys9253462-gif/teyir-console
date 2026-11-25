package httpapi

import (
	"encoding/json"
	"time"
)

// 当前采集状态与历史缺口分开；缺口不会伪装成现在断网。
func measurementHealth(sources, gaps json.RawMessage, sampled time.Time, initialized bool) (bool, int) {
	var history []json.RawMessage
	if err := json.Unmarshal(gaps, &history); err != nil {
		// Keep current-health evaluation independent; malformed history stays visible as a gap.
		history = []json.RawMessage{json.RawMessage(`{}`)}
	}
	if !initialized || time.Since(sampled) > 90*time.Second {
		return false, len(history)
	}
	var health map[string]struct {
		OK        bool   `json:"ok"`
		SampledAt string `json:"sampled_at"`
	}
	if json.Unmarshal(sources, &health) != nil || len(health) == 0 {
		return false, len(history)
	}
	for _, source := range health {
		when, err := time.Parse(time.RFC3339Nano, source.SampledAt)
		if !source.OK || err != nil || time.Since(when) > 90*time.Second {
			return false, len(history)
		}
	}
	return true, len(history)
}
