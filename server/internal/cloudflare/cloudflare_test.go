package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeCF 模拟 Cloudflare：只有 example.com 这个 zone，记录存在内存里。
func fakeCF(t *testing.T, existing *map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := func(result any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result}) }
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				ok([]map[string]string{{"id": "z1", "name": "example.com"}})
			} else {
				ok([]any{})
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			if r.URL.Query().Get("type") == "AAAA" {
				ok([]any{})
			} else if *existing == nil {
				ok([]any{})
			} else {
				ok([]any{*existing})
			}
		case r.Method == http.MethodPost || r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			var rec map[string]any
			_ = json.Unmarshal(body, &rec)
			if rec["proxied"] != false {
				t.Errorf("records must never be proxied: %s", body)
			}
			// PATCH 与 Cloudflare 一样只改传入的字段，其余（comment / tags）保留。
			if r.Method == http.MethodPatch && *existing != nil {
				merged := map[string]any{}
				for k, v := range *existing {
					merged[k] = v
				}
				for k, v := range rec {
					merged[k] = v
				}
				rec = merged
			}
			rec["id"] = "r1"
			*existing = rec
			ok(rec)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
}

func TestUpsertA(t *testing.T) {
	var rec map[string]any
	srv := fakeCF(t, &rec)
	defer srv.Close()
	c := &Client{Token: "tok", Base: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()

	if got, err := c.UpsertA(ctx, "hy2.node.example.com", "1.2.3.4"); err != nil || got != "created" {
		t.Fatalf("first upsert: %v %v", got, err)
	}
	if got, err := c.UpsertA(ctx, "hy2.node.example.com", "1.2.3.4"); err != nil || got != "unchanged" {
		t.Fatalf("same ip: %v %v", got, err)
	}
	if got, err := c.UpsertA(ctx, "hy2.node.example.com", "5.6.7.8"); err != nil || got != "updated" {
		t.Fatalf("new ip: %v %v", got, err)
	}
	if _, err := c.UpsertA(ctx, "a.other.org", "1.2.3.4"); err == nil {
		t.Fatal("unknown zone must fail")
	}
	if _, err := c.UpsertA(ctx, "hy2.example.com", "::1"); err == nil {
		t.Fatal("ipv6 must be rejected for an A record")
	}
}

func TestUpsertAPaginationAndPartialFailure(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		t.Run(fmt.Sprint(failDelete), func(t *testing.T) {
			deleted := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ok := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
				switch {
				case r.URL.Path == "/zones":
					ok([]map[string]string{{"id": "z1", "name": "example.com"}})
				case r.Method == http.MethodGet && r.URL.Query().Get("type") == "AAAA":
					_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "IPv6 lookup unavailable"}}})
				case r.Method == http.MethodGet:
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					batch := []map[string]any{}
					for i := (page - 1) * 100; i < min(page*100, 101); i++ {
						batch = append(batch, map[string]any{"id": fmt.Sprint(i), "name": "hy2.example.com", "content": "5.6.7.8", "proxied": false})
					}
					ok(batch)
				case r.Method == http.MethodDelete:
					if failDelete && deleted == 1 {
						_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "delete denied"}}})
						return
					}
					deleted++
					ok(map[string]any{})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
			}))
			defer srv.Close()
			c := &Client{Token: "tok", Base: srv.URL, HTTP: srv.Client()}
			res, err := c.UpsertARecords(context.Background(), "hy2.example.com", "5.6.7.8")
			if failDelete {
				if err == nil || res.Deleted != 1 || !strings.Contains(err.Error(), "删除多余 A") {
					t.Fatalf("partial deletion must fail honestly: %+v %v", res, err)
				}
			} else if err != nil || deleted != 100 || res.Deleted != 100 || res.Warning() == "" {
				t.Fatalf("must visit all pages and warn about unknown AAAA: deleted=%d %+v %v", deleted, res, err)
			}
		})
	}
}

