package geo

import "strings"

// translate.go — 把英文地区名翻成中文。
//
// 关键设计（2026-10-02 用户决策）：
//
//  1. **在出口处统一翻译**，不在离线库里改源数据。
//     源数据（`"Tokyo"`）保留英文：便于排障时与 ip-api 输出对齐，
//     也便于以后切回英文或做国际化。翻译是「显示层」的事。
//
//  2. **provider 不翻译**（保留 `AWS`、`Alibaba Cloud`）——
//     这些商家名在运维圈已是通用术语，翻译反而是噪音。
//
//  3. **多源出口都过这一道**：cache / 离线库 / 公网 API 的 Info
//     全部在 Lookup 末尾走一次 zh()，不会有"这条是中文那条是英文"的混乱。
//
//  4. **大小写不敏感、容错**：匹配时一律 ToLower，被识别的源如果是
//     "tokyo" / "TOKYO" / "Tokyo" 都能命中。命中不了**就原样返回**，
//     比强行翻译成「未知」好——用户能在面板里直接看到源字符串，
//     知道"这台机器识别器没见过"，比"显示东京但其实是别的地方"诚实。

// regionTranslations 是「英文 → 中文」的直译表。
//
// 来源：
//   - 大陆/港澳台主流译法（参考维基百科中文条目）
//   - 优先选**耳熟能详**的形式（如"东京"而非"東京都"）
//
// 加新城市时务必加单测（见 translate_test.go 的 TestRegionTranslate）。
// 不要写"城市+州"的复合名（如"San Francisco, California"）——
// normalizeIPAPI 已经把 city / country 分开了，这里只翻纯城市。
var regionTranslations = map[string]string{
	// 亚洲
	"tokyo":        "东京",
	"osaka":        "大阪",
	"seoul":        "首尔",
	"hong kong":    "香港",
	"taipei":       "台北",
	"singapore":    "新加坡",
	"manila":       "马尼拉",
	"bangkok":      "曼谷",
	"mumbai":       "孟买",
	"bangalore":    "班加罗尔",
	"jakarta":      "雅加达",
	"kuala lumpur": "吉隆坡",
	"dhaka":        "达卡",
	// 东亚邻近
	"beijing":   "北京",
	"shanghai":  "上海",
	"guangzhou": "广州",
	"shenzhen":  "深圳",
	"hangzhou":  "杭州",
	"chengdu":   "成都",
	"chennai":   "金奈",
	"hyderabad": "海得拉巴",
	// 北美
	"ashburn":        "阿什本",
	"columbus":       "哥伦布",
	"new york":       "纽约",
	"los angeles":    "洛杉矶",
	"seattle":        "西雅图",
	"san jose":       "圣何塞",
	"santa clara":    "圣克拉拉",
	"san francisco":  "旧金山",
	"dallas":         "达拉斯",
	"chicago":        "芝加哥",
	"atlanta":        "亚特兰大",
	"miami":          "迈阿密",
	"denver":         "丹佛",
	"phoenix":        "凤凰城",
	"portland":       "波特兰",
	"virginia":       "弗吉尼亚",
	"north virginia": "北弗吉尼亚",
	"montreal":       "蒙特利尔",
	"toronto":        "多伦多",
	"vancouver":      "温哥华",
	// 欧洲
	"stockholm": "斯德哥尔摩",
	"frankfurt": "法兰克福",
	"london":    "伦敦",
	"paris":     "巴黎",
	"amsterdam": "阿姆斯特丹",
	"berlin":    "柏林",
	"munich":    "慕尼黑",
	"madrid":    "马德里",
	"barcelona": "巴塞罗那",
	"milan":     "米兰",
	"rome":      "罗马",
	"warsaw":    "华沙",
	"vienna":    "维也纳",
	"zurich":    "苏黎世",
	"dublin":    "都柏林",
	"helsinki":  "赫尔辛基",
	// 大洋洲
	"sydney":    "悉尼",
	"melbourne": "墨尔本",
	"auckland":  "奥克兰",
}

