package httpapi

// store_contract_test.go — 把「商城（外部商城）依赖的那份
// HTTP 契约」逐字段钉死。
//
// 为什么需要这个文件
// ------------------
// 外部商城节点的适配器模块 里有一个 `panel_type == 'ops'`
// 适配器，靠**四个控制台端点**给客户开户并交付订阅：
//
//	POST  /api/login                        {email,password}          -> {token}
//	GET   /api/nodes                        -> [{id,name,has_api,...}]
//	POST  /api/nodes/{id}/users             {duration_days,ip_limit,
//	                                         traffic_gb,note}         -> 201
//	                                        {user_id,password,uri,clash,sing_box}
//	DELETE /api/nodes/{id}/users/{uid}      + X-Confirm-Dangerous: yes
//	POST  /api/store/orders                 + X-Store-Key
//	                                        {order_id,label,expires_at,ip_limit}
//	                                        -> {subscription_url,clash_url,
//	                                            singbox_url,uri_url,delivery_url}
//
// 这套东西**没有任何编译期保护**：两侧是两个独立仓库、两种语言，
// 中间只有一份隐式的 JSON 约定。控制台这边把 `traffic_gb` 改个名、
// 把 `sing_box` 写成 `singbox`、把 X-Store-Key 换成别的头，
// Go 侧照样编译通过、`go test` 照样全绿 —— 直到客户付了钱拿不到货。
//
// 而商城侧对失败的处理是**静默降级**：`_ops_merged_subscription()` 在
// `res.status_code != 200` 时直接 `return {}`，然后 `delivery['ops_subscription_url']`
// 变成空串，客户拿到的是一条单节点直链而不是合并订阅。
// 「付了钱、拿到个能用的东西、但没有你以为的那个东西」—— 最难被发现的一类。
//
// 所以这里用源码级检查把这个契约冻住（与 store_columns_test.go 同一做法：
// 不依赖数据库，纯静态），并在失败信息里写清「商城哪一行会因此坏掉」。
//
// 改了这些字段就必须同步改商城
// 外部商城的 ops 适配器 ——
// 那是另一台机器上的代码，本仓库改不动它。

