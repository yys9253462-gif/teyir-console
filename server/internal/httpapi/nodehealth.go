package httpapi

import (
	"fmt"
	"time"
)

// nodeStaleAfter 超过这么久没有一次成功检测，就不能再相信库里的 status。
//
// 为什么需要它（2026-10-14 菲律宾-马尼拉实测）：采集器 SSH 连不上主机时只把
// servers.status 置 offline，hysteria_nodes.status 不会动 —— 节点页于是一直
// 显示上一次的「在线」，最后检测时间停在 19 小时前也没人发现。
// 采集周期默认 30 秒，5 分钟 = 连续约 10 轮没更新，足以排除偶发抖动。
const nodeStaleAfter = 5 * time.Minute

// nodeHealth 给节点页一个「能不能用」的结论与原因，供列表标红与排查。
// 返回的 reason 只在 ok=false 时有意义。
func nodeHealth(status, statusError string, lastCheckedAt *time.Time, now time.Time) (bool, string) {
	if status != "online" {
		label := map[string]string{"warning": "不稳定", "offline": "离线", "unknown": "状态未知"}[status]
		if label == "" {
			label = status
		}
		if statusError != "" {
			return false, label + "：" + statusError
		}
		return false, label
	}
	if lastCheckedAt == nil {
		return false, "从未检测成功"
	}
	if age := now.Sub(*lastCheckedAt); age > nodeStaleAfter {
		return false, fmt.Sprintf("已 %s 未检测成功，显示的状态可能过期（主机 SSH 或节点 API 不可达）", humanDuration(age))
	}
	return true, ""
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%d 天 %d 小时", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%d 小时 %d 分钟", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
}
