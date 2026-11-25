package httpapi

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestApplySubscriptionAccounts exercises the layer that turns the shared fleet
// build into a per-buyer config. Getting this wrong is not a cosmetic bug: if
// the shared password survives, expiry is unenforceable, which is the entire
// reason provisioning exists.
func TestApplySubscriptionAccounts(t *testing.T) {
	t.Parallel()

	fleet := []subscriptionNode{
		{ID: "node-a", Name: "a", Host: "a.example.com", Port: 19984, Password: "SHARED"},
		{ID: "node-b", Name: "b", Host: "b.example.com", Port: 19984, Password: "SHARED"},
		{ID: "node-c", Name: "c", Host: "c.example.com", Port: 19984, Password: "SHARED"},
	}

	t.Run("own password overrides the shared one", func(t *testing.T) {
		rec := &subscriptionRecord{nodeAccounts: map[string]subscriptionNodeAccount{
			"node-a": {UserID: "sub_x", Password: "OWN-A"},
			"node-b": {UserID: "sub_y", Password: "OWN-B"},
		}}
		got := applySubscriptionAccounts(fleet, rec, nil)

		// ⚠️ 2026-10-03 变更：node-c 没有专属账号 → **不再下发**。
		//
		// 原测试断言"未开户的节点保留共享主口令作为兜底"
		// （注释原文：so a paid customer keeps a working config），
		// 理由是"宁可给个可能不通的节点，也别少一个"。
		//
		// 实测证明那个取舍是错的：客户端拿主口令去连，节点侧不认识 →
		// 报 `authentication error, HTTP status code: 200`。
		// 买家看到的是"这个节点用不了"，而真实原因是"没给它开号"——
		// 报错完全指不出方向。宁可少一个节点，也不要给一个连不上的。
		if len(got) != 2 {
			t.Fatalf("只应下发有专属账号的 2 个节点，实际 %d 个", len(got))
		}
		if got[0].Password != "OWN-A" || got[1].Password != "OWN-B" {
			t.Fatalf("own passwords not applied: %q %q", got[0].Password, got[1].Password)
		}
		if got[0].AccountUserID != "sub_x" {
			t.Fatalf("account id not carried: %q", got[0].AccountUserID)
		}
		for _, n := range got {
			if n.ID == "node-c" {
				t.Fatal("node-c 没有专属账号，绝不应下发 —— " +
					"用它会让客户端拿主口令去连一个不认识的账号")
			}
		}
	})

	t.Run("the cached build is never mutated", func(t *testing.T) {
		rec := &subscriptionRecord{nodeAccounts: map[string]subscriptionNodeAccount{
			"node-a": {UserID: "sub_x", Password: "OWN-A"},
		}}
		_ = applySubscriptionAccounts(fleet, rec, nil)

		// A second subscriber must not inherit the first subscriber's password.
		// This is the failure mode that would hand one buyer another's account.
		if fleet[0].Password != "SHARED" {
			t.Fatalf("applySubscriptionAccounts mutated the shared build: %q", fleet[0].Password)
		}
	})

	t.Run("legacy subscription with no accounts gets nothing", func(t *testing.T) {
		// ⚠️ 2026-10-03 变更：旧行（node_accounts 为空）原本会拿到全部节点的
		// 共享主口令。现在一个都不下发 —— 那些账号在节点侧并不属于它，
		// 拿主口令连上去是 admin_master 身份，与"这条订阅"无关。
		rec := &subscriptionRecord{}
		got := applySubscriptionAccounts(fleet, rec, nil)
		if len(got) != 0 {
			t.Fatalf("没有任何专属账号时不应下发任何节点，实际 %d 个", len(got))
		}
	})

	t.Run("an account with a blank password is not served", func(t *testing.T) {
		// ⚠️ 2026-10-03 变更：半写入的行（有 user_id 没密码）原本回退到共享
		// 主口令。现在直接不下发 —— "回退到主口令"等于把一个不属于它的
		// 身份发给客户端，比少一个节点坏得多。
		rec := &subscriptionRecord{nodeAccounts: map[string]subscriptionNodeAccount{
			"node-a": {UserID: "sub_x", Password: ""},
		}}
		got := applySubscriptionAccounts(fleet, rec, nil)
		if len(got) != 0 {
			t.Fatalf("密码为空的账号不应下发（回退主口令等于发错身份），实际 %d 个", len(got))
		}
	})
}

func TestSubscriptionToNodeDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		exp  *time.Time
		want int
	}{
		{"perpetual gets a long finite window", nil, 3650},
		{"exact 7 days stays 7", ptrTime(now.Add(7 * 24 * time.Hour)), 7},
		{"partial day rounds up", ptrTime(now.Add(25 * time.Hour)), 2},
		{"one hour left still gets a day", ptrTime(now.Add(time.Hour)), 1},
		{"already expired floors at 1", ptrTime(now.Add(-time.Hour)), 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rec := &subscriptionRecord{ExpiresAt: tc.exp}
			if got := subscriptionToNodeDays(rec, now); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}

	// A perpetual subscription must NOT be handed to the node as "no expiry":
	// portal.py would default it to 30 days, and an account that outlives its
	// subscription is the exact bug this feature removes.
	t.Run("perpetual is finite, not unbounded", func(t *testing.T) {
		rec := &subscriptionRecord{}
		if got := subscriptionToNodeDays(rec, now); got <= 0 {
			t.Fatalf("perpetual produced a non-positive window: %d", got)
		}
	})
}

func TestParseNodeAccounts(t *testing.T) {
	t.Parallel()

	t.Run("round trip", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]subscriptionNodeAccount{
			"n1": {UserID: "u1", Password: "p1", CreatedAt: 123},
		})
		got := parseNodeAccounts(raw)
		if len(got) != 1 || got["n1"].UserID != "u1" || got["n1"].CreatedAt != 123 {
			t.Fatalf("bad round trip: %+v", got)
		}
	})

	// Every malformed input must degrade to "no accounts" rather than failing
	// the load. This column is allowed to be absent, and a delivery page must
	// not go down because of it.
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"empty object", []byte(`{}`)},
		{"garbage", []byte(`{"n1": `)},
		{"wrong shape", []byte(`[1,2,3]`)},
	} {
		tc := tc
		t.Run("malformed: "+tc.name, func(t *testing.T) {
			if got := parseNodeAccounts(tc.raw); got != nil {
				t.Fatalf("expected nil, got %+v", got)
			}
		})
	}
}

// TestProvisionUserIDShape pins the id against portal.py's validator,
// ^[A-Za-z0-9._-]{1,64}$. An id the node rejects turns every order into a
// provisioning failure, and the buyer would silently receive the shared
// password again.
func TestProvisionUserIDShape(t *testing.T) {
	t.Parallel()
	rec := &subscriptionRecord{ID: "00000000-0000-4000-8000-000000000174"}
	got := provisionUserID(rec)
	if len(got) > 64 {
		t.Fatalf("id too long for portal.py: %d chars (%q)", len(got), got)
	}
	for _, r := range got {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !ok {
			t.Fatalf("id has a character portal.py rejects: %q in %q", r, got)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestPortalUserCreateResponseParsesRealNodePayload pins the exact payload
// portal.py returns.
//
// The bug this guards against took provisioning out entirely and silently:
// portal.py computes traffic_gb as `float(...)`, so it serialises as `0.0`, and
// decoding that into a plain `int` failed the whole response. The node said
// "ok", we logged "节点返回的开户结果无法解析", and the buyer quietly received the
// node's SHARED password — i.e. the P0 came straight back, with no visible
// error anywhere on the delivery page.
func TestPortalUserCreateResponseParsesRealNodePayload(t *testing.T) {
	t.Parallel()

	// Copied verbatim from a live node (fields truncated where irrelevant).
	raw := `{
	  "ok": true,
	  "user_id": "sub_probe_shape",
	  "password": "00112233445566778899aabbccddeeff",
	  "ip_limit": 2,
	  "traffic_gb": 0.0,
	  "expires_at": 1790826690,
	  "uri": "hysteria2://00112233445566778899aabbccddeeff@node2.example.com:19984?sni=node2.example.com",
	  "reality_uri": "",
	  "clash": "{\"mixed-port\": 7890}",
	  "sing_box": "{\"outbounds\": []}"
	}`

	var parsed portalUserCreateResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("real node payload failed to decode: %v", err)
	}
	if !parsed.OK || parsed.UserID == "" || parsed.Password == "" {
		t.Fatalf("payload decoded but fields are empty: %+v", parsed)
	}
	if parsed.IPLimit != 2 {
		t.Fatalf("ip_limit wrong: %d", parsed.IPLimit)
	}
	if parsed.ExpiresAt != 1790826690 {
		t.Fatalf("expires_at wrong: %d", parsed.ExpiresAt)
	}
}

// TestFlexIntAcceptsBothNumberShapes covers the tolerant decoder directly.
func TestFlexIntAcceptsBothNumberShapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want int
	}{
		{`0.0`, 0},
		{`0`, 0},
		{`5`, 5},
		{`5.0`, 5},
		{`5.9`, 5},
		{`-3`, -3},
		{`null`, 0},
		{`""`, 0},
		{`"abc"`, 0},
	} {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			var f flexInt
			if err := json.Unmarshal([]byte(tc.in), &f); err != nil {
				t.Fatalf("flexInt should never error, got %v", err)
			}
			if int(f) != tc.want {
				t.Fatalf("flexInt(%s) = %d, want %d", tc.in, int(f), tc.want)
			}
		})
	}
}

