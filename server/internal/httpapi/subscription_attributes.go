package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// 属性同步仅由既有PATCH/对账入口调用，不新增后台存量扫描。
func subscriptionAttributes(rec *subscriptionRecord, now time.Time) map[string]any {
	status := "disabled"
	if rec.Enabled && !rec.expired(now) {
		status = "active"
	}
	expiry := now.AddDate(10, 0, 0).Unix()
	if rec.ExpiresAt != nil {
		expiry = rec.ExpiresAt.Unix()
	}
	return map[string]any{"expires_at": expiry, "ip_limit": rec.IPLimit, "status": status}
}

func (s *Server) syncSubscriptionNodeAttributes(ctx context.Context, rec *subscriptionRecord) subscriptionStateOutcome {
	out := subscriptionStateOutcome{Failed: map[string]string{}}
	if rec == nil || len(rec.nodeAccounts) == 0 {
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
	// 与状态联动共用名称预取与聚合逻辑：保留已删节点可读名称、识别 gone 状态并防同名节点互相覆盖。
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
			// 每个节点单独限时，防止慢节点耗尽整个请求周期导致后续节点同步中途取消。
			nodeCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
			defer cancel()
			target, err := s.loadPortalTarget(nodeCtx, j.nodeID)
			if errors.Is(err, errPortalNodeNotFound) {
				r.err = "节点记录已删除（这条映射是残留，可在节点页清理）"
				r.gone = true
				results[i] = r
				return
			}
			if err != nil {
				r.err = err.Error()
				results[i] = r
				return
			}
			r.name = target.NodeName
			attrs := subscriptionAttributes(rec, time.Now())
			attrs["user_id"] = j.userID
			// 先GET权威用户，不允许缺失用户通过编辑被重新开户。
			_, err = portalCall(nodeCtx, target.BaseURL, target.APIKey, http.MethodGet, "/users/get/"+j.userID, nil)
			if err == nil {
				envelope, updateErr := portalCall(nodeCtx, target.BaseURL, target.APIKey, http.MethodPost, "/users/update", attrs)
				err = updateErr
				if err == nil {
					var result struct {
						OK bool `json:"ok"`
					}
					if json.Unmarshal(envelope.Data, &result) != nil || !result.OK {
						err = errors.New("节点未确认属性同步")
					}
				}
				// 读回核对到期时间，防止门户静默钳制或忽略属性变更导致谎报成功。
				if err == nil {
					if verifyErr := verifySyncedAccountAttributes(nodeCtx, target, j.userID, rec); verifyErr != nil {
						err = verifyErr
					}
				}
			}
			if err != nil {
				r.err = err.Error()
			}
			results[i] = r
		}(i, j)
	}
	wg.Wait()
	return collectSubscriptionStatusResults(results)
}

// verifySyncedAccountAttributes 读回 /users/get，比对这次同步真正要改的字段。
// 与 verifyAccountExpiry 同一思路：HTTP 200 / ok:true 只说明「请求被接受」，不说明「值已经变了」。
//
// ⚠️ 复用 verifyAccountExpiry 而不是自己再解一遍：/users/get 的返回是 `{"user":{"expires_at":…}}`，
// 按顶层字段解析会永远得到 0、把每个节点都判成失败（比不校验更糟）。
func verifySyncedAccountAttributes(ctx context.Context, target *portalTarget, userID string, rec *subscriptionRecord) error {
	if rec.ExpiresAt == nil {
		return nil // 永久订阅没有到期时间可比对
	}
	envelope, err := portalCall(ctx, target.BaseURL, target.APIKey, http.MethodGet, "/users/get/"+userID, nil)
	if err != nil {
		return fmt.Errorf("读回属性失败：%w", err)
	}
	if err := verifyAccountExpiry(envelope.Data, rec.ExpiresAt.Unix()); err != nil {
		return fmt.Errorf("属性同步后读回不一致：%w", err)
	}
	return nil
}

// 独立可测试的契约：先读再更新；存在账号绝不create；旧节点缺接口绝不降级覆盖。
type accountPortalCall func(string, string, map[string]any) error

// syncAccountPassword 是同步时下发给节点的口令：订阅映射里已有的就用它，没有才用确定性推导值。
//
// 2026-11-04：原来一律推导值。映射里的口令就是买家配置里的口令：两者不同（确定性密码之前开的号、
// 从未对账过）时，任何一次后台同步（新节点上架触发补开户、续费、改设备数）都会在**每台**节点上
// 悄悄换掉买家正在用的口令，已导入的客户端全部认证失败。映射为空时两者相同，行为不变。
func syncAccountPassword(rec *subscriptionRecord, nodeID string) string {
	if acc, ok := rec.nodeAccounts[nodeID]; ok && acc.Password != "" {
		return acc.Password
	}
	return provisionAccountPassword(rec, nodeID)
}

func syncAccount(rec *subscriptionRecord, nodeID string, now time.Time, call accountPortalCall) error {
	uid := provisionUserID(rec)
	err := call(http.MethodGet, "/users/get/"+uid, nil)
	attrs := subscriptionAttributes(rec, now)
	attrs["user_id"] = uid
	// 确定性推导仍是新开户的来源（provisionAccountPassword）；已有映射时沿用映射里的口令。
	attrs["password"] = syncAccountPassword(rec, nodeID)
	if err == nil {
		return call(http.MethodPost, "/users/update", attrs)
	}
	if !isUserNotFound(err) {
		return err
	}
	if !rec.Enabled || rec.expired(now) {
		return errors.New("停用或到期订阅不允许重新开户")
	}
	attrs["create_only"] = true // 节点持锁检查，封住GET后并发创建造成的覆盖窗口
	delete(attrs, "status")
	return call(http.MethodPost, "/users/create", attrs)
}

func (s *Server) reconcileOneNode(ctx context.Context, rec *subscriptionRecord, c subscriptionCandidate) provisionResult {
	out := provisionResult{NodeID: c.id, NodeName: c.name}
	if s.nodeMaintenance(ctx, c.id) {
		out.Err = errors.New("节点正在迁移，稍后重试")
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	key, err := s.openNodeKey(c.ciphertext)
	if err != nil {
		out.Err = err
		return out
	}
	out.Err = syncAccount(rec, c.id, time.Now(), func(method, path string, payload map[string]any) error {
		envelope, err := portalCall(ctx, c.apiURL, key, method, path, payload)
		if err != nil {
			return err
		}
		var result struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(envelope.Data, &result) != nil || !result.OK {
			return errors.New("节点未确认属性同步")
		}
		return nil
	})
	if out.Err == nil && rec.ExpiresAt != nil {
		envelope, err := portalCall(ctx, c.apiURL, key, http.MethodGet, "/users/get/"+provisionUserID(rec), nil)
		if err != nil {
			out.Err = err
		} else {
			out.Err = verifyAccountExpiry(envelope.Data, rec.ExpiresAt.Unix())
		}
	}

	if out.Err == nil {
		out.Account = subscriptionNodeAccount{UserID: provisionUserID(rec), Password: syncAccountPassword(rec, c.id), CreatedAt: time.Now().Unix()}
		if old, ok := rec.nodeAccounts[c.id]; ok {
			out.Account.CreatedAt = old.CreatedAt
		}
	}
	return out
}
