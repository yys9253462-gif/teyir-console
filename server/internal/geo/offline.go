package geo

// offline.go — 离线归属库的加载。
//
// 库的形态与来源
// --------------
// 用 ip-api 官方提供的**免费可商用**离线数据集不现实（要账号/授权），
// 所以这里采用「自建精简库」的路线：
//
//   GEO_OFFLINE_DB 指向一个 JSON 文件，结构见离线库格式。它由部署脚本
//   生成/更新（例如定期从 ip-api 的批量接口灌一遍，或从公开的
//   ASN/国家归属数据转换），**不进 git 仓库**——几十 MB 的二进制/JSON
//   塞进仓库会让每次 clone 都变慢，且它是数据不是代码。
//
// 为什么不直接内置一份库在 Go 源码里（go:embed）：
// 一旦 embed，库的更新就必须走「改代码 → 发版 → 重新部署」，
// 而 IP 段的归属是**持续变化**的数据。embed 会把一个数据运维问题
// 变成代码发布问题。用外部文件则换库＝替换文件＋重启，或直接热加载。
//
// 为什么加载失败不能是致命错误：
// 没有离线库只是「识别要走公网 API」或「识别不出来」，而节点管理、
// 面板对接、开户交付这些主功能一个都不受影响。把加载失败做成启动失败，
// 等于让一个锦上添花的功能有权拒绝服务 —— 那是本末倒置。
// 所以：加载失败只在日志里留一条 WARN，Resolver 照常工作（db 为 nil）。
//
// 离线库格式（JSON）
// -----------------
//
//	{
//	  "_meta": {"source": "ip-api 批量导出 2026-10-01", "count": 2},
//	  "entries": [
//	    {
//	      "network": "3.0.0.0/8",          // 必填，CIDR
//	      "region": "Tokyo",                // 短地区名（城市优先）
//	      "region_long": "Tokyo, Japan",    // 可选
//	      "country": "JP",                  // 可选，ISO 两位
//	      "provider": "AWS",                // 短商家名
//	      "provider_long": "Amazon.com, Inc.", // 可选
//	      "asn": "AS16509"                  // 可选
//	    }
//	  ]
//	}
//
// 查找语义：**最长前缀匹配**。一个 IP 会同时落在 3.0.0.0/8 和
// 3.5.0.0/16 里，必须选更具体的那条 —— 否则粗粒度的条目会盖掉精确的。

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

// offlineEntry 是加载后的一条归属记录。
type offlineEntry struct {
	network *net.IPNet
	info    Info
	// prefixLen 用于最长前缀匹配排序。
	prefixLen int
}

// OfflineFileDB 是基于单个 JSON 文件的离线库。
type OfflineFileDB struct {
	entries []offlineEntry
	source  string
}

// offlineFile 是 JSON 文件的顶层结构。
type offlineFile struct {
	Meta    offlineMeta     `json:"_meta"`
	Entries []offlineRecord `json:"entries"`
}

type offlineMeta struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

type offlineRecord struct {
	Network      string `json:"network"`
	Region       string `json:"region"`
	RegionLong   string `json:"region_long"`
	Country      string `json:"country"`
	Provider     string `json:"provider"`
	ProviderLong string `json:"provider_long"`
	ASN          string `json:"asn"`
}

// LoadOfflineFile 加载离线库文件。
//
// 返回的 error 只用于日志；调用方拿到 (nil, err) 时应当继续运行。
// 文件不存在也返回 error —— 让「没配库」和「配了但坏了」在日志里可区分。
func LoadOfflineFile(path string) (*OfflineFileDB, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("未配置离线库路径")
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("读取离线库 %s 失败: %w", trimmed, err)
	}

	var parsed offlineFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("解析离线库 %s 失败: %w", trimmed, err)
	}

	db := &OfflineFileDB{source: trimmed}
	// 坏条目跳过而不是整库失败：一份手工维护/脚本生成的数据里混进几条
	// 写错的 CIDR 是常态，为它们放弃整个库不合理。
	var skipped int
	for _, record := range parsed.Entries {
		entry, ok := record.toEntry()
		if !ok {
			skipped++
			continue
		}
		db.entries = append(db.entries, entry)
	}

	// 按前缀长度降序：查找时第一条命中的就是最具体的那个网段，
	// 于是 Lookup 可以是「线性扫、命中即返回」，不需要每次都算 max。
	sort.SliceStable(db.entries, func(i, j int) bool {
		return db.entries[i].prefixLen > db.entries[j].prefixLen
	})

	if len(db.entries) == 0 {
		return nil, fmt.Errorf("离线库 %s 里没有可用条目（原始 %d 条，跳过 %d 条）",
			trimmed, len(parsed.Entries), skipped)
	}
	if parsed.Meta.Source != "" {
		db.source = trimmed + " (" + parsed.Meta.Source + ")"
	}
	return db, nil
}

func (r offlineRecord) toEntry() (offlineEntry, bool) {
	_, network, err := net.ParseCIDR(strings.TrimSpace(r.Network))
	if err != nil {
		return offlineEntry{}, false
	}
	info := Info{
		Region:       strings.TrimSpace(r.Region),
		RegionLong:   strings.TrimSpace(r.RegionLong),
		Country:      strings.ToUpper(strings.TrimSpace(r.Country)),
		Provider:     strings.TrimSpace(r.Provider),
		ProviderLong: strings.TrimSpace(r.ProviderLong),
		ASN:          strings.TrimSpace(r.ASN),
	}
	// 判空必须看**全部**归属字段，不能只看短名。
	// 只给了 region_long / provider_long 的记录同样有价值（短名稍后回退成它），
	// 若在这里按短名判空就会被整条丢掉 —— 单测抓到过。
	if info.Region == "" && info.RegionLong == "" && info.Provider == "" &&
		info.ProviderLong == "" && info.Country == "" && info.ASN == "" {
		// 只有网段、没有任何归属信息 —— 这条记录没有价值。
		return offlineEntry{}, false
	}
	// 地区短名为空但有全称时，退回全称，避免「有地区信息却填不进去」。
	if info.Region == "" {
		info.Region = info.RegionLong
	}
	if info.Provider == "" {
		info.Provider = info.ProviderLong
	}
	prefixLen, _ := network.Mask.Size()
	return offlineEntry{network: network, info: info, prefixLen: prefixLen}, true
}

// Lookup 实现 OfflineDB。查不到返回 nil。
func (d *OfflineFileDB) Lookup(ip string) *Info {
	if d == nil {
		return nil
	}
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return nil
	}
	for i := range d.entries {
		if d.entries[i].network.Contains(parsed) {
			// entries 已按前缀降序排好，第一条命中即最长前缀。
			result := d.entries[i].info
			return &result
		}
	}
	return nil
}

// Source 实现 OfflineDB。
func (d *OfflineFileDB) Source() string {
	if d == nil {
		return ""
	}
	return d.source
}

// Len 返回可用条目数（诊断用）。
func (d *OfflineFileDB) Len() int {
	if d == nil {
		return 0
	}
	return len(d.entries)
}
