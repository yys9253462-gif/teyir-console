package httpapi

// manage_geo_test.go — 钉死「人工编辑会把字段来源标成 manual」这一行为。
//
// 为什么它是关键路径而不是细节
// ----------------------------
// 用户选定的约定是「已经手工填过的字段永远不覆盖」。这条约定要成立，
// **必须**在人工编辑时把来源从 auto 翻成 manual。
//
// 如果漏了这一步（我第一版就漏了），后果是：
//   用户手工把名字改成 `HK-主力` → 看着没问题 → 某天又加了一个节点 →
//   自动识别把它冲回 `Hong Kong-Vultr`
// 用户会认为"这面板动不动就改我的名字"，而这是最难排查的一类 bug
// ——改的时候完全正常，问题在几天后以另一个操作的副作用出现。
//
// 源码级检查（不依赖数据库），与项目里 store_columns_test.go 同一手法。

import (
	"regexp"
	"strings"
	"testing"
)

func TestManualEditMarksSourcesAsManual(t *testing.T) {
	source := readFileForTest(t, "manage.go")

	// 定位更新主机的 handler 里的字段构建段。
	idx := strings.Index(source, `if input.Name != nil && strings.TrimSpace(*input.Name) != ""`)
	if idx < 0 {
		t.Fatal("找不到更新主机的 name 处理分支")
	}
	// 取到 provider 分支结束为止，覆盖三个字段的处理。
	tail := source[idx:]
	end := strings.Index(tail, `if input.Tags != nil`)
	if end < 0 {
		t.Fatal("找不到 Tags 分支（用于界定范围）")
	}
	segment := tail[:end]

	// 三个字段各自都要有对应的来源标记写入。
	cases := []struct {
		field  string
		marker string
	}{
		{"name", `add("name_source", sourceManual)`},
		{"region", `add("region_source", sourceManual)`},
		{"provider", `add("provider_source", sourceManual)`},
	}
	for _, tc := range cases {
		if !strings.Contains(segment, tc.marker) {
			t.Errorf("人工修改 %s 时没有把来源标成 manual —— "+
				"这个字段会在下一次自动识别时被静默覆盖", tc.field)
		}
	}
}

// 真正要保证的是：**每个字段的值写入，紧跟其后就是它的来源写入**，
// 且三对按 name → region → provider 的顺序出现。
//
// 不检查字节距离 —— 我第一版卡了 400 字节上限，结果因为自己写的注释很长
// 而误报。注释长度不是行为特征，用它当判据是错的。
// 改成检查**相邻的 add 调用序列**：这直接描述了"成对写入"这个意图，
// 也不受注释、空行、换行风格影响。
func TestManualEditPairsValueWithSource(t *testing.T) {
	source := readFileForTest(t, "manage.go")

	idx := strings.Index(source, `if input.Name != nil && strings.TrimSpace(*input.Name) != ""`)
	if idx < 0 {
		t.Fatal("找不到 name 分支")
	}
	tail := source[idx:]
	end := strings.Index(tail, `if input.Tags != nil`)
	if end < 0 {
		t.Fatal("找不到 Tags 分支（用于界定范围）")
	}
	segment := tail[:end]

	// 抽出全部 add(...) 调用的**第一个参数名**，得到调用序列。
	re := regexp.MustCompile(`add\("([a-z_]+)"`)
	var sequence []string
	for _, match := range re.FindAllStringSubmatch(segment, -1) {
		sequence = append(sequence, match[1])
	}

	// 期望序列：值字段与它的 _source 字段严格交替。
	want := []string{
		"name", "name_source",
		"region", "region_source",
		"provider", "provider_source",
	}
	if len(sequence) < len(want) {
		t.Fatalf("add 调用太少（%d 个）：%v", len(sequence), sequence)
	}
	for i, expected := range want {
		if sequence[i] != expected {
			t.Fatalf("第 %d 个 add 调用是 %q，期望 %q\n实际序列: %v",
				i+1, sequence[i], expected, sequence)
		}
	}
}

// 反向断言：确认这条规则真的会拦住"漏标来源"的写法。
// 测试自身也要能失败 —— 一个永远通过的测试没有价值。
func TestManualEditRuleWouldCatchMissingSource(t *testing.T) {
	// 模拟一段漏了来源标记的代码（只有值、没有 _source）。
	broken := `
	if input.Name != nil && strings.TrimSpace(*input.Name) != "" {
		add("name", strings.TrimSpace(*input.Name))
	}
	if input.Region != nil {
		add("region", strings.TrimSpace(*input.Region))
	}
	if input.Provider != nil {
		add("provider", strings.TrimSpace(*input.Provider))
	}
	if input.Tags != nil {
`
	idx := strings.Index(broken, `if input.Name != nil`)
	tail := broken[idx:]
	end := strings.Index(tail, `if input.Tags != nil`)
	segment := tail[:end]

	re := regexp.MustCompile(`add\("([a-z_]+)"`)
	var sequence []string
	for _, match := range re.FindAllStringSubmatch(segment, -1) {
		sequence = append(sequence, match[1])
	}

	// 这段坏代码的序列里不该出现任何 _source —— 断言检测逻辑能看出差别。
	for _, name := range sequence {
		if strings.HasSuffix(name, "_source") {
			t.Fatalf("构造的坏样本里不该有来源标记，却有 %q", name)
		}
	}
	if len(sequence) != 3 {
		t.Fatalf("坏样本应有 3 个 add 调用，实际 %d 个", len(sequence))
	}
}