// TestUpsertAReplacesAllARecords：同名多条 A 记录时，首条改写、其余删除；AAAA 不动但报出来。
func TestUpsertAReplacesAllARecords(t *testing.T) {
	type rec struct{ ID, Type, Name, Content string }
	store := []rec{
		{"a1", "A", "hy2.example.com", "1.1.1.1"},
		{"a2", "A", "hy2.example.com", "2.2.2.2"},
		{"a3", "A", "hy2.example.com", "3.3.3.3"},
		{"o1", "A", "other.example.com", "9.9.9.9"},
		{"s1", "AAAA", "hy2.example.com", "2001:db8::1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := func(result any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result}) }
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			ok([]map[string]string{{"id": "z1", "name": "example.com"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			out := []map[string]any{}
			for _, x := range store {
				if x.Type == r.URL.Query().Get("type") && x.Name == r.URL.Query().Get("name") {
					out = append(out, map[string]any{"id": x.ID, "name": x.Name, "content": x.Content, "proxied": false})
				}
			}
			ok(out)
		case r.Method == http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			for i := range store {
				if store[i].ID == id {
					store[i].Content = body["content"].(string)
				}
			}
			ok(map[string]any{"id": id})
		case r.Method == http.MethodDelete:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			for i := range store {
				if store[i].ID == id {
					if store[i].Type != "A" || store[i].Name != "hy2.example.com" {
						t.Errorf("deleted unrelated record %+v", store[i])
					}
					store = append(store[:i], store[i+1:]...)
					break
				}
			}
			ok(map[string]any{"id": id})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	c := &Client{Token: "tok", Base: srv.URL, HTTP: srv.Client()}
	res, err := c.UpsertARecords(context.Background(), "hy2.example.com", "5.6.7.8")
	if err != nil || res.Action != "updated" || res.Deleted != 2 {
		t.Fatalf("got %+v %v", res, err)
	}
	var aHy2 []string
	for _, x := range store {
		if x.Type == "A" && x.Name == "hy2.example.com" {
			aHy2 = append(aHy2, x.Content)
		}
	}
	if len(aHy2) != 1 || aHy2[0] != "5.6.7.8" {
		t.Fatalf("all A answers must point to new ip, got %v", aHy2)
	}
	if len(store) != 3 {
		t.Fatalf("other.example.com A and AAAA must remain, store=%+v", store)
	}
	if len(res.AAAA) != 1 || res.Warning() == "" {
		t.Fatalf("AAAA must be surfaced as a warning: %+v", res)
	}
	// 再跑一次：只剩一条且已正确 → unchanged。
	if got, err := c.UpsertA(context.Background(), "hy2.example.com", "5.6.7.8"); err != nil || got != "unchanged" {
		t.Fatalf("second run: %v %v", got, err)
	}
}

// 2026-11-04：改 IP 只能改 content/ttl/proxied，记录上的 comment / tags 要保留（原来 PUT 整条替换）。
func TestUpsertAKeepsRecordComment(t *testing.T) {
	rec := map[string]any{"id": "r1", "type": "A", "name": "hy2.node.example.com", "content": "1.2.3.4", "proxied": false, "comment": "东京主力，勿删"}
	srv := fakeCF(t, &rec)
	defer srv.Close()
	c := &Client{Token: "tok", Base: srv.URL, HTTP: srv.Client()}
	if got, err := c.UpsertA(context.Background(), "hy2.node.example.com", "5.6.7.8"); err != nil || got != "updated" {
		t.Fatalf("update: %v %v", got, err)
	}
	if rec["content"] != "5.6.7.8" || rec["comment"] != "东京主力，勿删" {
		t.Fatalf("update must change the IP and keep the comment: %+v", rec)
	}
}

// 验证域名空白/尾点与 IPv4 映射地址归一化，以及异常满页响应触发分页硬上限。
func TestUpsertANormalizesAndCapsPagination(t *testing.T) {
	var rec map[string]any
	srv := fakeCF(t, &rec)
	defer srv.Close()
	c := &Client{Token: " tok ", Base: srv.URL + "/", HTTP: srv.Client()}

	if got, err := c.UpsertA(context.Background(), " hy2.node.example.com. ", " ::ffff:1.2.3.4 "); err != nil || got != "created" {
		t.Fatalf("归一化后的域名与 IPv4 映射地址应成功创建: got=%q err=%v", got, err)
	}
	if rec["name"] != "hy2.node.example.com" || rec["content"] != "1.2.3.4" {
		t.Fatalf("写入的域名或 IPv4 未归一化: %+v", rec)
	}
	if _, err := c.UpsertA(context.Background(), "   ", "1.2.3.4"); err == nil {
		t.Fatal("空域名必须报错")
	}

	// 模拟上游永远返回 100 条满页记录，验证达到分页上限后中止，不陷入死循环。
	pages := 0
	loopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zones" {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]string{{"id": "z1", "name": "example.com"}}})
			return
		}
		pages++
		batch := make([]map[string]any, 100)
		for i := range batch {
			batch[i] = map[string]any{"id": fmt.Sprintf("r-%d-%d", pages, i), "name": "hy2.example.com", "content": "1.2.3.4", "proxied": false}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": batch})
	}))
	defer loopSrv.Close()
	loopClient := &Client{Token: "tok", Base: loopSrv.URL, HTTP: loopSrv.Client()}
	if _, err := loopClient.UpsertARecords(context.Background(), "hy2.example.com", "1.2.3.4"); err == nil || !strings.Contains(err.Error(), "分页超过上限") {
		t.Fatalf("超限分页必须报错中止，实际: err=%v pages=%d", err, pages)
	}
	if pages != maxDNSRecordPages {
		t.Fatalf("分页请求次数应恰好等于上限 %d，实际 %d", maxDNSRecordPages, pages)
	}

	// 上游返回结构异常时不得静默吞掉 JSON 解析错误。
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"result":"not-an-array"}`))
	}))
	defer badSrv.Close()
	badClient := &Client{Token: "tok", Base: badSrv.URL, HTTP: badSrv.Client()}
	if _, _, err := badClient.FindZone(context.Background(), "a.example.com"); err == nil || !strings.Contains(err.Error(), "无法解析") {
		t.Fatalf("FindZone 不应静默吞掉解析错误: %v", err)
	}
	if _, err := badClient.ListZones(context.Background()); err == nil || !strings.Contains(err.Error(), "无法解析") {
		t.Fatalf("ListZones 不应静默吞掉解析错误: %v", err)
	}
}

// 验证多条同名 A 记录回退到后续已有目标 IP 时，直接保留并复用匹配记录、零 PATCH，
// 彻底避免先把首条记录改写成重复 IP 触发 Cloudflare 81058 冲突报错。
func TestUpsertARecordsReusesExistingMatchingRecordAvoiding81058(t *testing.T) {
	type rec struct {
		ID, Name, Content string
		Proxied           bool
	}
	store := []rec{
		{"a1", "hy2.example.com", "1.1.1.1", false},
		{"a2", "hy2.example.com", "2.2.2.2", false},
	}
	patchCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			ok([]map[string]string{{"id": "z1", "name": "example.com"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			if r.URL.Query().Get("type") == "AAAA" {
				ok([]any{})
				return
			}
			out := make([]map[string]any, 0, len(store))
			for _, x := range store {
				out = append(out, map[string]any{"id": x.ID, "name": x.Name, "content": x.Content, "proxied": x.Proxied})
			}
			ok(out)
		case r.Method == http.MethodPatch:
			patchCount++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			newIP, _ := body["content"].(string)
			for _, x := range store {
				if x.ID != id && x.Content == newIP {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"success": false,
						"errors":  []map[string]any{{"code": 81058, "message": "An identical record already exists."}},
					})
					return
				}
			}
			for i := range store {
				if store[i].ID == id {
					store[i].Content = newIP
					store[i].Proxied = false
				}
			}
			ok(map[string]any{"id": id})
		case r.Method == http.MethodDelete:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			for i := range store {
				if store[i].ID == id {
					store = append(store[:i], store[i+1:]...)
					break
				}
			}
			ok(map[string]any{"id": id})
		}
	}))
	defer srv.Close()

	c := &Client{Token: "tok", Base: srv.URL, HTTP: srv.Client()}
	res, err := c.UpsertARecords(context.Background(), "hy2.example.com", "2.2.2.2")
	if err != nil {
		t.Fatalf("切回已有 A 记录 IP 不应触发 81058 冲突: %v", err)
	}
	if patchCount != 0 || res.Deleted != 1 || res.Action != "updated" {
		t.Fatalf("应直接保留已有未代理记录并删除多余记录: patch=%d res=%+v", patchCount, res)
	}
	if len(store) != 1 || store[0].ID != "a2" || store[0].Content != "2.2.2.2" {
		t.Fatalf("最终应只保留原 a2 记录，实际: %+v", store)
	}
}

// 验证拒绝 3xx 重定向、拒绝非 2xx 伪成功响应，并拦截含连续点或空白的非法域名。
func TestClientRejectsRedirectBadHTTPStatusAndMalformedDomain(t *testing.T) {
	if defaultHTTPClient.CheckRedirect == nil || defaultHTTPClient.CheckRedirect(nil, nil) == nil {
		t.Fatal("defaultHTTPClient 必须禁用重定向")
	}
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1/evil")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
	}))
	defer redirSrv.Close()
	c := &Client{Token: "tok", Base: redirSrv.URL, HTTP: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if _, err := c.ListZones(context.Background()); err == nil || !strings.Contains(err.Error(), "重定向") {
		t.Fatalf("3xx 重定向必须被拒绝，实际: %v", err)
	}

	fakeOKSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
	}))
	defer fakeOKSrv.Close()
	c2 := &Client{Token: "tok", Base: fakeOKSrv.URL, HTTP: fakeOKSrv.Client()}
	if _, err := c2.ListZones(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("非 2xx 伪成功响应必须报错，实际: %v", err)
	}

	for _, badDomain := range []string{"a..example.com", ".example.com", "a b.example.com"} {
		if _, err := c2.UpsertARecords(context.Background(), badDomain, "1.2.3.4"); err == nil {
			t.Errorf("非法域名 %q 必须被拒绝", badDomain)
		}
	}
}