import (
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 1. 路由：商城是按固定路径打过来的，改路径等于断链
// ---------------------------------------------------------------------------

func TestShopFacingRoutesAreRegistered(t *testing.T) {
	server := readFileForTest(t, "server.go")

	// 商城调用的路径 -> 商城侧对应的函数/常量
	routes := map[string]string{
		"POST /api/login":                    "ops_login() 换 JWT",
		"GET /api/nodes":                     "ops_nodes() 列节点",
		"POST /api/nodes/{id}/users":         "provision_ops() 开户",
		"DELETE /api/nodes/{id}/users/{uid}": "deprovision_ops() 回收",
		"POST /api/store/orders":             "_ops_merged_subscription() 换合并订阅",
	}
	for route, why := range routes {
		if !strings.Contains(server, `"`+route+`"`) {
			t.Errorf("路由 %q 不见了（商城侧 %s 会直接 404）。\n"+
				"改路由须同步更新外部商城对接适配器",
				route, why)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 开户接口的入参：名字与类型都不能动
// ---------------------------------------------------------------------------

// TestCreateUsersRequestTagsMatchShopPayload 盯住 portalCreateUser 的入参结构体。
//
// 商城 provision_ops() 发的 payload 是：
//
//	{'duration_days': int, 'ip_limit': int, 'traffic_gb': int, 'note': str}
//
// 注意**不带 user_id** —— 控制台必须允许它缺省（第 161 行注释已写明）。
func TestCreateUsersRequestTagsMatchShopPayload(t *testing.T) {
	body := extractFuncBodyFrom(t, readFileForTest(t, "portalusers.go"), "portalCreateUser")

	for _, tag := range []string{"user_id", "duration_days", "ip_limit", "traffic_gb", "note"} {
		if !strings.Contains(body, `json:"`+tag+`"`) {
			t.Errorf("开户入参缺少 json:%q —— 商城 provision_ops() 发的就是这个名字，"+
				"改名后 Go 侧静默忽略该字段（JSON 解码不报错），"+
				"结果是「下单成功但配额/时长没生效」", tag)
		}
	}

	// traffic_gb 必须保持整数类型。
	//
	// 商城那条注释是踩出来的：控制台这个字段是 Go 的 int，
	// 发 0.0 这种浮点会被 JSON 解码直接拒掉，返回一句没有上下文的 "invalid payload"。
	// 反过来，如果谁把控制台这边改成 float64，商城发整数仍然能过 ——
	// 但语义会漂（流量按 GB 还是按字节？），所以把类型也钉住。
	trafficField := regexp.MustCompile(`TrafficGB\s+(\S+)`).FindStringSubmatch(body)
	if trafficField == nil {
		t.Fatalf("portalCreateUser 里找不到 TrafficGB 字段 —— 被改名或删掉了")
	}
	if trafficField[1] != "int" {
		t.Errorf("TrafficGB 的类型从 int 变成了 %s。\n"+
			"商城侧按整数发（node_service.py 里有明确注释），类型一变，"+
			"「流量配额」的含义就没人说得清了", trafficField[1])
	}
}

// ---------------------------------------------------------------------------
// 3. 开户接口的出参：商城读五个键
// ---------------------------------------------------------------------------

func TestCreateUsersResponseKeysMatchShopExpectation(t *testing.T) {
	body := extractFuncBodyFrom(t, readFileForTest(t, "portalusers.go"), "portalCreateUser")
	assertJSONKeys(t, body, "provision_ops() 从开户结果里读",
		"user_id", "password", "uri", "clash", "sing_box")

	// 状态码必须是 201 —— 商城接受 (200, 201)，但改成 202 之类就会走 else 分支报错。
	if !strings.Contains(body, "http.StatusCreated") {
		t.Errorf("开户成功不再返回 201；商城 provision_ops() 只接受 200/201，" +
			"其它状态码会抛 'ops 面板开户失败 HTTP xxx'")
	}
	// 缺口令时必须报错，否则商城会拿到空的 password 静默交付。
	if !strings.Contains(body, "节点未返回新用户的口令") {
		t.Errorf("开户接口少了「节点未返回口令」这一道校验 —— " +
			"它是防止商城把空 password 交付给客户的唯一一道闸")
	}
}

// ---------------------------------------------------------------------------
// 4. 回收：危险操作确认头
// ---------------------------------------------------------------------------

func TestDeleteUserRequiresDangerousConfirmHeader(t *testing.T) {
	// admin.go 里的用户删除处理器（商城 deprovision_ops 走的就是它）。
	src := readFileForTest(t, "admin.go")
	if !strings.Contains(src, `Header.Get("X-Confirm-Dangerous")`) {
		t.Errorf("X-Confirm-Dangerous 这个头不见了。\n" +
			"商城 deprovision_ops() 发的是 headers={'X-Confirm-Dangerous': 'yes'}；\n" +
			"换头名 → 回收全部 412，账号留在节点上仍然能连，而商城以为已经删干净了")
	}
}

// ---------------------------------------------------------------------------
// 5. 商城密钥：头名、缺省状态码
// ---------------------------------------------------------------------------

// TestStoreKeyHeaderAndUnconfiguredStatus 盯住 requireStoreKey。
//
// 两件事都不能动：
//
//	头名 X-Store-Key  —— 商城发死的是它（store_key 存在 token_enc 的加密 JSON 里）
//	未配置时的 503     —— 商城靠「状态码 != 200」判断"没配密钥"并退回单节点直链；
//	                     若改成 200 + 空对象，商城的 res.json() 会当成正常结果，
//	                     随后 merged.get('subscription_url') 取到空串 ——
//	                     表面上没报错，客户却拿到一条空链接。
func TestStoreKeyHeaderAndUnconfiguredStatus(t *testing.T) {
	body := extractFuncBodyFrom(t, readFileForTest(t, "store.go"), "requireStoreKey")

	if !strings.Contains(body, `Header.Get("X-Store-Key")`) {
		t.Errorf("X-Store-Key 头名变了 —— 商城 _ops_merged_subscription() 发死的就是它，" +
			"改名后每一次合并订阅请求都会 401，客户只能拿到单节点直链")
	}
	if !strings.Contains(body, "http.StatusServiceUnavailable") {
		t.Errorf("「商城对接尚未启用」不再返回 503。\n" +
			"商城按 `res.status_code != 200` 判断未启用并退回直链；" +
			"改成 2xx 会让它把错误响应当成正常结果")
	}
	// Authorization: Bearer 的兜底也要留着 —— 两种发法都有人用。
	if !strings.Contains(body, `TrimPrefix(r.Header.Get("Authorization"), "Bearer ")`) {
		t.Errorf("requireStoreKey 不再接受 Authorization: Bearer <key> 的兜底形式")
	}
}

// ---------------------------------------------------------------------------
// 6. /api/store/orders 的入参与出参
// ---------------------------------------------------------------------------

func TestStoreOrderRequestAndResponseContract(t *testing.T) {
	store := readFileForTest(t, "store.go")

	req := extractFuncBodyFrom(t, store, "storeCreateOrder")
	// 商城 _ops_merged_subscription() 发的 payload。
	assertJSONKeys(t, req, "_ops_merged_subscription() 发的",
		"order_id", "label", "expires_at", "ip_limit")

	// order_id 幂等：商城重试支付回调时靠它避免开出第二份订阅。
	if !strings.Contains(req, "loadSubscriptionByOrder") {
		t.Errorf("storeCreateOrder 不再按 order_id 查重 —— " +
			"支付平台重放回调会给同一个客户开出第二份订阅")
	}

	resp := extractFuncBodyFrom(t, store, "writeStoreOrder")
	// 商城交付时读这些键。少了哪个，客户拿到的就是空链接。
	assertJSONKeys(t, resp, "商城交付时读",
		"order_id", "token", "expires_at", "delivery_url",
		"subscription_url", "clash_url", "singbox_url", "uri_url")
}

// ---------------------------------------------------------------------------
// 7. 续费地址：控制台回给商城的收银台地址
// ---------------------------------------------------------------------------

// 续费入口必须通过可配置环境变量 STORE_RENEW_BASE_URL 提供，不得硬编码私有商业域名。
func TestRenewURLPointsAtTheShop(t *testing.T) {
	for _, name := range []string{"writeStoreOrder", "subscriptionView"} {
		body := extractFuncBodyFrom(t, readFileForTest(t, "store.go"), name)
		if !strings.Contains(body, `"renew_url"`) || !strings.Contains(body, "storeRenewBaseURL()") {
			t.Errorf("%s 必须通过 storeRenewBaseURL() 动态构造 renew_url", name)
		}
	}
	t.Setenv("STORE_RENEW_BASE_URL", "")
	if got := subscriptionRenewURL(&subscriptionRecord{OrderID: "OPS-prdord_fixture000001", Token: "tok"}, storeRenewBaseURL()); got != "" {
		t.Errorf("未配置 STORE_RENEW_BASE_URL 时应返回空串，实际得到 %q", got)
	}
	t.Setenv("STORE_RENEW_BASE_URL", "https://shop.example.com")
	want := "https://shop.example.com/?renew=prdord_fixture000001&sub=tok"
	if got := subscriptionRenewURL(&subscriptionRecord{OrderID: "OPS-prdord_fixture000001", Token: "tok"}, storeRenewBaseURL()); got != want {
		t.Errorf("配置 STORE_RENEW_BASE_URL 后期望 %q，实际得到 %q", want, got)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// assertJSONKeys 断言一段源码里出现了这些 JSON 键。
//
// 两种写法都要认，因为同一个响应在源码里可能以两种形态出现：
//
//	"key": value            —— map 字面量（如 writeStoreOrder 的 payload）
//	payload["key"] = value  —— 条件赋值（如那四个订阅链接，只在订阅有效时才给）
//
// 只扫字面量、不解析 AST：这些 key 的产生方式就这两种，正则足够，
// 而且误报比漏报好（漏报才会放过真问题 —— 那条链接客户点开是空的）。
func assertJSONKeys(t *testing.T, src, who string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		ok := strings.Contains(src, `"`+k+`":`) || // map 字面量
			strings.Contains(src, `["`+k+`"]`) || // 下标赋值
			strings.Contains(src, `json:"`+k+`"`) // struct tag
		if !ok {
			t.Errorf("%s 的 JSON 键 %q 不见了。\n"+
				"两侧是两个仓库两种语言，改名不会让任何一边编译失败 —— "+
				"只会让商城拿到一个 undefined，然后静默交付空值", who, k)
		}
	}
}