// zhRegion 把英文地区名翻成中文，匹配不上原样返回。
//
// 为什么匹配不上原样返回（不写「未知」占位）：
//   - 维护一份覆盖所有城市的翻译表不可能（ip-api 全球 10000+ 城市）
//   - 显示「东京」比显示「未知」诚实——前者用户能猜到意思，后者是死路
//   - 用户看到英文（比如"Mumbai"）反而会去查，那比告诉「孟买」更主动
//
// 但要避免显示纯英文的「美国」，而不是「弗吉尼亚 / 阿什本」之类 ——
// 因为 ip-api 偶尔会**只给州/省，不给城市**。那时它落到 country fallback，
// 这里照样能翻（"United States" 不在表里，原样输出"美国"，够用）。
func zhRegion(english string) string {
	// TrimSpace 后判空，能挡掉 ""、"   "、"\t\n"、"\n\r\n" 这些"看起来像
	// 有内容其实是空白"的输入 —— 它们落到面板上会显示一个孤零零的空块，
	// 那是另一个 bug。统一归零。
	if strings.TrimSpace(english) == "" {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(english))
	if v, ok := regionTranslations[key]; ok {
		return v
	}
	return english
}

// countryTranslations 是「ISO 3166-1 alpha-2 国家码 → 中文国家名」的表。
//
// 为什么按国家码而不是国家英文名查：geo.Info.Country 存的就是大写 ISO 码
// （normalizeIPAPI / 离线库都 strings.ToUpper 过），它比英文名稳定 ——
// ip-api 的 country 全称偶尔是 "United States"、偶尔 "USA"，而码恒为 US。
// 覆盖范围按「会出现在 VPS 机房」的国家来，不必全球齐全；未命中时
// zhCountry 原样返回传入码（与 zhRegion 的「不写未知、原样更诚实」一致），
// 运维看到 "SG-新加坡" 里的 SG 也能自行判断。
//
// 港澳台按中国领土表述，绝不单列成国。
var countryTranslations = map[string]string{
	"JP": "日本",
	"US": "美国",
	"SG": "新加坡",
	"KR": "韩国",
	"DE": "德国",
	"GB": "英国",
	"UK": "英国",
	"FR": "法国",
	"NL": "荷兰",
	"CA": "加拿大",
	"AU": "澳大利亚",
	"PH": "菲律宾",
	"ID": "印度尼西亚",
	"MY": "马来西亚",
	"TH": "泰国",
	"IN": "印度",
	"RU": "俄罗斯",
	"IT": "意大利",
	"ES": "西班牙",
	"PL": "波兰",
	"CH": "瑞士",
	"IE": "爱尔兰",
	"FI": "芬兰",
	"NZ": "新西兰",
	"BR": "巴西",
	"ZA": "南非",
	"AE": "阿联酋",
	"TR": "土耳其",
	"VN": "越南",
	"BD": "孟加拉国",
	"KZ": "哈萨克斯坦",
	"UA": "乌克兰",
	"SE": "瑞典",
	"NO": "挪威",
	"DK": "丹麦",
	"PT": "葡萄牙",
	"AT": "奥地利",
	"CZ": "捷克",
	"HU": "匈牙利",
	"RO": "罗马尼亚",
	"GR": "希腊",
	"MX": "墨西哥",
	"AR": "阿根廷",
	"CL": "智利",
	"SA": "沙特阿拉伯",
	"IL": "以色列",
	"EG": "埃及",
	"NG": "尼日利亚",
	"PK": "巴基斯坦",
	"NP": "尼泊尔",
	"LK": "斯里兰卡",
	"KH": "柬埔寨",
	"MM": "缅甸",
	"LA": "老挝",
	"MN": "蒙古",
	// 中国（含港澳台，一律挂中国）
	"CN": "中国",
	"HK": "中国香港",
	"MO": "中国澳门",
	"TW": "中国台湾",
}

// zhCountry 把 ISO 国家码翻成中文国家名，未命中原样返回传入值。
//
// 入参大小写不敏感（内部 ToUpper），因为调用方可能手填小写码。
func zhCountry(code string) string {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return ""
	}
	if v, ok := countryTranslations[strings.ToUpper(trimmed)]; ok {
		return v
	}
	return trimmed
}
