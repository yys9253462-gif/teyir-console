// Package cloudflare 是面板用到的最小 Cloudflare DNS 客户端（2026-10-15）。
//
// 只做一件事：把「某个域名 → 某个 IPv4」写成一条 A 记录（有就改、没有就建），
// 且**强制不走 CDN 代理**（proxied=false）—— Hysteria 是 UDP 协议，
// 经过 Cloudflare 橙云代理后根本连不上，Let's Encrypt 也会签到错误的地址。
package cloudflare

import (
	"bytes"
	"context"
	"crypto/tls"
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

const (
	apiBase = "https://api.cloudflare.com/client/v4"
	// maxDNSRecordPages 给 DNS 记录分页设置硬上限，防止异常响应导致无限分页死循环。
	maxDNSRecordPages = 100
)

// defaultHTTPClient 在未注入自定义客户端时复用安全传输层连接池，禁用重定向并强制 TLS 1.2 以上。
var defaultHTTPClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	},
}

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
	token := strings.TrimSpace(c.Token)
	if token == "" {
		return nil, errors.New("未配置 Cloudflare API Token")
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("编码 Cloudflare 请求体失败：%w", err)
		}
		rd = bytes.NewReader(raw)
	}
	base := strings.TrimRight(strings.TrimSpace(c.Base), "/")
	if base == "" {
		base = apiBase
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTPClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("连接 Cloudflare 失败：%w", ctxErr)
		}
		return nil, errors.New("连接 Cloudflare 失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return nil, fmt.Errorf("Cloudflare 返回重定向（HTTP %d），已拒绝跟随", resp.StatusCode)
	}
	var env envelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return nil, fmt.Errorf("Cloudflare 返回 %d，无法解析", resp.StatusCode)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			if msg := strings.TrimSpace(e.Message); msg != "" {
				msgs = append(msgs, msg)
			}
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Cloudflare 拒绝访问（%d）：Token 无效或缺少 Zone.DNS 编辑权限", resp.StatusCode)
		}
		if len(msgs) == 0 {
			return nil, fmt.Errorf("Cloudflare 报错（HTTP %d）", resp.StatusCode)
		}
		return nil, fmt.Errorf("Cloudflare 报错：%s", strings.Join(msgs, "；"))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("Cloudflare 返回异常状态码（HTTP %d）", resp.StatusCode)
	}
	return env.Result, nil
}

// FindZone 从域名逐级向上找所属的 zone（a.b.example.com → b.example.com → example.com）。
func (c *Client) FindZone(ctx context.Context, domain string) (id, name string, err error) {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	labels := strings.Split(normalized, ".")
	for i := 0; i+1 < len(labels); i++ {
		if labels[i] == "" {
			continue
		}
		candidate := strings.Join(labels[i:], ".")
		res, err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(candidate), nil)
		if err != nil {
			return "", "", err
		}
		var zones []struct{ ID, Name string }
		if err := json.Unmarshal(res, &zones); err != nil {
			return "", "", fmt.Errorf("Cloudflare 域名列表无法解析：%w", err)
		}
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
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" || strings.HasPrefix(domain, ".") || strings.Contains(domain, "..") || strings.ContainsAny(domain, " \t\r\n") {
		return out, errors.New("域名不能为空或包含非法空白/连续点号")
	}
	ip = strings.TrimSpace(ip)
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return out, fmt.Errorf("%s 不是 IPv4 地址", ip)
	}
	// 归一化为标准点分十进制 IPv4，防止 IPv4 映射 IPv6（如 ::ffff:1.2.3.4）被原样发给 A 记录接口。
	ip = parsed.To4().String()
	zoneID, _, err := c.FindZone(ctx, domain)
	if err != nil {
		return out, err
	}
	escapedZone := url.PathEscape(zoneID)
	records, err := c.listDNSRecords(ctx, zoneID, domain, "A")
	if err != nil {
		return out, err
	}
	// 当存在多条同名 A 记录且目标 IP 已位于后续记录时，优先保留匹配目标 IP（且未开代理）的那条作为首条：
	// 既避免先把 records[0] 改写成已存在的目标 IP 触发 Cloudflare 81058 重复记录报错，也能减少不必要的写请求并保留其备注标签。
	if len(records) > 1 {
		best := -1
		for i, r := range records {
			if r.Content != ip {
				continue
			}
			if !r.Proxied {
				best = i
				break
			}
			if best == -1 {
				best = i
			}
		}
		if best > 0 {
			records[0], records[best] = records[best], records[0]
		}
	}
	body := map[string]any{"type": "A", "name": domain, "content": ip, "ttl": 60, "proxied": false}
	switch {
	case len(records) == 0:
		if _, err := c.do(ctx, http.MethodPost, "/zones/"+escapedZone+"/dns_records", body); err != nil {
			return out, err
		}
		out.Action = "created"
	case records[0].Content == ip && !records[0].Proxied:
		out.Action = "unchanged"
	default:
		// 2026-11-04：PATCH 而不是 PUT。PUT 是整条替换：body 里没写的 comment / tags 会被清空，
		// 每换一次 IP、每迁一次节点，运维在 Cloudflare 上给记录写的备注就丢一次。
		patch := map[string]any{"content": ip, "ttl": 60, "proxied": false}
		if _, err := c.do(ctx, http.MethodPatch, "/zones/"+escapedZone+"/dns_records/"+url.PathEscape(records[0].ID), patch); err != nil {
			return out, err
		}
		out.Action = "updated"
	}
	for _, extra := range records[min(1, len(records)):] {
		if _, err := c.do(ctx, http.MethodDelete, "/zones/"+escapedZone+"/dns_records/"+url.PathEscape(extra.ID), nil); err != nil {
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
	escapedZone := url.PathEscape(zoneID)
	for page := 1; page <= maxDNSRecordPages; page++ {
		res, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records?type=%s&per_page=100&page=%d&name=%s", escapedZone, url.QueryEscape(kind), page, url.QueryEscape(domain)), nil)
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
	return nil, fmt.Errorf("Cloudflare %s 记录分页超过上限（%d 页），已中止拉取", kind, maxDNSRecordPages)
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
		if err := json.Unmarshal(res, &zones); err != nil {
			return nil, fmt.Errorf("Cloudflare 域名列表无法解析：%w", err)
		}
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
