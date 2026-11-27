package ippurity

// offline.go — 离线判定库的加载。
//
// 库的形态与来源
// --------------
// 与 geo 包同样的理由，用"自建精简库"而非内置：IP_PURITY_OFFLINE_DB 指向一个
// JSON 文件，结构见下方格式。它由部署脚本生成/更新（例如从公开的 ASN 类型
// 数据、数据中心网段清单转换），**不进 git 仓库** —— 它是数据不是代码。
//
// 为什么不 go:embed：一旦 embed，库更新就必须走"改代码 → 发版 → 重新部署"，
// 而 IP 段归属是**持续变化**的数据。用外部文件则换库＝替换文件＋重启。
//
// 为什么加载失败不能是致命错误：
// 没有离线库只是"判定要走公网 API"或"判定不出来"，节点管理等主功能一个都
// 不受影响。把加载失败做成启动失败，等于让一个锦上添花的功能有权拒绝服务。
// 所以：加载失败只在日志里留一条 WARN，Resolver 照常工作（db 为 nil）。
//
// 离线库格式（JSON）
// -----------------
//
//	{
//	  "_meta": {"source": "自建 ASN 类型库 2026-10-15", "count": 3},
//	  "entries": [
//	    {
//	      "network": "3.0.0.0/8",       // 必填，CIDR
//	      "connection_type": "datacenter", // residential/datacenter/mobile/business
//	      "asn": "AS16509",              // 可选
//	      "asn_org": "Amazon.com, Inc."  // 可选
//	    }
//	  ]
//	}
//
// 查找语义：**最长前缀匹配**。一个 IP 会同时落在 3.0.0.0/8 和 3.5.0.0/16 里，
// 必须选更具体的那条 —— 否则粗粒度条目会盖掉精确的。

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

// offlineEntry 是加载后的一条判定记录。
type offlineEntry struct {
	network *net.IPNet
	verdict Verdict
	// prefixLen 用于最长前缀匹配排序。
	prefixLen int
}

// OfflineFile 是从 JSON 加载的离线库。
type OfflineFile struct {
	entries []offlineEntry
	source  string
}

// LoadOfflineFile 从路径加载离线库。
func LoadOfflineFile(path string) (*OfflineFile, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("未配置离线库路径")
	}
	body, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("读取离线库失败: %w", err)
	}
	var doc struct {
		Meta struct {
			Source string `json:"source"`
		} `json:"_meta"`
		Entries []struct {
			Network        string `json:"network"`
			ConnectionType string `json:"connection_type"`
			ASN            string `json:"asn"`
			ASNOrg         string `json:"asn_org"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("解析离线库失败: %w", err)
	}
	out := &OfflineFile{source: strings.TrimSpace(doc.Meta.Source)}
	for _, raw := range doc.Entries {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw.Network))
		if err != nil {
			// 单条坏记录跳过，不让整库加载失败。
			continue
		}
		ones, _ := network.Mask.Size()
		out.entries = append(out.entries, offlineEntry{
			network:   network,
			prefixLen: ones,
			verdict: Verdict{
				ConnectionType: normalizeConnectionType(raw.ConnectionType),
				ASN:            strings.TrimSpace(raw.ASN),
				ASNOrg:         strings.TrimSpace(raw.ASNOrg),
			},
		})
	}
	// 按前缀长度稳定降序排：查找时第一个命中的就是最具体的，且同前缀长度保持文件内原始顺序。
	sort.SliceStable(out.entries, func(i, j int) bool {
		return out.entries[i].prefixLen > out.entries[j].prefixLen
	})
	return out, nil
}

// Lookup 最长前缀匹配。查不到返回 nil（不是错误）。
func (f *OfflineFile) Lookup(ip string) *Verdict {
	if f == nil {
		return nil
	}
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return nil
	}
	for _, entry := range f.entries {
		if entry.network.Contains(parsed) {
			v := entry.verdict
			v.IP = strings.TrimSpace(ip)
			return &v
		}
	}
	return nil
}

// Source 返回库的自述。
func (f *OfflineFile) Source() string {
	if f == nil {
		return ""
	}
	return f.source
}

// Len 返回条目数（诊断用）。
func (f *OfflineFile) Len() int {
	if f == nil {
		return 0
	}
	return len(f.entries)
}

// normalizeConnectionType 把库里可能的各种写法折成枚举值。
// 认不出的值一律回落 unknown —— 脏值进入判定链会让界面显示乱码。
func normalizeConnectionType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "residential", "home", "isp":
		return "residential"
	case "datacenter", "hosting", "idc":
		return "datacenter"
	case "mobile", "cellular":
		return "mobile"
	case "business", "enterprise":
		return "business"
	default:
		return "unknown"
	}
}

// 编译期断言：OfflineFile 满足 OfflineDB。
var _ OfflineDB = (*OfflineFile)(nil)
