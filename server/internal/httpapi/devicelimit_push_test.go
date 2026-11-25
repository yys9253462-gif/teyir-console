package httpapi

import (
	"strings"
	"testing"
	"time"
)

// 下发节奏：集合变了立刻下发；不变时只在超额期间按间隔续期；回到名额内下发一次空列表。
func TestDeviceLimitNeedsPush(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if deviceLimitNeedsPush(deviceLimitPush{}, false, "", now) {
		t.Fatal("从没拦过、现在也不超额，不该下发")
	}
	if !deviceLimitNeedsPush(deviceLimitPush{}, false, "1.1.1.1", now) {
		t.Fatal("新出现超额必须下发")
	}
	prev := deviceLimitPush{ips: "1.1.1.1", at: now.Add(-time.Minute)}
	if deviceLimitNeedsPush(prev, true, "1.1.1.1", now) {
		t.Fatal("集合没变、未到续期时间，不该重复下发")
	}
	if !deviceLimitNeedsPush(deviceLimitPush{ips: "1.1.1.1", at: now.Add(-5 * time.Minute)}, true, "1.1.1.1", now) {
		t.Fatal("超额期间到了续期时间必须续期，否则节点上的拦截会过期")
	}
	if !deviceLimitNeedsPush(prev, true, "", now) {
		t.Fatal("回到名额内必须下发空列表解除")
	}
	if deviceLimitDenyTTL <= int(deviceLimitRefresh.Seconds()) {
		t.Fatal("续期间隔必须短于节点拦截有效期")
	}
	body := funcBlock(t, "devicelimit.go", "func (s *Server) checkDeviceLimits(")
	// 不完整的一轮：已确认的超额照常拦（2026-11-02 起），但只增不减、不释放名额。
	// 行为由 devicelimit_partial_test.go 的数据库集成用例覆盖，这里钉住三处闸门。
	if !strings.Contains(body, "info.complete") || !strings.Contains(body, "observed := !listed || !partial[subID]") ||
		!strings.Contains(body, "deviceLimitUnion(prev.ips, sorted)") || !strings.Contains(body, "if partial[subID] {") {
		t.Fatal("incomplete telemetry must preserve the prior deny list")
	}
	if push := funcBlock(t, "devicelimit.go", "func (s *Server) applyDeviceLimitPush("); !strings.Contains(push, "deviceLimits.blocked[job.subID] = len(job.ips)") || !strings.Contains(body, "s.runDeviceLimitPushes(ctx, jobs, now)") {
		t.Fatal("blocked count must follow a successful portal push")
	}
	if !strings.Contains(body, `mode != "enforce"`) {
		t.Fatal("切回观察时必须解除已下发的拦截")
	}
	watcher := funcBlock(t, "devicelimit.go", "func (s *Server) StartDeviceLimitWatch(")
	if !strings.Contains(watcher, "clearDeviceLimitEnforcement(runCtx)") {
		t.Fatal("关闭模式必须在有界上下文中主动清除已下发的拦截")
	}
	if !strings.Contains(watcher, "mode load failed; preserving current enforcement") {
		t.Fatal("模式读取失败时必须保留现有节点封禁，不能当成 observe 清除")
	}
	mode := funcBlock(t, "devicelimit.go", "func (s *Server) deviceLimitMode(")
	if !strings.Contains(mode, "errors.Is(err, pgx.ErrNoRows)") || !strings.Contains(mode, "invalid device limit mode") {
		t.Fatal("仅缺省设置可使用 observe；非法值和读取失败必须显式报错")
	}
}
