package httpapi

// subscription_state.go —— 「停用订阅」与节点账号状态的联动。
//
// ## 为什么需要这个文件（2026-10-03 管理员实测反馈）
//
// 管理员在面板点了「停用」，但**已经拉取到配置的客户端照常能用** ——
// 实测四个节点 4/4 仍然连通。
//
// 原因：客户端**每次连接**都要经过节点的鉴权回调重新认证，
// 而「停用订阅」原来只做了一件事 —— 让 /sub/{token} 返回 403，
// 也就是「不再向新客户端发放配置」。节点上那个账号仍然 active，
// 老客户端手里的配置仍然有效（有效期甚至到 2106 年）。
//
// 所以原来「停用」与「断开」是两件事，而界面上只写了「停用」——
// 操作者几乎必然会理解成「客户断开了」。
//
// ## 现在的语义（管理员确认）
//
//   停用订阅 → 同时把该订阅在**每个节点**上的账号置为 disabled
//              ⇒ 客户端**立刻**连不上（鉴权回调会拒）
//
//   启用订阅 → 把账号恢复为 active
//              ⇒ 原样恢复，买家不用重配（只改了 status，密码没动）
//
// ## 与「删除」的分工
//
//   停用 = 可逆的断开（保留账号、密码、已用流量）
//   删除 = 不可逆的注销（删账号，且回收订阅）
//
// ## 失败必须如实上报
//
// 节点不可达、老节点没有 set_status 端点（404）、限流 —— 都会让某个节点
// 停不掉。这时**必须把失败的节点报给操作者**，不能只说「已停用」：
// 与项目铁律①同源（回收/退款类操作必须验返回值是否诚实）。
// 同时把结果落库（last_disable_error），便于事后排查与重试。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// subscriptionStateOutcome 记录一次「停用/启用」在各节点上的结果。
type subscriptionStateOutcome struct {
	// Applied 是成功切换的节点名。
	Applied []string
	// Failed 是没切成功的节点名 -> 原因。
	Failed map[string]string
	// Gone 是「映射里还挂着、但节点记录已删除」的节点名（2026-11-06）。
	// 它们同样进 Failed（运维要看得见），但**不该**触发后台无限重试：那台机器已经不在了，永远成功不了。
	Gone         []string
	failuresByID map[string]bool
}

// onlyGone 表示失败的全部是已删除的节点（没有一个是「该切没切成」）。
func (o subscriptionStateOutcome) onlyGone() bool {
	if len(o.failuresByID) == 0 {
		return false
	}
	for _, gone := range o.failuresByID {
		if !gone {
			return false
		}
	}
	return true
}

// allOK 表示每个节点都成功切换。
func (o subscriptionStateOutcome) allOK() bool { return len(o.Failed) == 0 }

