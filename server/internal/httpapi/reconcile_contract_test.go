package httpapi

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReconcileMustMergeExistingAccounts 钉住「对账不得丢弃已有映射」这一契约。
//
// 背景（2026-10-04，由后端审计发现并经受控实验证实）：
//
//	reconcileSubscriptionAccounts 原本从**空 map** 开始、只装成功的节点，
//	而调用方是 `SET node_accounts=$1`（全量覆盖，非合并）。于是只要对账时
//	有任一节点不可达（超时/抖动/节点正在重启门户——都是常态），那一台的
//	映射就被静默抹掉，后果是：
//	  * subscription.go 让买家订阅里少一个节点，且无任何提示；
//	  * store.go 的 `node_accounts ? nodeID` 查不到该订阅 → 退订回收不掉账号。
//	这正是 AGENTS.md 里「560 孤儿账号」的同一类故障。
//
// 兄弟函数 provisionSubscriptionAccounts 一直是先铺底的，两者行为不一致。
//
// 这个测试的价值在**防止回退**：只要有人把铺底那段删掉，它立刻红。
// 用源码文本断言而不是运行时行为，是因为这条约束的本质是
// 「这两个函数的写法必须一致」—— 它是结构性的，不是某个输入下的输出。
func TestReconcileMustMergeExistingAccounts(t *testing.T) {
	raw, err := os.ReadFile("provisioning.go")
	if err != nil {
		t.Fatalf("读 provisioning.go 失败: %v", err)
	}
	src := string(raw)

	// 定位 reconcileSubscriptionAccounts 的函数体
	reStart := regexp.MustCompile(`func \(s \*Server\) reconcileSubscriptionAccounts\(`)
	loc := reStart.FindStringIndex(src)
	if loc == nil {
		t.Fatal("找不到 reconcileSubscriptionAccounts —— 函数被改名了？本测试需同步更新")
	}

	// 取到下一个顶层 func 之前的片段作为函数体
	rest := src[loc[1]:]
	nextFunc := strings.Index(rest, "\nfunc ")
	body := rest
	if nextFunc >= 0 {
		body = rest[:nextFunc]
	}

	// 必须出现「遍历 rec.nodeAccounts 并写进 accounts」的铺底逻辑。
	// 判据取 `range rec.nodeAccounts` + `accounts[k] = v` 两段同时出现，
	// 避免误判（单独出现其中一句不构成铺底）。
	hasRange := strings.Contains(body, "range rec.nodeAccounts")
	hasAssign := regexp.MustCompile(`accounts\[k\]\s*=\s*v`).MatchString(body)

	if !hasRange || !hasAssign {
		t.Error("reconcileSubscriptionAccounts 必须先把已有映射铺进 accounts，否则：" +
			"调用方的 `SET node_accounts=$1` 全量覆盖会把「本次不可达节点」的映射静默抹掉，\n" +
			"导致买家订阅少节点、且退订时回收不掉该账号。\n" +
			"参考兄弟函数 provisionSubscriptionAccounts 的写法（先 range rec.nodeAccounts）。")
	}
}

// TestReconcileMessageMustNotLie 钉住返回文案与代码行为一致。
//
// 原文案写「失败节点未改动」，而代码恰恰把它们的映射删了 ——
// 消息与行为矛盾会误导运维（项目 AGENTS.md 里刚为同类问题立过规矩）。
func TestReconcileMessageMustNotLie(t *testing.T) {
	raw, err := os.ReadFile("provisioning.go")
	if err != nil {
		t.Fatalf("读 provisioning.go 失败: %v", err)
	}
	src := string(raw)

	if strings.Contains(src, `"已把确定性密码重新下发到各节点；失败节点未改动"`) {
		t.Error("这句文案必须说明「保留原有映射」。裸写「未改动」在铺底修复前是反的 —— " +
			"留着旧措辞会让下一个人以为映射会被自动保住，而保护是靠铺底实现的，不是靠这句话。")
	}
}
