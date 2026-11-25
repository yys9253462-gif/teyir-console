package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// node_account_reconcile.go — 每天一次的节点账号对账（2026-10-28）。
//
// 到期回收靠订阅行里的 node_accounts 映射：映射里有的才会去节点上删。
// 映射之外的账号（行被删掉时没回收干净、测试残留、半下线遗留……）没有任何路径会再碰到，
// 只能靠节点自己的 expires_at 兜底 —— 账号还躺在节点上，Reality 也可能还在下发。
// 示例场景：节点上可能残留类似 sub_00000000 的账号：数据库、审计里都查不到，节点上仍是 active。
//
// 这里反过来从节点出发：列出每台节点上的 sub_<uuid> 账号，对照数据库，
// 「订阅行已不存在」或「行已软删且映射里没有这台节点」的就是残留，直接在节点上删除。
//
// 保守规则（宁可漏删一个也不能误删买家正在用的账号）：
//   - 只认 sub_<uuid> 形式的账号；其他账号（hy2_*、手工建的）一概不碰。
//   - 订阅行还在（未软删）的一律不删：映射缺口由 JIT 开户 / 账号对账处理，不是残留。
//   - 节点侧创建不到 2 小时的不删：开户和落库之间有时间差。
//   - 单台节点残留数异常多（>20 且超过该节点 sub_ 账号一半）时整台跳过、只报告：
//     更像是数据库读错了，而不是真有这么多残留。

const (
	nodeReconcileSettingKey = "node_account_reconcile_last_run"
	nodeReconcileInterval   = 23 * time.Hour
	nodeReconcileMinAge     = 2 * time.Hour
	nodeReconcileBulkGuard  = 20
)

