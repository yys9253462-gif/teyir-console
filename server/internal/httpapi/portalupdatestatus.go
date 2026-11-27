package httpapi

// portalupdatestatus.go — 批量更新门户前，告诉操作者哪些节点真的需要更新（2026-10-15）。
//
// 此前弹窗只列节点名，「选择可更新节点」会把所有绑了门户账号的节点全勾上，
// 已是最新的节点也要再走一遍两轮更新 + 重启。判据与批量更新本身一致：
// 节点 check-version 返回的六个文件 blob 指纹 vs GitHub main 上的指纹
// （fetchPortalBundle），逐个文件比对 —— 不看版本号，因为不改版本号的修复也是更新。

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

const portalUpdateStatusTTL = 60 * time.Second

type portalUpdateNode struct {
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	PortalVersion string   `json:"portal_version"`
	UpToDate      *bool    `json:"up_to_date"` // nil = 无法判断
	OutdatedFiles []string `json:"outdated_files"`
	Error         string   `json:"error,omitempty"`
}

type portalUpdateStatus struct {
	Error     string             `json:"error,omitempty"`
	Nodes     []portalUpdateNode `json:"nodes"`
	CheckedAt time.Time          `json:"checked_at"`
}

type portalUpdateStatusFlight struct {
	done chan struct{}
	res  portalUpdateStatus
}

var portalUpdateStatusCache struct {
	sync.Mutex
	result   *portalUpdateStatus
	gen      uint64
	inflight *portalUpdateStatusFlight
}

// GET /api/portal-update-status[?refresh=1]
func (s *Server) getPortalUpdateStatus(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	writeJSON(w, http.StatusOK, s.cachedPortalUpdateStatus(r.Context(), r.URL.Query().Get("refresh") == "1"))
}

// 缓存 60 秒并合并并发请求：GitHub 匿名 API 每小时只有 60 次额度，
// 节点门户又是单线程的，两个标签页同时打开弹窗不该各问一遍。
func (s *Server) cachedPortalUpdateStatus(ctx context.Context, force bool) portalUpdateStatus {
	c := &portalUpdateStatusCache
	c.Lock()
	if !force && c.result != nil && time.Since(c.result.CheckedAt) < portalUpdateStatusTTL {
		res := *c.result
		c.Unlock()
		return res
	}
	if flight := c.inflight; flight != nil {
		c.Unlock()
		select {
		case <-flight.done:
			return flight.res
		case <-ctx.Done():
			return portalUpdateStatus{Error: "检查尚未完成，请稍后重试", Nodes: []portalUpdateNode{}}
		}
	}
	flight := &portalUpdateStatusFlight{done: make(chan struct{})}
	startGen := c.gen
	c.inflight = flight
	c.Unlock()

	// 在独立协程中执行并配合异常恢复，确保即使发生异常也必清空 inflight 并唤醒所有等待者；
	// 写回缓存前比对版本号，防止批量更新完成后在途旧检查覆盖最新状态。
	go func() {
		res := portalUpdateStatus{Error: "检查门户版本状态时发生内部异常", Nodes: []portalUpdateNode{}, CheckedAt: time.Now()}
		defer func() {
			if rec := recover(); rec != nil && s.logger != nil {
				s.logger.Error("portal update status panic", "panic", rec, "stack", string(debug.Stack()))
			}
			flight.res = res
			c.Lock()
			if c.gen == startGen {
				copied := res
				c.result = &copied
			}
			if c.inflight == flight {
				c.inflight = nil
			}
			c.Unlock()
			close(flight.done)
		}()
		// 独立超时：发起者关掉弹窗不应让共享这次检查的其它请求失败。
		fetchCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res = s.checkPortalUpdateStatus(fetchCtx)
		cancel()
	}()

	select {
	case <-flight.done:
		return flight.res
	case <-ctx.Done():
		return portalUpdateStatus{Error: "检查尚未完成，请稍后重试", Nodes: []portalUpdateNode{}}
	}
}

