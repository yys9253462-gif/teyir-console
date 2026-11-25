package httpapi

import "testing"

// TestUpgradeActionsPinTarget 钉住「升级动词的 target 由服务端固化」这一契约。
//
// 背景（2026-10-04 实测踩到）：这三个动词最初把 target 放在 Allowed 里、
// 指望调用方传。但前端 NodeControlPanel.run() 只发 {verb}，
// 于是点「升级 portal 自身」必然 400：
//
//	{"error":"升级目标只支持 core / portal / awg"}
//
// 三个升级按钮**从写出来起就没成功过**，而且因为没有单测覆盖，
// 这个缺陷一直没被发现。
//
// 这个测试的价值不在"验证当前实现"，而在**防止回退**：
// 只要有人把 target 挪回 Allowed、或改错取值，它立刻红。
func TestUpgradeActionsPinTarget(t *testing.T) {
	cases := map[string]string{
		"upgrade_core":   "core",
		"upgrade_portal": "portal",
		"upgrade_awg":    "awg",
	}
	for verb, want := range cases {
		spec, ok := portalActions[verb]
		if !ok {
			t.Fatalf("%s 不在 portalActions 里", verb)
		}
		// 1) 必须是服务端固定值
		if spec.Fixed["target"] != want {
			t.Errorf("%s: Fixed[target] = %q, 期望 %q（target 必须服务端固化，"+
				"否则调用方漏传就 400）", verb, spec.Fixed["target"], want)
		}
		// 2) 不该再出现在 Allowed 里 —— 白名单越窄越安全，
		//    留着只会让"前端传了个别的值"成为可能。
		for _, a := range spec.Allowed {
			if a == "target" {
				t.Errorf("%s: target 仍在 Allowed 里，应只保留 Fixed", verb)
			}
		}
		// 3) 端点必须是 do-upgrade
		if spec.Path != "do-upgrade" {
			t.Errorf("%s: Path = %q, 期望 do-upgrade", verb, spec.Path)
		}
		// 4) 升级是危险操作，必须二次确认
		if !spec.Dangerous {
			t.Errorf("%s: 升级操作必须标记 Dangerous（需要 X-Confirm-Dangerous）", verb)
		}
	}
}

// TestBuildFormUpgradeTargetCannotBeOverridden 验证调用方**无法**覆盖
// 服务端固化的 target —— 这是 Fixed 相对 Allowed 的核心价值。
func TestBuildFormUpgradeTargetCannotBeOverridden(t *testing.T) {
	spec := portalActions["upgrade_portal"]

	// 即便调用方恶意传了 target=core，也必须被 Fixed 覆盖回 portal。
	form := buildForm(spec, map[string]string{"target": "core"})
	if form["target"] != "portal" {
		t.Errorf("target 被调用方覆盖了：得到 %q, 期望 portal", form["target"])
	}

	// 调用方什么都不传时，同样要得到正确的 target
	// （这正是原缺陷的场景：前端只发 {verb}）。
	form = buildForm(spec, nil)
	if form["target"] != "portal" {
		t.Errorf("未传 target 时得到 %q, 期望 portal —— "+
			"若为空说明调用方漏传就必然 400", form["target"])
	}
}
