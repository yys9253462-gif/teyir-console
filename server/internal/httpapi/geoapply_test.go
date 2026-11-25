package httpapi

// geoapply_test.go — 钉死「自动识别怎么落地」这三条业务规则。
//
// 为什么这些测试值得单独存在
// --------------------------
// 这套规则的失败模式**全都是静默的**：识别结果没填进去，用户看到的是
// 「加完节点名字还是 IP，跟上个版本一样」—— 不会报错、不会告警，
// 只会让人以为"这功能没用"。而更糟的一类是**多填了**：把运维者
// 精心起的业务代号（"HK-主力"）冲成 "Hong Kong-Vultr"。
// 后者是数据损坏，且用户未必立刻发现。
//
// 所以这里逐条固定：格式、只填空、manual 永不覆盖、auto 可被纠正、
// 识别失败时一个字段都不许动。

import (
	"testing"

	"nodeops/server/internal/geo"
)

// 规则 1（2026-10-09 变更）：命名格式是 `国家-城市`，商家不再进名字。
func TestApplyGeoNameFormat(t *testing.T) {
	cases := []struct {
		name string
		info geo.Info
		want string
	}{
		{"国家+城市", geo.Info{Country: "JP", Region: "东京"}, "日本-东京"},
		{"只有国家", geo.Info{Country: "US"}, "美国"},
		{"只有城市", geo.Info{Region: "东京"}, "东京"},
		// 商家有值也不进名字。
		{"商家被忽略", geo.Info{Country: "JP", Region: "东京", Provider: "AWS"}, "日本-东京"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyGeo(geoCurrent{NameAuto: true}, tc.info)
			if got.Name != tc.want {
				t.Fatalf("Name = %q，期望 %q", got.Name, tc.want)
			}
			if !got.Applied {
				t.Fatal("应当标记为已应用")
			}
		})
	}
}

// 绝不产出带孤儿连字符的名字。
func TestApplyGeoNeverProducesDanglingHyphen(t *testing.T) {
	got := applyGeo(geoCurrent{NameAuto: true}, geo.Info{Region: "东京"})
	if got.Name == "东京-" || got.Name == "-东京" {
		t.Fatalf("产生了孤儿连字符: %q", got.Name)
	}
}

// 规则 2 的核心：manual 的字段一个字都不动。
//
// 这是整个功能最重要的安全属性 —— 用户手打的业务代号必须赢过 geo 猜测。
func TestApplyGeoNeverOverwritesManualValues(t *testing.T) {
	current := geoCurrent{
		Name:     "HK-主力",
		Region:   "Hong Kong",
		Provider: "自建",
		// 三个字段都是人工填的。
		NameAuto:     false,
		RegionAuto:   false,
		ProviderAuto: false,
	}
	// 识别出来的值与人工值完全不同 —— 全部应当被忽略。
	got := applyGeo(current, geo.Info{Region: "Tokyo", Provider: "AWS", Source: "api"})

	if got.Name != "HK-主力" {
		t.Fatalf("人工命名被覆盖: %q", got.Name)
	}
	if got.Region != "Hong Kong" {
		t.Fatalf("人工地区被覆盖: %q", got.Region)
	}
	if got.Provider != "自建" {
		t.Fatalf("人工商家被覆盖: %q", got.Provider)
	}
	if got.NameAuto || got.RegionAuto || got.ProviderAuto {
		t.Fatal("人工字段的来源标记不该被改成 auto")
	}
	if got.Applied {
		t.Fatal("什么都没改，不该标记为已应用")
	}
}

// 但**空的**字段要被填上 —— 即使来源不是 auto（新建行的来源就是空+false）。
//
// 2026-10-09 命名变更：Region/Provider 照常入库（Provider 只是不进名字），
// 名字 = 国家-城市；本例没有 Country，退化为只用城市。
func TestApplyGeoFillsEmptyFieldsEvenWhenNotAuto(t *testing.T) {
	got := applyGeo(geoCurrent{Name: "", NameAuto: false}, geo.Info{Country: "JP", Region: "Tokyo", Provider: "AWS"})
	if got.Region != "Tokyo" || got.Provider != "AWS" {
		t.Fatalf("空字段应当被填充，得到 %+v", got)
	}
	if got.Name != "日本-Tokyo" {
		t.Fatalf("空名字应当被填充，得到 %q", got.Name)
	}
}

// 只填空的一半：手填了地区、没填商家时，应当只补商家，且名字要用生效的 region。
func TestApplyGeoPartialFill(t *testing.T) {
	got := applyGeo(geoCurrent{Region: "Tokyo", RegionAuto: false}, geo.Info{Region: "Frankfurt", Provider: "Hetzner"})

	if got.Region != "Tokyo" {
		t.Fatalf("人工地区不该被改: %q", got.Region)
	}
	if got.Provider != "Hetzner" {
		t.Fatalf("空商家应当被补: %q", got.Provider)
	}
	// 关键：名字要用**人工的 Tokyo**，而不是识别到的 Frankfurt。
	if got.Name != "Tokyo" {
		t.Fatalf("名字应当用手工地区，得到 %q", got.Name)
	}
}

