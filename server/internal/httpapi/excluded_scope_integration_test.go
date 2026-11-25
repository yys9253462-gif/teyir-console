package httpapi

import (
	"context"
	"testing"
)

// 2026-11-03：交付页「暂时不可用」只列买家本该拿到的节点：
// 已下架（买家订阅）或已移出分类的节点，即使映射里还留着旧账号，也不能出现。
func TestExcludedInScopeAppliesSameRulesAsFeed(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const listed = "00000000-0000-0000-0000-0000000d0101"
	const unlisted = "00000000-0000-0000-0000-0000000d0102"
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,in_store) VALUES('`+listed+`','listed',true),('`+unlisted+`','unlisted',false)`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	rec := &subscriptionRecord{ID: fixSub, OrderID: "fixture-order", nodeAccounts: map[string]subscriptionNodeAccount{
		listed:   {UserID: "sub_a", Password: "p"},
		unlisted: {UserID: "sub_a", Password: "p"},
	}}
	got := s.excludedInScope(ctx, []subscriptionExcluded{{NodeID: listed, Name: "listed"}, {NodeID: unlisted, Name: "unlisted"}, {NodeID: "00000000-0000-0000-0000-0000000d0103", Name: "no-account"}}, rec)
	if len(got) != 1 || got[0].NodeID != listed {
		t.Fatalf("buyer delivery page must only list in-store nodes it has an account on, got %+v", got)
	}
	// 直接圈定节点时，范围外的节点也不能出现。
	rec.NodeIDs = []string{unlisted}
	rec.OrderID = ""
	got = s.excludedInScope(ctx, []subscriptionExcluded{{NodeID: listed, Name: "listed"}, {NodeID: unlisted, Name: "unlisted"}}, rec)
	if len(got) != 1 || got[0].NodeID != unlisted {
		t.Fatalf("node-scoped subscription must only list its scoped nodes, got %+v", got)
	}
}
