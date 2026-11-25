package geo

// translate_test.go — 钉死翻译函数的几件关键事：
//
//   1. **必须有正向用例**：用户决定翻译的那批城市都要测（"东京/马尼拉/阿什本"）
//      这是产品验收的凭证 —— 哪天有人手抖删了一行表项，这条测试会红。
//   2. **必须有负向用例**：未识别的输入**必须原样返回**，而不是给个"未知"占位。
//      这是诚实性的钉子 —— 改返回"未知"看似友好，但会让用户失去信息。
//   3. **大小写与首尾空格**：实测 ip-api 输出有时是"tokyo" / "TOKYO" / " Tokyo "，
//      一律能命中。"San Francisco" 含空格也要能匹配。

import (
	"strings"
	"testing"
)

func TestZhRegion_TranslatedCities(t *testing.T) {
	// 用户决策：地区全翻，provider 保留英文。
	// 这一组是面板上**实际会出现**的城市（基于当前 7 台机器 + 离线库覆盖的商家段）。
	// 加新城市时务必在这里加一行 —— 这是合同。
	cases := []struct {
		en   string
		want string
	}{
		// 你自己的 7 台机器 + 之前识别过的
		{"Tokyo", "东京"},
		{"Ashburn", "阿什本"},
		{"Columbus", "哥伦布"},
		{"Stockholm", "斯德哥尔摩"},
		{"Manila", "马尼拉"},
		{"Sydney", "悉尼"},
		// 其它有代表性的（覆盖其他云厂商覆盖的区域）
		{"Los Angeles", "洛杉矶"},
		{"San Francisco", "旧金山"},
		{"Hong Kong", "香港"},
		{"Singapore", "新加坡"},
		{"Frankfurt", "法兰克福"},
		{"London", "伦敦"},
		{"Seoul", "首尔"},
	}
	for _, tc := range cases {
		t.Run(tc.en, func(t *testing.T) {
			got := zhRegion(tc.en)
			if got != tc.want {
				t.Errorf("zhRegion(%q) = %q，期望 %q", tc.en, got, tc.want)
			}
		})
	}
}

// 没翻译的输入必须**原样返回**（不是"未知"）。
//
// 理由：让用户看到源字符串（英文）比给一个"未知"占位更诚实。
// "未知"意味着面板拒绝承认这里有信息；英文原文意味着面板承认
// "我不知道中文怎么说，但你能查到"。
func TestZhRegion_UnknownPassesThrough(t *testing.T) {
	unknown := []string{
		"Backendistan",  // 虚构
		"Coolsville",    // 虚构
		"Севастополь",   // 实有但我故意没收录
		"United States", // 国家而非城市，按设计不翻
		"日本",            // 中文输入本身不该再翻，原样返回即可
	}
	for _, in := range unknown {
		got := zhRegion(in)
		if got != in {
			t.Errorf("未收录的 %q 应原样返回，实际 %q", in, got)
		}
	}
}

// 大小写、首尾空格要无感。
//
// 实测 ip-api 输出形态：
//   - 正常: "Tokyo"
//   - 偶尔全大写: "TOKYO"
//   - 偶尔带空格: " Tokyo "
//   - 带逗号分隔的: "San Francisco, California"
//
// 第二种与第三种必须能命中；第四种**不在本函数责任范围**，
// （normalizeIPAPI 应该提前把 ", California" 切掉），但万一
// 落到这里，原样返回也是合理 fallback。
func TestZhRegion_CaseAndWhitespace(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"tokyo", "东京"},
		{"TOKYO", "东京"},
		{"Tokyo", "东京"},
		{"  Tokyo  ", "东京"},
		{"san francisco", "旧金山"},
		{"SAN FRANCISCO", "旧金山"},
		{"San Francisco", "旧金山"},
	}
	for _, tc := range cases {
		got := zhRegion(tc.in)
		if got != tc.want {
			t.Errorf("zhRegion(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// 空输入要返回空串，不是任何错误信息。
func TestZhRegion_EmptyReturnsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if got := zhRegion(in); got != "" {
			t.Errorf("空输入应返回空串，实际 %q", got)
		}
	}
}

// ⚠️ 完整性自检：翻译表里所有 key 都必须 lowercase。
// 这是给后续维护者的护栏 —— 哪天有人加了 "Tokyo" 和 "tokyo" 两条，
// 第二次 "tokyo" 会覆盖前一个（map 行为），但 "Tokyo" 会变成死代码。
func TestRegionTranslations_KeysAreNormalized(t *testing.T) {
	for k := range regionTranslations {
		if strings.ToLower(k) != k {
			t.Errorf("翻译表 key %q 必须小写", k)
		}
		if strings.TrimSpace(k) != k {
			t.Errorf("翻译表 key %q 不能有首尾空格", k)
		}
	}
}