// applySubscriptionNodeStatus 把该订阅在**所有持有账号的节点**上的状态
// 切换成 active（启用）或 disabled（停用）。
//
// 并发执行（每个节点一次 HTTP），但结果按节点名排序返回，便于界面稳定展示。
func (s *Server) applySubscriptionNodeStatus(ctx context.Context, rec *subscriptionRecord, active bool) subscriptionStateOutcome {
	out := subscriptionStateOutcome{Failed: map[string]string{}}
	if rec == nil || len(rec.nodeAccounts) == 0 {
		// 没有节点账号的订阅（老数据、或还没 JIT 补齐）——
		// 没有可切换的对象，视为"无事可做"而不是失败。
		return out
	}

	type job struct {
		nodeID string
		userID string
	}
	jobs := make([]job, 0, len(rec.nodeAccounts))
	for nodeID, acc := range rec.nodeAccounts {
		if acc.UserID == "" {
			continue
		}
		jobs = append(jobs, job{nodeID: nodeID, userID: acc.UserID})
	}
	if len(jobs) == 0 {
		return out
	}

	// 预取名称，使凭据或节点读取失败时也能定位到具体节点。
	ids := make([]string, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.nodeID)
	}
	names := s.nodeNamesByID(ctx, ids)

	results := make([]subscriptionStatusResult, len(jobs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			r := subscriptionStatusResult{nodeID: j.nodeID, name: names[j.nodeID]}
			if r.name == "" {
				r.name = j.nodeID
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				r.err = "操作已取消或超时"
				results[i] = r
				return
			}

			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			target, err := s.loadPortalTarget(callCtx, j.nodeID)
			if errors.Is(err, errPortalNodeNotFound) {
				// 2026-11-06：映射里残留的已删节点。仍要**如实上报**（否则运维看不到这条残留映射），
				// 但要单独标记：调用方据此不排后台无限重试 —— 那台机器已经不在了，重试永远不会成功。
				r.err = "节点记录已删除（这条映射是残留，可在节点页清理）"
				r.gone = true
				results[i] = r
				return
			}
			if err != nil {
				r.err = "读取节点失败：" + err.Error()
				results[i] = r
				return
			}
			r.name = target.NodeName
			// 老节点没有这个端点会返回 404/400，必须让它成为一条
			// **可读的失败信息**，而不是静默跳过。
			envelope, err := portalCall(callCtx, target.BaseURL, target.APIKey, http.MethodPost,
				"/users/set_status", map[string]any{"user_id": j.userID, "active": active})
			if err != nil {
				// 即使用户不存在也不能确认停用成功：旧 UUID 可能仍在 Xray 入站中。
				r.err = err.Error()
				results[i] = r
				return
			}
			var parsed struct {
				OK     bool   `json:"ok"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
				r.err = "节点返回无法解析"
				results[i] = r
				return
			}
			want := "disabled"
			if active {
				want = "active"
			}
			if !parsed.OK || parsed.Status != want {
				r.err = "节点未确认目标状态：" + want
				results[i] = r
				return
			}
			results[i] = r
		}(i, j)
	}
	wg.Wait()

	out = collectSubscriptionStatusResults(results)
	return out
}

// describeSubscriptionStateOutcome 把结果拼成一句给人看的话。
//
// 关键：**有失败时必须说出来**。只写「已停用」而实际有节点没停掉，
// 正是本项目反复出现的那类谎报（铁律①）。
func describeSubscriptionStateOutcome(o subscriptionStateOutcome, active bool) string {
	verb := "停用"
	if active {
		verb = "启用"
	}
	if o.allOK() {
		if len(o.Applied) == 0 {
			return "没有需要" + verb + "的节点账号"
		}
		return "已在 " + itoa(len(o.Applied)) + " 个节点上" + verb + "：" + strings.Join(o.Applied, "、")
	}
	var parts []string
	names := make([]string, 0, len(o.Failed))
	for n := range o.Failed {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		parts = append(parts, n+"（"+o.Failed[n]+"）")
	}
	msg := "部分节点" + verb + "失败：" + strings.Join(parts, "；")
	if len(o.Applied) > 0 {
		msg += "。已成功的：" + strings.Join(o.Applied, "、")
	}
	if active {
		msg += "。这些节点上的账号未确认恢复，请重试并检查节点返回的原因。"
	} else {
		msg += "。这些节点上的访问权限尚未确认撤销，客户端可能仍可用，请重试或改用删除。"
	}
	return msg
}

// itoa 小工具：避免为一行格式化引入 strconv（本文件是唯一用处）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// nodeNamesByID 批量取节点名（失败行要给操作者看得懂的名字，而不是 UUID）。
// 查询失败时返回空 map —— 调用方用 UUID 兜底，不因为"名字查不到"而让操作失败。
func (s *Server) nodeNamesByID(ctx context.Context, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if uuidPattern.MatchString(id) {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return out
	}
	rows, err := s.db.Query(ctx, `SELECT id::text, name FROM hysteria_nodes WHERE id = ANY($1::uuid[])`, valid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			out[id] = name
		}
	}
	return out
}

// Keep identity independent of display names, including names that already
// resemble a disambiguated label. Reserve all original names before suffixing.
type subscriptionStatusResult struct {
	nodeID string
	name   string
	err    string
	gone   bool
}

func collectSubscriptionStatusResults(results []subscriptionStatusResult) subscriptionStateOutcome {
	out := subscriptionStateOutcome{Failed: map[string]string{}, failuresByID: map[string]bool{}}
	counts := map[string]int{}
	used := map[string]bool{}
	for _, r := range results {
		counts[r.name]++
		used[r.name] = true
	}
	for _, r := range results {
		name := r.name
		if counts[name] > 1 {
			name += " [" + r.nodeID + "]"
			for used[name] {
				name += "-x"
			}
			used[name] = true
		}
		if r.err != "" {
			out.Failed[name] = r.err
			out.failuresByID[r.nodeID] = r.gone
			if r.gone {
				out.Gone = append(out.Gone, name)
			}
		} else {
			out.Applied = append(out.Applied, name)
		}
	}
	sort.Strings(out.Applied)
	sort.Strings(out.Gone)
	return out
}