// TestNoReclaimPathDeletesWithoutRevoking is a source-level guard on the one
// invariant this whole feature rests on:
//
//	a subscription row must never be removed without first revoking the
//	buyer's node-side accounts.
//
// The row is the only place the account ids are recorded. Delete it first and
// the account is unreachable-but-live — the buyer keeps working past expiry
// with a password we handed them. We shipped exactly that bug three times in a
// row (the original shared-password design, the sweeper's bulk DELETE, and the
// operator's delete button), each time because a new reclaim path was written
// without noticing it had to revoke.
//
// A behavioural test per path would not have caught the third one: the path did
// not exist yet. This checks the shape of the code instead, so a future path is
// covered the moment it is written.
func TestNoReclaimPathDeletesWithoutRevoking(t *testing.T) {
	t.Parallel()

	// Deletions of subscription rows that are allowed to stand alone. Every one
	// of these is either not a reclaim (test helpers) or explicitly paired with
	// a revocation call the reader can see within the same function.
	//
	// If you are adding an entry here, stop: adding a reclaim path that deletes
	// without revoking is the bug. Call purgeSubscriptionByRecord(rec) instead,
	// which revokes first and deletes second.
	allowedSolo := map[string]string{
		// purgeSubscriptionByRecord revokes on the line above the delete.
		"purgeSubscriptionByRecord": "revokes immediately before deleting",
	}

	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("cannot read store.go: %v", err)
	}

	// Split into top-level functions so we can ask "does this function delete a
	// subscription, and does the same function also revoke?".
	funcs := splitGoFuncs(string(src))
	deletesRE := regexp.MustCompile(`(?s)DELETE\s+FROM\s+subscription_tokens`)
	revokesRE := regexp.MustCompile(`revokeSubscriptionAccounts|purgeSubscriptionByRecord`)

	checked := 0
	for name, body := range funcs {
		if !deletesRE.MatchString(body) {
			continue
		}
		checked++
		if reason, ok := allowedSolo[name]; ok {
			t.Logf("%s: deletes without revoking, explicitly allowed (%s)", name, reason)
			continue
		}
		if !revokesRE.MatchString(body) {
			t.Errorf("%s deletes a subscription row but never revokes its node-side "+
				"accounts.\n"+
				"That orphans the buyer's account on the node: still authenticating, "+
				"under a password they already hold. Revoke first (call "+
				"purgeSubscriptionByRecord), then delete.", name)
		}
	}

	if checked == 0 {
		t.Fatal("found no subscription deletions at all — this guard is not " +
			"looking at what it thinks it is; the SQL may have been renamed")
	}
	t.Logf("checked %d function(s) that delete subscriptions", checked)
}

// splitGoFuncs maps top-level function name -> body for a Go source file. It is
// deliberately simple: these files are gofmt'd, so a top-level `func ` at column
// zero starts a function and the next such line ends it.
func splitGoFuncs(src string) map[string]string {
	lines := strings.Split(src, "\n")
	funcStart := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	out := map[string]string{}

	current := ""
	var body strings.Builder
	flush := func() {
		if current != "" {
			out[current] = body.String()
		}
		body.Reset()
	}
	for _, line := range lines {
		if m := funcStart.FindStringSubmatch(line); m != nil {
			// New top-level function: close the previous one.
			flush()
			current = m[1]
			continue
		}
		if current != "" {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	flush()
	return out
}