func (s *Server) checkPortalUpdateStatus(ctx context.Context) portalUpdateStatus {
	res := portalUpdateStatus{Nodes: []portalUpdateNode{}}
	expected, err := fetchPortalBundle(ctx)
	if err != nil {
		res.Error = "无法读取 GitHub 上的最新门户，暂时判断不了是否需要更新：" + err.Error()
		expected = nil
	}

	rows, err := s.db.Query(ctx, `SELECT id, name FROM hysteria_nodes
		WHERE deleted_at IS NULL AND portal_basic_user IS NOT NULL AND portal_basic_password IS NOT NULL
		ORDER BY created_at`)
	if err != nil {
		res.Error = "读取节点失败"
		res.CheckedAt = time.Now()
		return res
	}
	for rows.Next() {
		n := portalUpdateNode{OutdatedFiles: []string{}}
		if err := rows.Scan(&n.NodeID, &n.Name); err != nil {
			rows.Close()
			if s.logger != nil {
				s.logger.Warn("portal update status: node list scan failed:", "error", err)
			}
			res.Error = "读取节点列表失败，请查看服务端日志"
			res.CheckedAt = time.Now()
			return res
		}
		res.Nodes = append(res.Nodes, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		if s.logger != nil {
			s.logger.Warn("portal update status: node list read failed:", "error", err)
		}
		res.Error = "读取节点列表失败，请查看服务端日志"
		res.CheckedAt = time.Now()
		return res
	}
	rows.Close()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range res.Nodes {
		wg.Add(1)
		go func(n *portalUpdateNode) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.checkOnePortalUpdate(ctx, n, expected)
		}(&res.Nodes[i])
	}
	wg.Wait()
	res.CheckedAt = time.Now()
	return res
}

func (s *Server) checkOnePortalUpdate(ctx context.Context, n *portalUpdateNode, expected map[string]string) {
	callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	target, err := s.loadPortalWebTarget(callCtx, n.NodeID)
	if err != nil {
		n.Error = "无法连接门户：" + err.Error()
		return
	}
	// 与其它门户读取共用同一个每节点并发闸门（门户是单线程 http.server）。
	release, err := s.lockNodeRead(callCtx, n.NodeID)
	if err != nil {
		n.Error = "节点门户繁忙，稍后重试"
		return
	}
	env, err := portalWebCall(callCtx, target, http.MethodGet, "check-version", nil)
	release()
	if err != nil {
		n.Error = "无法读取门户版本：" + redactPortalError(err.Error(), target)
		return
	}
	var body struct {
		PortalCurrent string `json:"portal_current"`
		Bundle        *struct {
			Files map[string]string `json:"files"`
		} `json:"portal_bundle"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		n.Error = "门户版本返回格式无法解析"
		return
	}
	n.PortalVersion = body.PortalCurrent
	n.OutdatedFiles, n.UpToDate = comparePortalFiles(body.Bundle, expected)
}

// comparePortalFiles 逐文件比对指纹。expected 为 nil（GitHub 读不到）时不下结论。
// 很老的门户不返回文件指纹，一定需要更新。
func comparePortalFiles(bundle *struct {
	Files map[string]string `json:"files"`
}, expected map[string]string) ([]string, *bool) {
	outdated := []string{}
	if expected == nil {
		return outdated, nil
	}
	for _, name := range portalBundleFiles {
		if bundle == nil || bundle.Files[name] == "" || bundle.Files[name] != expected[name] {
			outdated = append(outdated, name)
		}
	}
	sort.Strings(outdated)
	ok := len(outdated) == 0
	return outdated, &ok
}

// invalidatePortalUpdateStatus 在批量更新结束后调用，递增版本号并清空缓存，防止在途旧检查回写过期结果。
func invalidatePortalUpdateStatus() {
	portalUpdateStatusCache.Lock()
	portalUpdateStatusCache.result = nil
	portalUpdateStatusCache.gen++
	portalUpdateStatusCache.Unlock()
}
