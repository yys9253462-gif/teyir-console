package httpapi

// geoapply.go — 把 geo 识别结果落到 servers / xui_panels 两处的**唯一**入口。
//
// 为什么要抽成一个文件而不是在各 handler 里各写一段
// --------------------------------------------------
// 「添加 hy2 节点」与「添加 3X-UI 面板」是两条完全不同的链路（一个走节点
// 门户 API 的自报公网 IP，一个走用户填的面板地址），但**命名的业务规则
// 必须完全一致** —— 否则同一个运维者会发现「加节点自动命名了、加面板没有」，
// 或者两边的名字格式不一样。规则集中在这里，改一处两边同时生效。
//
// 三条业务规则（用户 2026-10-01 明确选定）
// ----------------------------------------
//  1. 命名格式：`地区-商家`（如 `Tokyo-AWS`）。只有一边识别出来时就只用那一边，
//     绝不产出 `Tokyo-` 或 `-AWS` 这种带孤儿连字符的名字。
//  2. **已经手工填过的字段永远不覆盖。** 运维者手打的 `HK-自建` 一定比
//     geo 库猜出来的更准（他很可能是故意改成业务代号的）。
//     所以规则是「只填 auto 的、不碰 manual 的」。
//  3. 识别失败（没库、没网、IP 是内网）时**什么都不改**，让操作者手填。
//     识别是加分项，不是前置条件。
//
// 关于「来源标记」为什么必须显式存库
// ----------------------------------
// 规则 2 看起来用「字段非空就跳过」也能实现，但那样有个改不掉的毛病：
// 自动填完之后字段就非空了，于是**第二次识别永远跳过** —— 万一第一次
// 识别错了（离线库那条记录有误），用户就再也没机会靠自动识别纠正回来。
// 更糟的是节点重复导入到同一台主机时，明明该刷新却因为"字段非空"而不刷新。
//
// 所以库里存 auto/manual 两种来源（迁移 015），判断依据是**来源**而不是"非空"：
//   - 加列时存量行全部标 manual（它们确实都是人手打的，见迁移里的论证）
//   - 自动填入的值标 auto，因此可以被下一次识别更新
//   - 用户在界面上改过就标 manual，从此不再被动
//
// 注意 Source 值与这里的判定**必须只有一处定义**（下面两个常量），
// 否则界面上写 manual、后端比较 "manual " 这种带空格的脏值就会静默失效。

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"nodeops/server/internal/geo"
)

const (
	// sourceAuto：字段由自动识别生成，允许被后续识别更新。
	sourceAuto = "auto"
	// sourceManual：字段由人工填写/修改，自动识别永不覆盖。
	sourceManual = "manual"
)

// geoFields 是识别结果的落地形态：已经按「不覆盖 manual」处理过，
// 调用方直接写库即可。
type geoFields struct {
	// Region / Provider / Name 是**应当写入的最终值**（含保留的既有值）。
	Region   string
	Provider string
	Name     string
	// RegionAuto / ProviderAuto / NameAuto 表示该字段当前是否处于
	// auto（可被自动更新）状态。写库时要一并落盘，作为下次判断的依据。
	RegionAuto   bool
	ProviderAuto bool
	NameAuto     bool
	// Applied 表示这次是否真的识别出了东西且**有字段被改动**。
	// false 时调用方不必发 UPDATE。
	Applied bool
	// Source 是识别来源（offline/api/cache），仅用于日志与审计。
	Source string
}

// geoCurrent 是调用方传入的「当前状态」。
type geoCurrent struct {
	Name         string
	Region       string
	Provider     string
	NameAuto     bool
	RegionAuto   bool
	ProviderAuto bool
}

