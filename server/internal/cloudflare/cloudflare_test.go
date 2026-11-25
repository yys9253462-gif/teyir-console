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
