package httpapi

import (
	"testing"
)

// 订阅下发集合的判据（2026-10-03 修 BUG 后更新）。
//
// ⚠️ 夹具说明：原夹具里的 adminRec 是 `&subscriptionRecord{OrderID: ""}`
// —— **没有任何 nodeAccounts**。而现实中管理员自用订阅同样会在每个在役
// 节点上开专属账号（needsAccountProvisioning 对它一样生效），
// 实测库里那条手工订阅就有 4 个节点的账号。
//
// 所以原夹具是"简化过了头"，它让"没账号也下发"这个错误行为看起来是对的。
// 这次修 BUG 的核心恰恰是补上这条判据：**没有专属账号的节点不得下发**。
// 夹具随之修正为带账号的真实形态。

func storeFleet() []subscriptionNode {
	return []subscriptionNode{
		{ID: "node-store-1", Name: "Tokyo-Store", InStore: true, Password: "SHARED-1"},
		{ID: "node-store-2", Name: "Osaka-Store", InStore: true, Password: "SHARED-2"},
		{ID: "node-private", Name: "Personal-Admin", InStore: false, Password: "SHARED-PRIVATE"},
	}
}

func TestApplySubscriptionAccounts_InStoreFilter(t *testing.T) {
	t.Parallel()

	t.Run("买家商业订单：严格过滤掉未加入商城的节点", func(t *testing.T) {
		buyerRec := &subscriptionRecord{
			OrderID: "SHOP-ORDER-123",
			nodeAccounts: map[string]subscriptionNodeAccount{
				"node-store-1": {UserID: "sub_1", Password: "BUYER-PASS-1"},
				"node-store-2": {UserID: "sub_2", Password: "BUYER-PASS-2"},
				"node-private": {UserID: "sub_priv", Password: "BUYER-PASS-PRIV"},
			},
		}

		got := applySubscriptionAccounts(storeFleet(), buyerRec, nil)
		if len(got) != 2 {
			t.Fatalf("买家订阅应仅包含 2 个商城节点，实际收到 %d 个", len(got))
		}
		for _, n := range got {
			if n.ID == "node-private" || !n.InStore {
				t.Fatalf("买家订阅中绝不应包含私有节点: %s", n.Name)
			}
			if n.Password == "SHARED-1" || n.Password == "SHARED-2" {
				t.Fatalf("下发了共享主口令而不是专属密码: %s", n.Name)
			}
		}
	})

	t.Run("管理员自用订阅：保留全部有账号的节点（含私有节点）", func(t *testing.T) {
		adminRec := &subscriptionRecord{
			OrderID: "", // 无 OrderID，表示管理员面板合并订阅
			nodeAccounts: map[string]subscriptionNodeAccount{
				"node-store-1": {UserID: "sub_a", Password: "ADMIN-PASS-1"},
				"node-store-2": {UserID: "sub_b", Password: "ADMIN-PASS-2"},
				"node-private": {UserID: "sub_c", Password: "ADMIN-PASS-PRIV"},
			},
		}

		got := applySubscriptionAccounts(storeFleet(), adminRec, nil)
		if len(got) != 3 {
			t.Fatalf("管理员自用订阅应保留全部 3 个节点，实际收到 %d 个", len(got))
		}
	})
}

