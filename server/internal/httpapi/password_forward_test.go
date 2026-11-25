package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这次修的是一个**极难发现的静默丢弃**（2026-10-03）。
//
// 现象：买家订阅里 4 个节点只有 1 个能用，报
//   authentication error, HTTP status code: 200
//
// 根因链：
//   节点侧 portal.py 的 /users/create：
//       pwd = params.get('password') or secrets.token_hex(16)
//   而面板的 POST /api/nodes/{id}/users handler 的 input 结构体里
//   **根本没有 password 字段** —— 调用方传的 password 被静默丢弃，
//   节点于是每次随机生成一个新密码。
//
// 为什么难查：它**看起来是成功的**（HTTP 200，还返回一个新密码）。
// 仓库里另一个入口（provisionOneNode → portalCall）一直是对的，
// 所以订单流程正常，只有"通过面板重建账号"这条路坏 ——
// 而修复数据时走的正是它，结果把 4 个本来能用的节点全弄坏了。
//
// 用源码断言钉住：**这个字段一旦被删掉，测试立刻红**。

func TestCreateUserHandlerForwardsPassword(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "portalusers.go", "func (s *Server) portalCreateUser(")

	// input 结构体必须有 Password 字段。
	if !strings.Contains(body, `json:"password"`) {
		t.Fatal("portalCreateUser 的 input 缺少 password 字段 —— " +
			"调用方传的密码会被静默丢弃，节点侧每次随机生成新密码，" +
			"导致库里记的密码与节点上的不一致（客户端报 authentication error）")
	}
	// 且必须真的转发进 payload。
	if !strings.Contains(body, `payload["password"] = pwd`) {
		t.Error("input.Password 必须转发到 payload，只声明字段而不转发等于没修")
	}
	// 不能无条件设置（空字符串会覆盖掉节点的默认行为）。
	if strings.Contains(body, `payload["password"] = input.Password`) &&
		!strings.Contains(body, "TrimSpace(input.Password)") {
		t.Error("转发前应判空：空的 password 不该进 payload，" +
			"否则会把空密码写给节点")
	}
}

// 节点侧语义的书面记录：不传 password 就随机生成。
//
// 这条不测实现（实现不在本仓库），而是**把契约写进测试**，
// 免得以后有人"顺手删掉 password 字段，因为看起来没人用"。
func TestNodeCreateContractIsDocumented(t *testing.T) {
	t.Parallel()
	// 开户实际走 syncAccount（subscription_attributes.go）；原来的 provisionOneNode
	// 已无调用方、于 2026-10-17 删除，契约随之挪到真正在用的这条路径上。
	body := funcBlock(t, "subscription_attributes.go", "func syncAccount(")

	if !strings.Contains(body, `"password"`) {
		t.Fatal("syncAccount 开号时必须带 password —— " +
			"节点侧不传就随机生成，重复开户会换密码")
	}
	pw := funcBlock(t, "subscription_attributes.go", "func syncAccountPassword(")
	if !strings.Contains(body, "syncAccountPassword(rec, nodeID)") || !strings.Contains(pw, "provisionAccountPassword(rec, nodeID)") {
		t.Error("密码必须由 provisionAccountPassword 确定性推导，" +
			"否则重复开户换密码、幂等性无从谈起")
	}
}

// 确定性密码必须有测试（纯函数，直接测值）。
func TestProvisionAccountPasswordStable(t *testing.T) {
	t.Parallel()
	rec := &subscriptionRecord{ID: "aaaa-bbbb"}
	a := provisionAccountPassword(rec, "node-x")
	b := provisionAccountPassword(rec, "node-x")
	if a != b || len(a) != 32 {
		t.Fatalf("确定性密码不稳定或长度不对: %q / %q", a, b)
	}
}

// 把"面板 API 与 go 端算法必须一致"写成可核对的注释锚点。
//
// 修复脚本（Python）要靠同样的算法算出同一个密码，
// 两边一旦漂移，重建出来的账号就会与库不一致。
func TestPasswordAlgorithmIsDocumentedForExternalCallers(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(".", "provisioning.go"))
	if err != nil {
		t.Fatalf("读取 provisioning.go 失败：%v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "hy2-account-password") {
		t.Error("确定性密码的域分隔前缀必须在源码里可查 —— " +
			"外部脚本（如账号修复工具）要按同一算法算，前缀漂移会导致库与节点不一致")
	}
}