// auto 的字段可以被后续识别纠正 —— 这是"识别错了还能救"的前提。
func TestApplyGeoAutoFieldsCanBeUpdated(t *testing.T) {
	got := applyGeo(geoCurrent{
		Name: "德国-法兰克福", Region: "Frankfurt", Provider: "Hetzner",
		NameAuto: true, RegionAuto: true, ProviderAuto: true,
	}, geo.Info{Country: "JP", Region: "Tokyo", Provider: "AWS"})

	if got.Region != "Tokyo" || got.Provider != "AWS" || got.Name != "日本-Tokyo" {
		t.Fatalf("auto 字段应当被更新，得到 %+v", got)
	}
	if !got.Applied {
		t.Fatal("有实际改动，应当标记为已应用")
	}
}

// 规则 3：识别不出来时一个字段都不许动。
func TestApplyGeoEmptyInfoChangesNothing(t *testing.T) {
	current := geoCurrent{Name: "myserver", Region: "", Provider: "API 节点"}
	got := applyGeo(current, geo.Info{})

	if got.Name != current.Name || got.Region != current.Region || got.Provider != current.Provider {
		t.Fatalf("空识别结果不该改动任何字段，得到 %+v", got)
	}
	if got.Applied {
		t.Fatal("空识别结果不该标记为已应用")
	}
}

// 值没变化时不该标记 Applied —— 否则每个节点导入都会多一次无意义的 UPDATE。
//
// 名字用新规则（国家-城市）：info 有 JP+东京，算出的名字就是「日本-东京」，
// 与 current.Name 相同 → 无改动。
func TestApplyGeoNoChangeIsNotApplied(t *testing.T) {
	got := applyGeo(geoCurrent{
		Name: "日本-东京", Region: "东京", Provider: "AWS",
		NameAuto: true, RegionAuto: true, ProviderAuto: true,
	}, geo.Info{Country: "JP", Region: "东京", Provider: "AWS"})
	if got.Applied {
		t.Fatal("识别结果与现有值相同时不该标记为已应用")
	}
}

// 来源标记的三个布尔值必须各自独立 —— 混在一起会让"只填空的一半"失效。
func TestApplyGeoSourceFlagsAreIndependent(t *testing.T) {
	got := applyGeo(geoCurrent{
		Region: "Tokyo", RegionAuto: false, // 人工
		Provider: "", ProviderAuto: false, // 空
		Name: "", NameAuto: false, // 空
	}, geo.Info{Region: "Frankfurt", Provider: "Hetzner"})

	if got.RegionAuto {
		t.Fatal("人工 region 不该被标成 auto")
	}
	if !got.ProviderAuto {
		t.Fatal("填充的 provider 应当标成 auto")
	}
	if !got.NameAuto {
		t.Fatal("填充的 name 应当标成 auto")
	}
}

// ---------------------------------------------------------------------------
// hostFromURL：决定 3X-UI 面板能不能被识别
// ---------------------------------------------------------------------------

func TestHostFromURL(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// 裸公网 IP（带端口/路径）→ 取出 IP
		{"https://1.2.3.4:54321/panel", "1.2.3.4"},
		{"http://1.2.3.4:2053", "1.2.3.4"},
		{"1.2.3.4:54321", "1.2.3.4"}, // 没带协议头也要能认
		{"https://1.2.3.4", "1.2.3.4"},
		{"http://[2606:4700:4700::1111]:8080/", "2606:4700:4700::1111"},

		// 域名不查 —— 反解得到的是 CDN 边缘节点，不是机器真实位置
		{"https://panel.example.com", ""},
		{"https://xui.example.com:54321/path", ""},

		// 内网/非法一律不查
		{"https://127.0.0.1:8080", ""},
		{"https://192.168.1.10", ""},
		{"https://10.0.0.5:2053", ""},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := hostFromURL(tc.input); got != tc.want {
			t.Errorf("hostFromURL(%q) = %q，期望 %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// autoSource：来源字符串只有两个合法值（与迁移 015 的 CHECK 约束一致）
// ---------------------------------------------------------------------------

func TestAutoSourceValues(t *testing.T) {
	if got := autoSource(true); got != "auto" {
		t.Fatalf("autoSource(true) = %q，期望 auto", got)
	}
	if got := autoSource(false); got != "manual" {
		t.Fatalf("autoSource(false) = %q，期望 manual", got)
	}
	// 迁移 015 里的 CHECK 只允许这两个值；写错会直接违反约束导致插入失败。
	if sourceAuto != "auto" || sourceManual != "manual" {
		t.Fatal("来源常量必须与迁移 015 的 CHECK 约束一致")
	}
}

func TestNextNumberedNodeName(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		existing map[string]bool
		want     string
	}{
		{"空 base", "", map[string]bool{}, ""},
		{"尚无重名", "日本-东京", map[string]bool{}, "日本-东京"},
		{"有基础名分配圈2", "日本-东京", map[string]bool{"日本-东京": true}, "日本-东京②"},
		{"有圈2分配圈3", "美国-哥伦布", map[string]bool{"美国-哥伦布": true, "美国-哥伦布②": true}, "美国-哥伦布③"},
		{"跳跃空缺补齐", "美国-哥伦布", map[string]bool{"美国-哥伦布": true, "美国-哥伦布③": true}, "美国-哥伦布②"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextNumberedNodeName(tc.base, tc.existing)
			if got != tc.want {
				t.Fatalf("nextNumberedNodeName(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}