var subAccountIDPattern = regexp.MustCompile(`^sub_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

type nodeReconcileAccount struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	UserID   string `json:"user_id"`
	Reason   string `json:"reason"`
	Error    string `json:"error,omitempty"`
}

type nodeReconcileReport struct {
	DryRun       bool                   `json:"dry_run"`
	NodesChecked int                    `json:"nodes_checked"`
	SubAccounts  int                    `json:"sub_accounts"`
	Deleted      []nodeReconcileAccount `json:"deleted"`
	Failed       []nodeReconcileAccount `json:"failed"`
	Skipped      []nodeReconcileAccount `json:"skipped"`
	NodeErrors   map[string]string      `json:"node_errors"`
}

type reconcileSubState struct {
	softDeleted bool
	mapped      map[string]bool
}

// residualReason 判断一个节点账号是否为残留；返回空串表示保留。
func residualReason(state *reconcileSubState, nodeID string) string {
	if state == nil {
		return "订阅记录已不存在"
	}
	if state.softDeleted && !state.mapped[nodeID] {
		return "订阅已删除，且映射里没有这台节点"
	}
	return ""
}

// bulkGuardTripped 单台节点的残留数是否多到可疑。
func bulkGuardTripped(residual, total int) bool {
	return residual > nodeReconcileBulkGuard && residual*2 > total
}

// ReconcileNodeAccounts 跑一轮对账；dryRun 时只报告不删除。
func (s *Server) ReconcileNodeAccounts(ctx context.Context, dryRun bool) nodeReconcileReport {
	report := nodeReconcileReport{DryRun: dryRun, NodeErrors: map[string]string{}}
	rows, err := s.db.Query(ctx, `SELECT id::text FROM hysteria_nodes
		WHERE deleted_at IS NULL AND COALESCE(api_url,'') <> '' AND api_key IS NOT NULL ORDER BY name`)
	if err != nil {
		report.NodeErrors["*"] = "节点列表读取失败：" + err.Error()
		return report
	}
	var nodeIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			report.NodeErrors["*"] = "节点列表解析失败，本轮不删除：" + err.Error()
			return report
		}
		nodeIDs = append(nodeIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		report.NodeErrors["*"] = "节点列表读取中断，本轮不删除：" + err.Error()
		return report
	}

	type nodeUsers struct {
		target *portalTarget
		users  []portalUserEntry
	}
	listed := make([]nodeUsers, len(nodeIDs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, provisioningConcurrency)
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			callCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
			defer cancel()
			target, err := s.loadPortalTarget(callCtx, id)
			if err != nil {
				mu.Lock()
				report.NodeErrors[shortID(id)] = err.Error()
				mu.Unlock()
				return
			}
			envelope, err := portalCall(callCtx, target.BaseURL, target.APIKey, http.MethodGet, "/users/list", nil)
			var parsed struct {
				Users []portalUserEntry `json:"users"`
			}
			if err == nil {
				err = json.Unmarshal(envelope.Data, &parsed)
			}
			if err != nil {
				mu.Lock()
				report.NodeErrors[target.NodeName] = err.Error()
				mu.Unlock()
				return
			}
			listed[i] = nodeUsers{target: target, users: parsed.Users}
		}(i, id)
	}
	wg.Wait()

	// 一次查出所有涉及的订阅行（含软删的）。
	idSet := map[string]bool{}
	for _, n := range listed {
		for _, u := range n.users {
			if m := subAccountIDPattern.FindStringSubmatch(u.UserID); m != nil {
				idSet[m[1]] = true
			}
		}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	states := map[string]*reconcileSubState{}
	if len(ids) > 0 {
		subRows, err := s.db.Query(ctx, `SELECT id::text, deleted_at IS NOT NULL, COALESCE(node_accounts,'{}'::jsonb)
			FROM subscription_tokens WHERE id = ANY($1::uuid[])`, ids)
		if err != nil {
			// 读不到数据库就不能判断任何账号是不是残留：整轮放弃，绝不按「查不到」去删。
			report.NodeErrors["*"] = "订阅记录读取失败，本轮不删除：" + err.Error()
			return report
		}
		for subRows.Next() {
			var id string
			var softDeleted bool
			var raw []byte
			if err := subRows.Scan(&id, &softDeleted, &raw); err != nil {
				subRows.Close()
				report.NodeErrors["*"] = "订阅记录解析失败，本轮不删除：" + err.Error()
				return report
			}
			var accounts map[string]json.RawMessage
			if err := json.Unmarshal(raw, &accounts); err != nil {
				subRows.Close()
				report.NodeErrors["*"] = "订阅账号映射解析失败，本轮不删除：" + err.Error()
				return report
			}
			mapped := make(map[string]bool, len(accounts))
			for nodeID := range accounts {
				mapped[nodeID] = true
			}
			states[id] = &reconcileSubState{softDeleted: softDeleted, mapped: mapped}
		}
		subRows.Close()
		if err := subRows.Err(); err != nil {
			report.NodeErrors["*"] = "订阅记录读取中断，本轮不删除：" + err.Error()
			return report
		}
	}

	now := time.Now()
	for _, n := range listed {
		if n.target == nil {
			continue
		}
		report.NodesChecked++
		var residual []nodeReconcileAccount
		total := 0
		for _, u := range n.users {
			m := subAccountIDPattern.FindStringSubmatch(u.UserID)
			if m == nil {
				continue
			}
			total++
			reason := residualReason(states[m[1]], n.target.NodeID)
			if reason == "" {
				continue
			}
			acc := nodeReconcileAccount{NodeID: n.target.NodeID, NodeName: n.target.NodeName, UserID: u.UserID, Reason: reason}
			if u.CreatedAt > 0 && now.Sub(time.Unix(u.CreatedAt, 0)) < nodeReconcileMinAge {
				acc.Reason += "（创建不到 2 小时，暂不处理）"
				report.Skipped = append(report.Skipped, acc)
				continue
			}
			residual = append(residual, acc)
		}
		report.SubAccounts += total
		if bulkGuardTripped(len(residual), total) {
			for _, acc := range residual {
				acc.Reason += fmt.Sprintf("（本节点残留 %d/%d，数量异常，整台跳过）", len(residual), total)
				report.Skipped = append(report.Skipped, acc)
			}
			continue
		}
		for _, acc := range residual {
			if dryRun {
				report.Deleted = append(report.Deleted, acc)
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
			_, err := portalCall(callCtx, n.target.BaseURL, n.target.APIKey, http.MethodPost, "/users/delete", map[string]any{"user_id": acc.UserID})
			cancel()
			if err != nil && !isUserNotFound(err) {
				acc.Error = err.Error()
				report.Failed = append(report.Failed, acc)
				continue
			}
			report.Deleted = append(report.Deleted, acc)
			// 同一个账号若已登记为孤儿，顺手结案。
			// 2026-11-03：原来丢弃错误 ⇒ 账号已删、孤儿记录却一直挂着，且无任何痕迹。
			if _, err := s.db.Exec(ctx, `UPDATE orphan_node_accounts SET resolved_at=now(), resolved_note='每日对账已在节点上删除'
				WHERE node_id=$1::uuid AND user_id=$2 AND resolved_at IS NULL`, acc.NodeID, acc.UserID); err != nil {
				s.logger.Warn("node reconcile: 孤儿记录结案失败", "node", shortID(acc.NodeID), "user_id", acc.UserID, "error", err)
			}
		}
	}
	sortReconcile := func(list []nodeReconcileAccount) {
		sort.Slice(list, func(i, j int) bool {
			if list[i].NodeName != list[j].NodeName {
				return list[i].NodeName < list[j].NodeName
			}
			return list[i].UserID < list[j].UserID
		})
	}
	sortReconcile(report.Deleted)
	sortReconcile(report.Failed)
	sortReconcile(report.Skipped)
	return report
}

// recordNodeReconcile 写审计（系统动作，user_id 为空），有删除 / 失败 / 跳过 / 节点错误时推 Telegram。
func (s *Server) recordNodeReconcile(ctx context.Context, report nodeReconcileReport) {
	details, _ := json.Marshal(report)
	if _, err := s.db.Exec(ctx, `INSERT INTO audit_logs (user_id, action, resource_type, resource_id, ip_address, details)
		VALUES (NULL, 'reconcile_node_accounts', 'node_account', NULL, NULL, $1::jsonb)`, string(details)); err != nil {
		s.logger.Warn("node reconcile: audit insert failed", "error", err)
	}
	s.logger.Info("node reconcile done", "nodes", report.NodesChecked, "sub_accounts", report.SubAccounts,
		"deleted", len(report.Deleted), "failed", len(report.Failed), "skipped", len(report.Skipped), "node_errors", len(report.NodeErrors))
	if len(report.Deleted)+len(report.Failed)+len(report.Skipped)+len(report.NodeErrors) == 0 {
		return
	}
	chat := s.telegramChatID(ctx)
	if chat == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🧹 节点账号对账：检查 %d 台节点、%d 个订阅账号\n", report.NodesChecked, report.SubAccounts)
	section := func(title string, list []nodeReconcileAccount) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s %d 个：\n", title, len(list))
		for i, acc := range list {
			if i == 10 {
				fmt.Fprintf(&b, "… 另有 %d 个，详见审计日志\n", len(list)-10)
				break
			}
			line := fmt.Sprintf("· %s %s — %s", acc.NodeName, shortUserID(acc.UserID), acc.Reason)
			if acc.Error != "" {
				line += "：" + acc.Error
			}
			b.WriteString(line + "\n")
		}
	}
	section("已删除残留", report.Deleted)
	section("删除失败（明天重试）", report.Failed)
	section("跳过", report.Skipped)
	if len(report.NodeErrors) > 0 {
		names := make([]string, 0, len(report.NodeErrors))
		for name := range report.NodeErrors {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "\n未能检查的节点 %d 台：\n", len(names))
		for _, name := range names {
			fmt.Fprintf(&b, "· %s：%s\n", name, report.NodeErrors[name])
		}
	}
	if err := s.sendTelegramLong(ctx, chat, b.String()); err != nil {
		s.logger.Warn("node reconcile: telegram failed", "error", err)
	}
}

func shortUserID(uid string) string {
	if len(uid) > 12 {
		return uid[:12]
	}
	return uid
}

// StartNodeAccountReconcile 每小时检查一次，距上次成功跑完超过 23 小时就跑一轮。
// 上次运行时间存在 system_settings，容器重启不会让它重复跑或漏跑。
func (s *Server) StartNodeAccountReconcile(ctx context.Context) {
	go func() {
		// 开机 1 分钟后先补一次开户缺口：部署 / 重启后不用等第一个整点周期。
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		s.queueProvisioningGaps(ctx, "boot")
		// 对账再等 9 分钟：避开启动时的开户 / sweeper 高峰。
		select {
		case <-ctx.Done():
			return
		case <-time.After(9 * time.Minute):
		}
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			// 每小时兜底：新节点开户漏掉的（节点当时不通等）在这里补排队，见 provision_gapfill.go。
			s.queueProvisioningGaps(ctx, "hourly")
			s.maybeReconcileNodeAccounts(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func nodeReconcileSucceeded(ctx context.Context, report nodeReconcileReport) bool {
	return report.NodesChecked > 0 && len(report.NodeErrors) == 0 && len(report.Failed) == 0 && len(report.Skipped) == 0 && ctx.Err() == nil
}

func (s *Server) maybeReconcileNodeAccounts(ctx context.Context) {
	var last string
	err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, nodeReconcileSettingKey).Scan(&last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.logger.Warn("node reconcile: 无法读取上次成功时间，跳过本轮", "error", err)
		return
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(last)); err == nil && time.Since(t) < nodeReconcileInterval {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	report := s.ReconcileNodeAccounts(runCtx, false)
	s.recordNodeReconcile(runCtx, report)
	// ⚠️ 2026-10-30：节点列表或订阅记录读失败时不能把"上次运行"写成现在。
	// 否则这一轮等于白跑，下一次却要再等 23 小时 —— 对账缺口被人为拉长一整天。
	// 只有全部检查和删除都成功时才刷新时间；部分失败留到下一轮重试。
	if report.NodesChecked > 0 && nodeReconcileSucceeded(runCtx, report) {
		s.putSetting(runCtx, nodeReconcileSettingKey, time.Now().UTC().Format(time.RFC3339), "每日节点账号对账上次运行时间")
	}
}
