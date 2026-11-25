package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nodeops/server/internal/secrets"
)

// 2026-11-12：确认未接入节点接口的哨兵错误可被 errors.Is 识别，定时巡检据此跳过重试。
func TestUnlockNoAPIErrorIsSentinel(t *testing.T) {
	if errUnlockNoAPI == nil || !strings.Contains(errUnlockNoAPI.Error(), "没有可用的节点 API") {
		t.Fatalf("未接入接口哨兵错误异常: %v", errUnlockNoAPI)
	}
	wrapped := errors.Join(errors.New("context"), errUnlockNoAPI)
	if !errors.Is(wrapped, errUnlockNoAPI) {
		t.Fatal("包装后的 errUnlockNoAPI 必须能被 errors.Is 识别")
	}
}

// 2026-11-12：同一台机器重新接入后，补删孤儿账号前必须识别出该账号正被在役订阅使用。
func TestOrphanAccountInUseDetectsReattachedMachine(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)
	box, err := secrets.NewBox("BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := box.Seal([]byte("portal-key-fixture-only"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id, name, api_url, api_key) VALUES($1::uuid, 'live-node', 'https://node.example.com:8443/', $2)`, fixNode, cipher); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts='{"`+fixNode+`":{"user_id":"sub_fixture","password":"p"}}' WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box

	// 地址大小写与末尾斜杠不同，仍应判定为同一台机器。
	got, err := s.orphanAccountInUse(ctx, "HTTPS://NODE.EXAMPLE.COM:8443", "other-key", "sub_fixture")
	if err != nil || got != "live-node" {
		t.Fatalf("同地址不同写法应命中在役节点，实际 got=%q err=%v", got, err)
	}
	// 地址不同但门户密钥相同，同样应判定为同一台机器。
	got, err = s.orphanAccountInUse(ctx, "https://other.example.com:8443", "portal-key-fixture-only", "sub_fixture")
	if err != nil || got != "live-node" {
		t.Fatalf("同密钥应命中在役节点，实际 got=%q err=%v", got, err)
	}
	// 地址与密钥均不匹配时，允许执行补删。
	got, err = s.orphanAccountInUse(ctx, "https://another.example.com:8443", "another-key", "sub_fixture")
	if err != nil || got != "" {
		t.Fatalf("不同机器不应阻断补删，实际 got=%q err=%v", got, err)
	}
}

// 2026-11-12：未绑定节点接口的节点在检测解锁时必须返回 errUnlockNoAPI。
func TestCheckAndStoreUnlockReturnsErrUnlockNoAPI(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id, name) VALUES($1::uuid, 'no-api-node')`, fixNode); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	_, err := s.checkAndStoreUnlock(ctx, fixNode)
	if !errors.Is(err, errUnlockNoAPI) {
		t.Fatalf("未接入节点接口时应返回 errUnlockNoAPI，实际 err=%v", err)
	}
}