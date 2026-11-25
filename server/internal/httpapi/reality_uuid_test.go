// uuid5 跨语言契约测试：Go 侧实现必须与 portal.py 的
// uuid.uuid5(uuid.NAMESPACE_URL, 'hy2-portal-reality:'+user_id) 完全一致。
//
// 为什么必须对拍：节点侧 xray.json 里的 client id 由 Python 生成，
// 面板侧订阅里的 UUID 由 Go 生成。两者只要差一个字节，客户端就会被
// 服务端拒绝，而报错只有 authentication failed —— 极难定位。
package httpapi

import "testing"

func TestRealityUUIDForUserMatchesPythonUUID5(t *testing.T) {
	// 期望值由 Python 标准库 uuid.uuid5(uuid.NAMESPACE_URL, 'hy2-portal-reality:' + uid) 生成。
	cases := []struct {
		userID string
		want   string
	}{
		{"admin_master", "a45f2360-5dcf-5922-8fbf-fabf5cf367a2"},
		{"user_demo_01", "48b72b77-9bdc-5c8f-9ad5-9bd91ddf539c"},
		{"hy2_demo_0001", "ddb5bcb8-e8a1-5722-bc9f-74f232d6bc48"},
		{"hy2_demo_0002", "913e4c86-ad4f-59b4-8512-0b2e8a43a4ed"},
		{"sub_00000000-0000-4000-8000-000000000001", "6f30259a-41c1-51aa-b134-6bb624fddd13"},
	}
	for _, c := range cases {
		got := realityUUIDForUser(c.userID)
		if got != c.want {
			t.Fatalf("realityUUIDForUser(%q) = %s, want %s", c.userID, got, c.want)
		}
		if len(got) != 36 || got[14] != '5' || !(got[19] == '8' || got[19] == '9' || got[19] == 'a' || got[19] == 'b') {
			t.Fatalf("%s: UUIDv5 形态非法 (%s)", c.userID, got)
		}
	}

	// 幂等：同一 user_id 反复调用结果必须一致（订阅可重复拉取的前提）。
	first := realityUUIDForUser("hy2_demo_0001")
	for i := 0; i < 5; i++ {
		if got := realityUUIDForUser("hy2_demo_0001"); got != first {
			t.Fatalf("不幂等：第 %d 次得到 %s，首次 %s", i, got, first)
		}
	}

	// 不同用户必须得到不同 UUID（多用户隔离的前提）。
	if realityUUIDForUser("a") == realityUUIDForUser("b") {
		t.Fatal("不同 user_id 不应产生相同 UUID")
	}
}

func TestUUIDHelpersRoundTrip(t *testing.T) {
	// NAMESPACE_URL 解析必须正确
	ns := parseUUIDBytes("6ba7b811-9dad-11d1-80b4-00c04fd430c8")
	want := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1,
		0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	for i := range want {
		if ns[i] != want[i] {
			t.Fatalf("NAMESPACE_URL 解析错误 at %d: got %02x want %02x", i, ns[i], want[i])
		}
	}

	// 格式化：必须是小写 8-4-4-4-12
	var b [16]byte
	for i := range b {
		b[i] = byte(i)
	}
	got := formatUUID(b)
	const wantStr = "00010203-0405-0607-0809-0a0b0c0d0e0f"
	if got != wantStr {
		t.Fatalf("formatUUID 错误: got %s want %s", got, wantStr)
	}

	// 非法输入不应 panic
	if len(parseUUIDBytes("not-a-uuid")) != 16 {
		t.Fatal("非法输入应返回 16 字节")
	}
}

func TestRealityReady(t *testing.T) {
	if (&nodeReality{}).ready() {
		t.Fatal("空结构不应 ready")
	}
	var nilR *nodeReality
	if nilR.ready() {
		t.Fatal("nil 不应 ready")
	}
	ok := &nodeReality{PublicKey: "pk", ShortID: "sid", DestSNI: "sni", Port: 443}
	if !ok.ready() {
		t.Fatal("完整参数应 ready")
	}
	for _, bad := range []*nodeReality{
		{ShortID: "sid", DestSNI: "sni", Port: 443},
		{PublicKey: "pk", DestSNI: "sni", Port: 443},
		{PublicKey: "pk", ShortID: "sid", Port: 443},
		{PublicKey: "pk", ShortID: "sid", DestSNI: "sni", Port: 0},
		{PublicKey: "pk", ShortID: "sid", DestSNI: "sni", Port: 70000},
	} {
		if bad.ready() {
			t.Fatalf("缺参数不应 ready: %+v", bad)
		}
	}
}
