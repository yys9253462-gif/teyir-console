package ippurity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// IPQS 是 IPQualityScore 的适配器（2026-10-15）。
//
// 它补的是 ipapi.is 给不了的东西：**单个 IP 的欺诈分**（0-100）与基于实时
// 行为的代理 / VPN / 近期滥用标记。ipapi.is 的 abuser_score 是整个 ASN 的
// 比例，同一家云厂商的节点分数几乎一样，区分不了好 IP 与坏 IP。
//
// 只用来覆盖评分与风险标记；连接类型、ASN、双 ISP 仍以 ipapi.is 为准 ——
// IPQS 免费档的 connection_type 常返回 "Premium required."。
//
// 接口：GET https://ipqualityscore.com/api/json/ip/{key}/{ip}?strictness=1
// ⚠️ 密钥在 URL 路径里，出错信息里绝不能带 URL。
type IPQS struct {
	Keys    *KeyStore
	BaseURL string
}

// ErrNoKey 表示该 provider 没有配置密钥 —— 调用方应当静默跳过，不算失败。
var ErrNoKey = errors.New("未配置密钥")

const DefaultIPQSBaseURL = "https://ipqualityscore.com/api/json/ip"

func (p IPQS) Name() string { return "ipqualityscore" }

type ipqsResponse struct {
	Success     bool   `json:"success"`
	Message     string `json:"message"`
	FraudScore  *int   `json:"fraud_score"`
	Proxy       *bool  `json:"proxy"`
	VPN         *bool  `json:"vpn"`
	Tor         *bool  `json:"tor"`
	RecentAbuse *bool  `json:"recent_abuse"`
	BotStatus   *bool  `json:"bot_status"`
	Mobile      *bool  `json:"mobile"`
	CountryCode string `json:"country_code"`
	ISP         string `json:"ISP"`
	ASN         int    `json:"ASN"`
}

func (p IPQS) Lookup(ctx context.Context, client *http.Client, ip string) (*Verdict, error) {
	key := strings.TrimSpace(p.Keys.Get(KeyIPQS))
	if key == "" {
		return nil, ErrNoKey
	}
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		base = DefaultIPQSBaseURL
	}
	endpoint := base + "/" + url.PathEscape(key) + "/" + url.PathEscape(ip) + "?strictness=1&allow_public_access_points=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("构造 IPQS 请求失败")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// 不带 err 原文：*url.Error 会把含密钥的完整 URL 打出来。
		return nil, errors.New("IPQS 请求失败（网络错误或超时）")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("IPQS 返回 %d", resp.StatusCode)
	}
	var r ipqsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, errors.New("解析 IPQS 响应失败")
	}
	if !r.Success {
		msg := strings.TrimSpace(r.Message)
		if len([]rune(msg)) > 120 {
			msg = string([]rune(msg)[:120])
		}
		return nil, fmt.Errorf("IPQS 拒绝请求：%s", msg)
	}
	return normalizeIPQS(ip, r), nil
}

func normalizeIPQS(ip string, r ipqsResponse) *Verdict {
	v := &Verdict{IP: ip, ScoreSource: "ipqs"}
	if r.FraudScore != nil {
		score := *r.FraudScore
		if score < 0 {
			score = 0
		}
		if score > 100 {
			score = 100
		}
		v.FraudScore = IntPtr(score)
	}
	if r.Proxy != nil || r.Tor != nil {
		v.Proxy = BoolPtr((r.Proxy != nil && *r.Proxy) || (r.Tor != nil && *r.Tor))
	}
	v.VPN = r.VPN
	v.Abuser = r.RecentAbuse
	v.Raw = map[string]any{
		"fraud_score": r.FraudScore, "proxy": r.Proxy, "vpn": r.VPN, "tor": r.Tor,
		"recent_abuse": r.RecentAbuse, "bot_status": r.BotStatus, "mobile": r.Mobile,
		"country_code": r.CountryCode, "isp": r.ISP, "asn": r.ASN,
	}
	return v
}
