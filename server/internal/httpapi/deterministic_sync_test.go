package httpapi

import (
	"errors"
	"testing"
)

// 2026-11-06：等也不会自愈的失败（401/403/端点不存在/版本过低/密钥解不开）判为确定性拒绝，
// 让其余节点已交付的订阅降级为 degraded，不再把订单永久钉在 pending；拿不准的仍按可恢复处理。
func TestDeterministicSyncFailureClassification(t *testing.T) {
	yes := []string{
		"节点 API 报错: 401 unauthorized",
		"节点 API 报错: 403 forbidden",
		"API endpoint not found",
		"API 密钥解密失败",
		"节点门户版本过低，不支持整机流量计量",
		"节点 portal 尚未提供测速端点（需升级 portal）",
	}
	for _, msg := range yes {
		if !deterministicSyncFailure(errors.New(msg)) {
			t.Errorf("should be deterministic: %s", msg)
		}
	}
	no := []string{
		"dial tcp 1.2.3.4:443: i/o timeout",
		"connection refused",
		// 端口号里含 401 / 403 的可恢复错误不能被当成确定性拒绝（否则「暂时连不上」被当成已交付）。
		"dial tcp 1.2.3.4:4013: connection refused",
		"dial tcp 1.2.3.4:4031: connect: network is unreachable",
		"context deadline exceeded",
		"节点上的到期时间与目标不一致（节点 1，目标 2）",
		"\"",
	}
	for _, msg := range no {
		if deterministicSyncFailure(errors.New(msg)) {
			t.Errorf("must stay recoverable: %s", msg)
		}
	}
	if deterministicSyncFailure(nil) {
		t.Error("nil error is not a failure")
	}
}

// 降级判据：全部失败都是确定性拒绝 + 范围内至少一个账号已交付。
func TestAllDeterministicAndDeliveredAccount(t *testing.T) {
	if allDeterministicSyncFailures(nil) {
		t.Fatal("no failures must not count as all-deterministic")
	}
	mixed := []provisionResult{
		{NodeID: "a", Err: errors.New("401 unauthorized")},
		{NodeID: "b", Err: errors.New("i/o timeout")},
	}
	if allDeterministicSyncFailures(mixed) {
		t.Fatal("a recoverable failure must block the degraded path")
	}
	allBad := []provisionResult{{NodeID: "a", Err: errors.New("403 forbidden")}}
	if !allDeterministicSyncFailures(allBad) {
		t.Fatal("all-deterministic must be detected")
	}
	scope := map[string]subscriptionNodeAccount{"a": {}, "c": {UserID: "sub_c"}}
	if !hasDeliveredAccount(scope, allBad) {
		t.Fatal("node c is delivered and not failing")
	}
	if hasDeliveredAccount(map[string]subscriptionNodeAccount{"a": {UserID: "sub_a"}}, allBad) {
		t.Fatal("the only in-scope account is the failing one")
	}
}

// 2026-11-06：属性读回校验必须按 /users/get 的真实结构（{"user":{"expires_at":…}}）比对，
// 按顶层解析会永远得到 0、把每个节点都判成失败。
func TestSyncedAttributeVerificationReusesUserEnvelope(t *testing.T) {
	var want int64 = 1800000000
	if err := verifyAccountExpiry([]byte(`{"ok":true,"user":{"expires_at":1800000000}}`), want); err != nil {
		t.Fatalf("nested user envelope must verify: %v", err)
	}
	if err := verifyAccountExpiry([]byte(`{"ok":true,"expires_at":1800000000}`), want); err == nil {
		t.Fatal("a top-level expires_at is NOT the /users/get shape and must not pass")
	}
}