// applyGeo 是核心规则实现。纯函数：不碰数据库、不出网。
//
// 出网部分由 Resolver 负责，于是这条最需要被测、也最容易写错的规则
// 可以脱离网络与数据库直接单测。
func applyGeo(current geoCurrent, info geo.Info) geoFields {
	fields := geoFields{
		Region:       current.Region,
		Provider:     current.Provider,
		Name:         current.Name,
		RegionAuto:   current.RegionAuto,
		ProviderAuto: current.ProviderAuto,
		NameAuto:     current.NameAuto,
	}
	if info.Empty() {
		return fields
	}
	fields.Source = info.Source

	// 规则 2：只动「auto 或空」的字段。
	//
	// 空也算可填 —— 这是新建场景（current 全是空、auto 为 false），
	// 以及「运维者清空了某字段想让它重新识别」的场景。
	if fillable(current.Region, current.RegionAuto) {
		if value := strings.TrimSpace(info.Region); value != "" && value != strings.TrimSpace(current.Region) {
			fields.Region = value
			fields.RegionAuto = true
			fields.Applied = true
		}
	}
	if fillable(current.Provider, current.ProviderAuto) {
		if value := strings.TrimSpace(info.Provider); value != "" && value != strings.TrimSpace(current.Provider) {
			fields.Provider = value
			fields.ProviderAuto = true
			fields.Applied = true
		}
	}

	// 规则 1：名字用「国家-城市」拼（2026-10-09 规则变更：不再含商家）。
	//
	// 国家取自本次识别结果（servers 表没有 country 列，不单独落库，
	// 只参与命名）；城市用 fields.Region —— 注意是**更新后的值**而不是
	// 只识别到的那一半，否则「手填了 region=东京、识别出 provider=AWS」
	// 会把已有的东京丢了（provider 现在不进名字，但 region 的保留逻辑不变）。
	// 识别不到国家时 joinGeoName 退化为只用城市，绝不产出 "-东京"。
	if fillable(current.Name, current.NameAuto) {
		if candidate := joinGeoName(info.CountryName(), fields.Region); candidate != "" &&
			candidate != strings.TrimSpace(current.Name) {
			fields.Name = candidate
			fields.NameAuto = true
			fields.Applied = true
		}
	}
	return fields
}

// fillable 判断一个字段当前是否允许被自动写入。
//
// 两种情况允许：值是空的（还没填过），或来源是 auto（之前就是自动填的）。
// 来源是 manual 且值非空 —— 一个字都不动。
func fillable(value string, auto bool) bool {
	if strings.TrimSpace(value) == "" {
		return true
	}
	return auto
}

// autoSource 把布尔标记折成入库用的来源字符串。
func autoSource(auto bool) string {
	if auto {
		return sourceAuto
	}
	return sourceManual
}

// joinGeoName 拼 `左-右`（现用于 `国家-城市`），缺一边时只返回那一边。
//
// 参数名保持 region/provider 是历史签名；调用方现在传 (国家, 城市)。
// 语义不变：两段非空才带连字符，绝不产出 "日本-" / "-东京"。
func joinGeoName(region, provider string) string {
	region = strings.TrimSpace(region)
	provider = strings.TrimSpace(provider)
	switch {
	case region != "" && provider != "":
		return region + "-" + provider
	case region != "":
		return region
	default:
		return provider
	}
}

// resolveGeo 查一次识别结果。**任何失败都只记日志、返回空 Info**，
// 永不向上抛错 —— 这是「识别不能挡住主流程」这条规则的执行点。
func (s *Server) resolveGeo(ctx context.Context, ip string) geo.Info {
	if s == nil || s.geo == nil {
		return geo.Info{}
	}
	info, err := s.geo.Lookup(ctx, ip)
	if err != nil {
		// 用 Debug 而不是 Warn：没配离线库、或遇到内网地址时这里必然报错，
		// 而那是完全正常的配置状态，不是需要运维介入的异常。
		// 真出问题时用 /api/geo/status 看。
		s.logger.Debug("geo lookup skipped", "ip", ip, "error", err)
		return geo.Info{}
	}
	return info
}