// ⚠️ 本次修 BUG 的核心断言：**没有专属账号的节点绝不下发**。
//
// 原实现只看 isBuyer/in_store，于是洛杉矶-NTT 出现在手工订阅配置里，
// 而它上面 sub_ 账号数 = 0、用的是节点主口令 —— 客户端连上必被拒
// （实测报 authentication error）。用户看到的是"导入的节点用不了"，
// 而真实原因是"这个节点压根没给这条订阅开号"。
//
// 宁可少下发一个节点，也不要给客户端一个连不上的节点：
// 后者会让买家以为整个订阅坏了。
func TestApplySubscriptionAccounts_SkipsNodesWithoutAccount(t *testing.T) {
	t.Parallel()

	fleet := storeFleet()

	t.Run("管理员订阅：缺账号的节点被跳过", func(t *testing.T) {
		rec := &subscriptionRecord{
			OrderID: "",
			nodeAccounts: map[string]subscriptionNodeAccount{
				// 只有 store-1 和 private 有账号；store-2 没有。
				"node-store-1": {UserID: "sub_a", Password: "P1"},
				"node-private": {UserID: "sub_c", Password: "P3"},
			},
		}
		got := applySubscriptionAccounts(fleet, rec, nil)
		if len(got) != 2 {
			t.Fatalf("应只下发有账号的 2 个节点，实际 %d 个", len(got))
		}
		for _, n := range got {
			if n.ID == "node-store-2" {
				t.Fatal("node-store-2 没有专属账号，绝不应下发 —— " +
					"客户端会拿它连一个不认识的账号，报 authentication error")
			}
		}
	})

	t.Run("买家订阅：同样跳过缺账号的节点", func(t *testing.T) {
		rec := &subscriptionRecord{
			OrderID: "SHOP-1",
			nodeAccounts: map[string]subscriptionNodeAccount{
				"node-store-1": {UserID: "sub_a", Password: "P1"},
				// store-2 缺账号
			},
		}
		got := applySubscriptionAccounts(fleet, rec, nil)
		if len(got) != 1 || got[0].ID != "node-store-1" {
			t.Fatalf("应只下发 node-store-1，实际 %d 个", len(got))
		}
	})

	t.Run("没有任何账号时一个都不下发", func(t *testing.T) {
		rec := &subscriptionRecord{OrderID: ""}
		if got := applySubscriptionAccounts(fleet, rec, nil); len(got) != 0 {
			t.Fatalf("无账号时不应下发任何节点，实际 %d 个", len(got))
		}
	})

	t.Run("账号缺密码时也不下发", func(t *testing.T) {
		// user_id 有但 password 为空 —— 半开状态，同样连不上。
		rec := &subscriptionRecord{
			OrderID: "",
			nodeAccounts: map[string]subscriptionNodeAccount{
				"node-store-1": {UserID: "sub_a", Password: ""},
				"node-store-2": {UserID: "", Password: "P2"},
			},
		}
		if got := applySubscriptionAccounts(fleet, rec, nil); len(got) != 0 {
			t.Fatalf("账号不完整（缺密码或缺 id）时不应下发，实际 %d 个", len(got))
		}
	})

	t.Run("下发的节点必须带上专属密码，不能是主口令", func(t *testing.T) {
		rec := &subscriptionRecord{
			OrderID: "",
			nodeAccounts: map[string]subscriptionNodeAccount{
				"node-store-1": {UserID: "sub_a", Password: "OWN-PASS"},
			},
		}
		got := applySubscriptionAccounts(fleet, rec, nil)
		if len(got) != 1 {
			t.Fatalf("期望 1 个节点，实际 %d", len(got))
		}
		if got[0].Password != "OWN-PASS" {
			t.Fatalf("必须用订阅专属密码，实际 %q（主口令 = 客户端连不上）", got[0].Password)
		}
		if got[0].AccountUserID != "sub_a" {
			t.Fatalf("必须带上账号 user_id 以便回收，实际 %q", got[0].AccountUserID)
		}
	})
}

// 密码必须是**确定性**的：同一 (订阅, 节点) 永远得到同一个值。
//
// 这是修另一个 BUG 的关键 —— 节点侧 /users/create 不带 password 时会
// 随机生成，而它对已存在的 user_id 是覆盖语义。于是重复开户会换密码，
// 库里记的旧密码失效 → 认证失败（实测哥伦布-AWS 就是这个原因）。
func TestProvisionAccountPasswordIsDeterministic(t *testing.T) {
	t.Parallel()

	rec := &subscriptionRecord{ID: "11111111-2222-3333-4444-555555555555"}
	same := &subscriptionRecord{ID: "11111111-2222-3333-4444-555555555555"}
	other := &subscriptionRecord{ID: "99999999-8888-7777-6666-555555555555"}

	a := provisionAccountPassword(rec, "node-A")
	b := provisionAccountPassword(same, "node-A")
	if a != b {
		t.Fatalf("同一 (订阅, 节点) 必须得到同一密码：%q vs %q", a, b)
	}
	// 同一订阅在不同节点上必须不同 —— 否则一个节点上的密码泄漏
	// 会让攻击者拿到另一节点上的账号。
	if a == provisionAccountPassword(rec, "node-B") {
		t.Error("同一订阅在不同节点上必须用不同密码")
	}
	// 同一节点上不同订阅必须不同 —— 否则一个买家能用另一个买家的账号。
	if a == provisionAccountPassword(other, "node-A") {
		t.Error("同一节点上不同订阅必须用不同密码")
	}
	// 长度与字符集：portal 侧只要求是字符串，但固定 32 位 hex 最稳。
	if len(a) != 32 {
		t.Errorf("密码长度应为 32（16 字节 hex），实际 %d", len(a))
	}
	// 不能把订阅 uuid 明文写进密码（那会泄漏订阅 id 到节点侧）。
	if len(rec.ID) >= 8 && (a == rec.ID || (len(a) > 8 && a[:8] == rec.ID[:8])) {
		t.Error("密码里不应出现订阅 id 的明文前缀")
	}
}
