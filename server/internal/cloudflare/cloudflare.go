// Package cloudflare 是面板用到的最小 Cloudflare DNS 客户端（2026-10-15）。
//
// 只做一件事：把「某个域名 → 某个 IPv4」写成一条 A 记录（有就改、没有就建），
// 且**强制不走 CDN 代理**（proxied=false）—— Hysteria 是 UDP 协议，
// 经过 Cloudflare 橙云代理后根本连不上，Let's Encrypt 也会签到错误的地址。
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const apiBase = "https://api.cloudflare.com/client/v4"

type Client struct {
	Token string
	HTTP  *http.Client
	Base  string // 测试用
}

type envelope struct {
	Success bool                       `json:"success"`
	Errors  []struct{ Message string } `json:"errors"`
	Result  json.RawMessage            `json:"result"`
}

func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	if strings.TrimSpace(c.Token) == "" {
		return nil, errors.New("未配置 Cloudflare API Token")
	}
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	base := c.Base
	if base == "" {
		base = apiBase
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("连接 Cloudflare 失败")
	}
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return nil, fmt.Errorf("Cloudflare 返回 %d，无法解析", resp.StatusCode)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Cloudflare 拒绝访问（%d）：Token 无效或缺少 Zone.DNS 编辑权限", resp.StatusCode)
		}
		return nil, fmt.Errorf("Cloudflare 报错：%s", strings.Join(msgs, "；"))
	}
	return env.Result, nil
}

// FindZone 从域名逐级向上找所属的 zone（a.b.example.com → b.example.com → example.com）。
func (c *Client) FindZone(ctx context.Context, domain string) (id, name string, err error) {
	labels := strings.Split(strings.Trim(strings.ToLower(domain), "."), ".")
	for i := 0; i+1 < len(labels); i++ {
		candidate := strings.Join(labels[i:], ".")
		res, err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(candidate), nil)
		if err != nil {
			return "", "", err
		}
		var zones []struct{ ID, Name string }
		_ = json.Unmarshal(res, &zones)
		if len(zones) > 0 {
			return zones[0].ID, zones[0].Name, nil
		}
	}
	return "", "", fmt.Errorf("Cloudflare 账号下找不到 %s 所属的域名（zone）", domain)
}

// UpsertA 把 domain 的 A 记录设为 ip（不走代理）。返回 "created" / "updated" / "unchanged"。
// 需要 AAAA 等附加信息的调用方用 UpsertARecords。
func (c *Client) UpsertA(ctx context.Context, domain, ip string) (string, error) {
	res, err := c.UpsertARecords(ctx, domain, ip)
	return res.Action, err
}

// UpsertResult 是 UpsertARecords 的结果。
type UpsertResult struct {
	Action    string   // created / updated / unchanged
	Deleted   int      // 删掉的多余 A 记录条数
	AAAA      []string // 同名 AAAA 记录的内容（不改动，只提醒）
	AAAAError string   // 无法确认 AAAA 时同样提示，不能谎称没有 IPv6 记录
}

// Warning 返回调用方应展示给操作者的提醒；没有则为空。
func (r UpsertResult) Warning() string {
	if r.AAAAError != "" {
		return "A 记录已同步，但无法核查 AAAA 记录：" + r.AAAAError + "；请手工确认 IPv6 地址"
	}
	if len(r.AAAA) == 0 {
		return ""
	}
	return fmt.Sprintf("该域名还有 AAAA 记录（%s）未改动，IPv6 客户端可能仍连旧地址", strings.Join(r.AAAA, ", "))
}

// UpsertARecords 让 domain 的**全部** A 应答都指向 ip：第一条改写，其余同名 A 记录删除
// （原来只改第一条，剩下的旧 IP 仍会被轮询返回）。AAAA 记录不动，但在结果里报出来。
func (c *Client) UpsertARecords(ctx context.Context, domain, ip string) (UpsertResult, error) {
	var out UpsertResult
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return out, fmt.Errorf("%s 不是 IPv4 地址", ip)
	}
	zoneID, _, err := c.FindZone(ctx, domain)
	if err != nil {
		return out, err
	}
	records, err := c.listDNSRecords(ctx, zoneID, domain, "A")
	if err != nil {
		return out, err
	}
	body := map[string]any{"type": "A", "name": domain, "content": ip, "ttl": 60, "proxied": false}
	switch {
	case len(records) == 0:
		if _, err := c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body); err != nil {
			return out, err
		}
		out.Action = "created"
	case records[0].Content == ip && !records[0].Proxied:
		out.Action = "unchanged"
	default:
		// 2026-11-04：PATCH 而不是 PUT。PUT 是整条替换：body 里没写的 comment / tags 会被清空，
		// 每换一次 IP、每迁一次节点，运维在 Cloudflare 上给记录写的备注就丢一次。
		patch := map[string]any{"content": ip, "ttl": 60, "proxied": false}
		if _, err := c.do(ctx, http.MethodPatch, "/zones/"+zoneID+"/dns_records/"+records[0].ID, patch); err != nil {
			return out, err
		}
		out.Action = "updated"
	}
	for _, extra := range records[min(1, len(records)):] {
		if _, err := c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+extra.ID, nil); err != nil {
			return out, fmt.Errorf("已改写首条 A 记录，但删除多余 A 记录 %s 失败：%w", extra.Content, err)
		}
		out.Deleted++
		out.Action = "updated"
	}
	// AAAA 只读不改：查询失败不撤销已完成的 A 同步，但必须告知操作者。
	if six, err := c.listDNSRecords(ctx, zoneID, domain, "AAAA"); err != nil {
		out.AAAAError = err.Error()
	} else {
		for _, r := range six {
			out.AAAA = append(out.AAAA, r.Content)
		}
	}
	return out, nil
}

type dnsRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

func (c *Client) listDNSRecords(ctx context.Context, zoneID, domain, kind string) ([]dnsRecord, error) {
	var records []dnsRecord
	for page := 1; ; page++ {
		res, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records?type=%s&per_page=100&page=%d&name=%s", zoneID, kind, page, url.QueryEscape(domain)), nil)
		if err != nil {
			return nil, err
		}
		var batch []dnsRecord
		if err := json.Unmarshal(res, &batch); err != nil {
			return nil, fmt.Errorf("Cloudflare %s 记录列表无法解析：%w", kind, err)
		}
		for _, rec := range batch {
			if strings.EqualFold(strings.TrimSuffix(rec.Name, "."), strings.TrimSuffix(domain, ".")) {
				if rec.ID == "" {
					return nil, fmt.Errorf("Cloudflare %s 记录缺少 ID", kind)
				}
				records = append(records, rec)
			}
		}
		if len(batch) < 100 {
			return records, nil
		}
	}
}

// ListZones 列出 Token 能看到的、已激活的域名（zone），按名称排序。
// 用于一键安装时让操作者只填前缀、从下拉里选域名。
func (c *Client) ListZones(ctx context.Context) ([]string, error) {
	var names []string
	for page := 1; page <= 10; page++ {
		res, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones?status=active&per_page=50&page=%d", page), nil)
		if err != nil {
			return nil, err
		}
		var zones []struct{ Name string }
		_ = json.Unmarshal(res, &zones)
		for _, z := range zones {
			names = append(names, strings.ToLower(z.Name))
		}
		if len(zones) < 50 {
			break
		}
	}
	sort.Strings(names)
	return names, nil
}