// hostFromURL 从 URL 里取出主机名（不含端口）。
//
// 3X-UI 面板登记的是 https://1.2.3.4:54321/xxx 这样的地址，要识别的是
// **主机**（1.2.3.4）。域名形态（panel.example.com）识别不了 geo ——
// 本包不解析域名，因为把域名反解成 IP 再查归属，得到的往往是 CDN 边缘节点
// 而不是这台机器真实所在，那比不识别更误导。所以域名一律返回空串，
// 让操作者手填。
func hostFromURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// 容错：用户可能没带协议头。
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	// 只有裸公网 IP 才值得查；域名交给操作者。
	if !geo.IsPublicIP(host) {
		return ""
	}
	return host
}

// suggestedNodeName 给「从 API 导入」对话框用的默认节点名，规则与手动
// 添加完全一致：先走 geo 识别 → 国家-城市；识别失败兜底 server_name 或
// 公网 IP。
//
// 2026-10-09 规则变更：不再带端口后缀、不再含商家。管理员要求名字是
// 「国家-城市」（如「日本-东京」）。代价是同城市多节点会重名 —— 靠商城
// 标签、域名、IP 区分，这是明确接受的权衡。
//
// listenPort 参数保留但不进名字：调用点（nodeapi.go）有生产签名漂移，
// 为不动那个文件而保留形参；Go 允许未使用的函数参数。
//
// 这是 previewNodeAPI 用过的同一段逻辑的纯函数版，便于在不依赖真实
// portal HTTP 的前提下钉死四条行为（识别成功 / 只识别 region / 只识别
// provider / 识别全空）。
//
// 调用方负责让 info 已翻译（即 Region 已是「东京」而非「Tokyo」、
// Country 已是「日本」而非 "JP"）—— 这是 Resolver.Lookup 出口的契约。
func suggestedNodeName(serverName, publicIP string, listenPort int, info geo.Info) string {
	_ = listenPort // 规则变更后端口不再进名字，保留形参见函数注释。
	geoResult := applyGeo(geoCurrent{
		NameAuto:     true,
		RegionAuto:   true,
		ProviderAuto: true,
	}, info)
	base := strings.TrimSpace(geoResult.Name)
	if base == "" {
		// 兜底：geo 未识别（离线库未收录、API 失败、IP 是内网），仍要给操作者
		// 一个可读的默认值。用 server_name，再退到公网 IP。
		base = strings.TrimSpace(serverName)
		if base == "" {
			base = strings.TrimSpace(publicIP)
		}
	}
	return base
}

var circledDigits = [...]string{
	"②", "③", "④", "⑤", "⑥", "⑦", "⑧", "⑨", "⑩",
	"⑪", "⑫", "⑬", "⑭", "⑮", "⑯", "⑰", "⑱", "⑲", "⑳",
}

// nextNumberedNodeName 在已有节点名集合中为 base 分配下一个可用名称。
// 如果 base 尚未使用，直接返回 base；
// 如果已存在，则依次追加 ②、③ ... 直到不重名；超过 ⑳ 则退化追加 -(n)。
func nextNumberedNodeName(base string, existing map[string]bool) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	if !existing[base] {
		return base
	}
	for i, c := range circledDigits {
		cand := base + c
		if !existing[cand] {
			return cand
		}
		_ = i
	}
	for n := 21; ; n++ {
		cand := fmt.Sprintf("%s-(%d)", base, n)
		if !existing[cand] {
			return cand
		}
	}
}

// disambiguateNodeName 查询数据库中未删除的节点名称集合，
// 为 base 自动计算带序号（如 ②、③）的唯一推荐名称。
func (s *Server) disambiguateNodeName(ctx context.Context, base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return base, nil
	}
	if s == nil || s.db == nil {
		return base, errors.New("database unavailable while disambiguating node name")
	}
	rows, err := s.db.Query(ctx, `SELECT name FROM hysteria_nodes WHERE deleted_at IS NULL`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	existing := make(map[string]bool)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "", err
		}
		if n != "" {
			existing[strings.TrimSpace(n)] = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return nextNumberedNodeName(base, existing), nil
}
