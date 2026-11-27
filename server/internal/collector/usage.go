package collector

// usage.go — 把各订阅在各节点上的用量落库，供「订阅中心」列表直接显示。
//
// 为什么需要它（2026-10-02 管理员要求「显示在线 IP 数量和用量」）
// ----------------------------------------------------------
// 用量只存在于**节点侧**（portal 的 users/list 里每个账号带
// traffic_used_bytes）。而订阅中心要显示"这条订阅用了多少"，
// 一条订阅在多个节点上各有账号，逐个实时查要走 N 次节点 API、
// 打开页面要等好几秒。
//
// 管理员选的是「采集器定期落库」：采集器本来每轮就在连各节点，
// 顺手把用量记进 subscription_usage，列表直接读本地表 —— 打开即见。
// 代价是最长一轮采集的延迟（默认 30 秒），对"看用量"完全够用。
//
// 为什么独立于 SSH 那条链路
// ------------------------
// 用量在 portal API 上，与 SSH 无关。有 SSH 的节点走 SSH 采指标、
// 没 SSH 的走 API 探状态 —— 但**用量一律走 API**，
// 否则"没绑 SSH 的节点看不到用量"这个坑会立刻出现
// （而按上一轮的产品定位，节点本来就不该依赖 SSH）。

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"nodeops/server/internal/portaltls"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// subscriptionNodeAccount 是 subscription_tokens.node_accounts 里的一项。
//
// 与 httpapi 包的 subscriptionNodeAccount 同构，但那边是 httpapi 的私有类型，
// 这里不能引（collector 不该依赖 httpapi —— 方向相反，会成环）。
// 所以这里按 JSON 契约独立声明一份，只用得上 user_id。
type subscriptionNodeAccount struct {
	UserID string `json:"user_id"`
}

// portalOnlineIP 是节点 portal online_ips 数组的一项。
//
// 来自节点鉴权时维护的内存态 ip_tracker，窗口与限流判定同一个常量
// （节点侧 180 秒）。2026-10-08 起由 portal 的 users/list 暴露 ——
// 在此之前它只喂给节点自己的页面渲染，面板拿不到。
type portalOnlineIP struct {
	IP       string `json:"ip"`
	LastSeen int64  `json:"last_seen"`
}

// portalUserUsage 是节点 portal users/list 返回的单条用户里我们关心的字段。
type portalUserUsage struct {
	CurrentOnline        *liveUserSnapshot `json:"current_online"`
	UserID               string            `json:"user_id"`
	MeasurementCurrentOK *bool             `json:"measurement_current_ok"`
	OnlineDevices        *int64            `json:"online_devices"`
	LegacyAuthValue      *int64            `json:"legacy_auth_value"`
	TrafficUsed          int64             `json:"traffic_used_bytes"`
	TrafficLimit         int64             `json:"traffic_limit_bytes"`
	MeasuredBytes        *int64            `json:"measured_bytes"`
	MeasurementOK        bool              `json:"measurement_ok"`
	MeasurementStartedAt string            `json:"measurement_started_at"`
	MeasurementSources   json.RawMessage   `json:"measurement_sources"`
	MeasurementGaps      json.RawMessage   `json:"measurement_gaps"`
	OnlineIPs            []portalOnlineIP  `json:"online_ips"`
}

// portalUsageEnvelope 是 users/list 的信封。
type portalUsageEnvelope struct {
	OK    bool              `json:"ok"`
	Error string            `json:"error"`
	Users []portalUserUsage `json:"users"`
	// 新版 portal 把节点级的 measurement_gaps 只放在信封里一次，不再复制进每个用户
	// （原来 53 个用户就 628 KB）。用户自己没带时用这份，旧版 portal 照旧按用户读。
	MeasurementGaps json.RawMessage `json:"measurement_gaps"`
	// measurement_sources 同理（节点门户 92bb5b4 起只放信封）：用户里没有就用这份。
	// 不兼容的话每个订阅都会被 measurementHealth 判成「当前采集异常/未知」（2026-10-30）。
	MeasurementSources json.RawMessage `json:"measurement_sources"`
}

// collectSubscriptionUsage 采一轮订阅用量。
//
// 对每个 (订阅, 节点) 组合：拿订阅里存的 user_id 去该节点的 users/list 里
// 找对应用户，把 used/limit 写进 subscription_usage。
//
// ⚠️ 采不到时**保留旧值并把 ok 置 false**，绝不清零。
// 把"节点不可达"写成 used_bytes=0，用户在界面上会看到"这条订阅用量 0"，
// 进而以为客户没在用 —— 而实际是没采到。这是本项目反复出现的那类
// "静默把未知说成 0"的坑，这里必须避开。
func (c *Collector) collectSubscriptionUsage(ctx context.Context) {
	rows, err := c.db.Query(ctx, usageMappingSQL)
	if err != nil {
		c.logger.Warn("collector: 查询订阅-节点映射失败", "error", err)
		return
	}

	type job struct {
		subID     string
		nodeID    string
		userID    string
		apiURL    string
		apiKey    []byte
		nodeLabel string
	}
	var jobs []job
	for rows.Next() {
		var subID, apiURL, nodeLabel, rowNodeID string
		var accountsRaw, apiKey []byte
		if err := rows.Scan(&subID, &accountsRaw, &apiURL, &apiKey, &nodeLabel, &rowNodeID); err != nil {
			rows.Close()
			c.logger.Warn("collector: 读取订阅-节点映射失败，本轮不采集用量", "error", err)
			return
		}
		var acc subscriptionNodeAccount
		if err := json.Unmarshal(accountsRaw, &acc); err != nil {
			continue
		}
		// SQL validates the entire mapping once, but transfers only this node account.
		if acc.UserID == "" {
			continue
		}
		jobs = append(jobs, job{
			subID: subID, nodeID: rowNodeID, userID: acc.UserID,
			apiURL: apiURL, apiKey: apiKey, nodeLabel: nodeLabel,
		})
	}
	// 2026-11-03：迭代中断时只采到一部分订阅，其余订阅这一轮悄悄不更新；
	// 与其拿半份清单去写库，不如整轮跳过并留下日志。
	if err := rows.Err(); err != nil {
		rows.Close()
		c.logger.Warn("collector: 订阅-节点映射读取不完整，本轮不采集用量", "error", err)
		return
	}
	rows.Close()

	if len(jobs) == 0 {
		return
	}

	// 按节点缓存一次 users/list，避免"同一节点上 10 个订阅就查 10 次"。
	perNode := map[string]map[string]portalUserUsage{}
	failures := map[string]string{}

	// 2026-11-06：先并发（最多 usagePrefetchConcurrency 路）把每个节点的 users/list 取回来，再逐条写库。
	// 原来是边遍历边串行请求：每个不可达节点要吃满 10 秒超时，几台节点挂掉就把整轮预算耗光，
	// 排在后面的健康节点被统一标成「本轮采集时间用尽」—— 一台机器的故障波及所有订阅的用量显示。
	fetchCtx, stopFetch := usageFetchContext(ctx)
	c.prefetchNodeUsers(fetchCtx, jobsNodeTargets(jobs, func(j job) (string, string, []byte) { return j.nodeID, j.apiURL, j.apiKey }), perNode, failures)
	stopFetch()
	// 进程优雅退出时不把未采完的节点批量标为过期，避免重启瞬间污染界面状态。
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	// Successful reads remain useful even if another node exhausted the read budget.
	// All persistence (including stale markers) shares one short, bounded grace period.
	ctx, stopWrite := detachedWriteContext(ctx, 5*time.Second)
	defer stopWrite()

	for _, j := range jobs {
		if ctx.Err() != nil {
			c.logger.Warn("collector: 用量落库预算用尽", "error", ctx.Err())
			return
		}
		users, cached := perNode[j.nodeID]
		if !cached {
			c.markUsageStale(ctx, j.subID, j.nodeID, j.userID, "本轮采集时间用尽，未读取到该节点用量")
			continue
		}

		if users == nil {
			// 该节点这一轮取不到 —— 保留旧值，标 ok=false。
			c.markUsageStale(ctx, j.subID, j.nodeID, j.userID, failures[j.nodeID])
			// 采集失败保留最后值；读取端按时间及健康标记判定未知。
			continue
		}
		u, found := users[j.userID]
		if !found {
			// 节点上没有这个账号。**这不是"用量 0"** ——
			// 是账号不存在（可能被节点侧手工删了、或还没生效）。
			// 明确记成采集失败并说明原因，别让它看起来像"没流量"。
			c.markUsageStale(ctx, j.subID, j.nodeID, j.userID, "节点上不存在该账号")
			// 采集失败保留最后值；读取端按时间及健康标记判定未知。
			continue
		}
		// 新来源与旧鉴权伪记账分表，不让未经计量的数据显示为0。
		var measured *int64
		var started *time.Time
		quality := u.MeasurementOK
		reason := ""
		if t, err := time.Parse(time.RFC3339Nano, u.MeasurementStartedAt); err == nil && u.MeasuredBytes != nil && *u.MeasuredBytes >= 0 {
			started = &t
			measured = u.MeasuredBytes
		} else {
			quality = false
			reason = "真实计量未初始化；历史鉴权值不是流量"
		}
		if !quality && reason == "" {
			reason = "真实计量存在缺口或部分协议采集失败"
		}
		sources, gaps := normalizeJSONObject(u.MeasurementSources), normalizeJSONArray(u.MeasurementGaps)
		if _, err := c.db.Exec(ctx, `
INSERT INTO subscription_measured_usage
 (subscription_id,node_id,user_id,measured_bytes,started_at,quality_ok,error,sources,gaps,collected_at,current_ok,online_devices)
VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,now(),$10,$11)
ON CONFLICT (subscription_id,node_id) DO UPDATE SET
 user_id=EXCLUDED.user_id, measured_bytes=EXCLUDED.measured_bytes,
 started_at=EXCLUDED.started_at, quality_ok=EXCLUDED.quality_ok,
 error=EXCLUDED.error, sources=EXCLUDED.sources, gaps=EXCLUDED.gaps, collected_at=now(),current_ok=EXCLUDED.current_ok,online_devices=EXCLUDED.online_devices`,
			j.subID, j.nodeID, j.userID, measured, started, quality, reason, string(sources), string(gaps), u.MeasurementCurrentOK, u.OnlineDevices); err != nil {
			c.logger.Warn("collector: 写入真实计量失败", "error", err)
		}

		// 在线 IP：整份替换这一对(订阅,节点)的当前集合。
		c.replaceOnlineIPs(ctx, j.subID, j.nodeID, u.OnlineIPs)
		c.replaceLiveConnection(ctx, j.subID, j.nodeID, j.userID, u.CurrentOnline)

		// 旧口径（portal 鉴权记账）继续同步。
		//
		// 为什么不因为"已经有真实计量了"就停写：面板显示的用量是
		// **两段相加**（旧口径历史 + 新口径计量）。旧口径这段此前
		// 停在 10-07 15:07 不再前进，那些数字仍会被显示 ——
		// 如果采集器不写，面板上的"历史"就成了一个永远不变的快照，
		// 看起来像"历史流量不会再增长"，而实际客户端重新认证时它会动。
		if _, err := c.db.Exec(ctx, `
INSERT INTO subscription_usage
 (subscription_id,node_id,user_id,used_bytes,limit_bytes,ok,error,sampled_at)
VALUES ($1::uuid,$2::uuid,$3,$4,$5,true,'',now())
ON CONFLICT (subscription_id,node_id) DO UPDATE SET
 user_id=EXCLUDED.user_id, used_bytes=EXCLUDED.used_bytes,
 limit_bytes=EXCLUDED.limit_bytes, ok=true, error='', sampled_at=now()`,
			j.subID, j.nodeID, j.userID, legacyUsageValue(u), u.TrafficLimit); err != nil {
			c.logger.Warn("collector: 写入旧口径记账失败", "error", err)
		}

	}
}

// replaceOnlineIPs 把某个 (订阅,节点) 的在线 IP 集合整份替换为当前值。
//
// 为什么是"整份替换"而不是增量 UPSERT：
//
//	在线 IP 是**状态**。增量写只会加、不会减 —— 客户端断开后那一行还在，
//	于是界面上会出现"这个 IP 还在用"，而它其实早就走了。
//	要让它消失，只能把该组合下不再出现的行删掉。
//
// 为什么放在一个事务里：
//
//	"删掉旧的 + 写入新的"必须原子。中间崩溃若留下空表，
//	界面会闪一下"0 个在线"，比显示陈旧数据更糟。
//
// 采到空数组是正常状态（真的没人连），不是失败 —— 所以不记 ok=false。
func (c *Collector) replaceOnlineIPs(ctx context.Context, subID, nodeID string, ips []portalOnlineIP) {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		c.logger.Warn("collector: 开启在线 IP 写入事务失败", "error", err)
		return
	}
	defer func() {
		rollbackCtx, stopRollback := detachedWriteContext(ctx, 2*time.Second)
		defer stopRollback()
		_ = tx.Rollback(rollbackCtx)
	}()

	if _, err := tx.Exec(ctx,
		`DELETE FROM subscription_online_ips WHERE subscription_id=$1::uuid AND node_id=$2::uuid`,
		subID, nodeID); err != nil {
		c.logger.Warn("collector: 清理旧在线 IP 失败", "error", err)
		return
	}
	if len(ips) > 0 {
		// 先去重（同一 IP 出现多条时与旧逻辑一致保留最后一条），再用单条 UNNEST 批量写入，避免逐条往返。
		dedupIdx := make(map[string]int, len(ips))
		ipList := make([]string, 0, len(ips))
		lastSeenList := make([]int64, 0, len(ips))
		for _, entry := range ips {
			ip := strings.TrimSpace(entry.IP)
			if ip == "" {
				// 节点侧偶尔会给出空串（tracker 的键不会，但防御性跳过更稳）。
				continue
			}
			if idx, exists := dedupIdx[ip]; exists {
				lastSeenList[idx] = entry.LastSeen
			} else {
				dedupIdx[ip] = len(ipList)
				ipList = append(ipList, ip)
				lastSeenList = append(lastSeenList, entry.LastSeen)
			}
		}
		if len(ipList) > 0 {
			if _, err := tx.Exec(ctx, `
INSERT INTO subscription_online_ips (subscription_id,node_id,ip,last_seen,collected_at)
SELECT $1::uuid, $2::uuid, u.ip, u.last_seen, now()
FROM unnest($3::text[], $4::bigint[]) AS u(ip, last_seen)
ON CONFLICT (subscription_id,node_id,ip) DO UPDATE SET
 last_seen=EXCLUDED.last_seen, collected_at=now()`,
				subID, nodeID, ipList, lastSeenList); err != nil {
				c.logger.Warn("collector: 批量写入在线 IP 失败", "error", err)
				return
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		c.logger.Warn("collector: 提交在线 IP 事务失败", "error", err)
	}
}

// markUsageStale 记录"这一轮没采到"，但**不动 used_bytes**。
//
// 用 UPSERT 而不是 UPDATE：首次采集就失败的组合也要留下行，
// 否则它在 subscription_usage 里不存在，前端连"这条没采到"都看不到，
// 会把它显示成"用量 0"——正是要避开的那种错觉。
func (c *Collector) markUsageStale(ctx context.Context, subID, nodeID, userID, reason string) {
	_, err := c.db.Exec(ctx, `INSERT INTO subscription_measured_usage
 (subscription_id,node_id,user_id,quality_ok,error,collected_at)
 VALUES ($1::uuid,$2::uuid,$3,false,$4,now())
 ON CONFLICT(subscription_id,node_id) DO UPDATE SET quality_ok=false,error=$4,collected_at=now(),current_ok=false`,
		subID, nodeID, userID, reason)
	if err != nil {
		c.logger.Warn("collector: 标记真实计量失败", "error", err)
	}
	// 2026-11-06：旧口径表同样标记「这次没采到」（ok=false），不动 used_bytes。
	// 原来只有成功路径写它、而且总是 ok=true：节点不可达或账号已不在节点上时，旧值一直以 ok=true
	// 参与上限汇总，store.go 里「凑不齐全部开户节点就不给总上限」对这种情况不起作用。
	if _, err := c.db.Exec(ctx, `UPDATE subscription_usage SET ok=false, error=$3
		WHERE subscription_id=$1::uuid AND node_id=$2::uuid`, subID, nodeID, reason); err != nil {
		c.logger.Warn("collector: 标记旧口径记账失败", "error", err)
	}
}

// fetchNodeUsers 读一个节点的 users/list。
//
// 这里不直接用 httpapi 的 portalCall：那会引入 collector → httpapi 的反向依赖。
// 复用 nodeapi_probe.go 里已有的 client 与 base-url 归一逻辑（同包内），
// 保证"端点拼法"只有一处定义。
func (c *Collector) fetchNodeUsers(ctx context.Context, apiURL string, apiKeyCipher []byte) ([]portalUserUsage, error) {
	if len(apiKeyCipher) == 0 || c.box == nil {
		return nil, errNoAPIKey
	}
	plain, err := c.box.Open(apiKeyCipher)
	if err != nil {
		return nil, errKeyDecrypt
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return fetchPortalUsers(callCtx, apiURL, string(plain))
}

// usagePrefetchConcurrency 是一轮用量采集里同时请求节点 users/list 的上限。
const usagePrefetchConcurrency = 6

type usageNodeTarget struct {
	nodeID string
	apiURL string
	apiKey []byte
}

// jobsNodeTargets 把订阅-节点任务按节点去重（同一节点上的多条订阅只请求一次）。
func jobsNodeTargets[J any](jobs []J, key func(J) (string, string, []byte)) []usageNodeTarget {
	seen := map[string]bool{}
	var out []usageNodeTarget
	for _, j := range jobs {
		id, url, k := key(j)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, usageNodeTarget{nodeID: id, apiURL: url, apiKey: k})
	}
	return out
}

// prefetchNodeUsers 并发取回各节点的 users/list。成功写进 perNode[nodeID]；失败写 perNode[nodeID]=nil
// 并在 failures 记原因。ctx 到期时没轮到的节点不写，交给调用方的「时间用尽」分支处理。
func (c *Collector) prefetchNodeUsers(ctx context.Context, targets []usageNodeTarget, perNode map[string]map[string]portalUserUsage, failures map[string]string) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, usagePrefetchConcurrency)
	for _, t := range targets {
		// 先检查上下文是否已结束，避免通道有空位时 select 伪随机继续派生已超时协程。
		if ctx.Err() != nil {
			wg.Wait()
			return
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(t usageNodeTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error("collector: 预拉取节点用量协程发生异常", "node_id", t.nodeID, "panic", r, "stack", string(debug.Stack()))
					mu.Lock()
					failures[t.nodeID] = "读取节点用量时发生内部异常"
					perNode[t.nodeID] = nil
					mu.Unlock()
				}
			}()
			fetched, err := c.fetchNodeUsers(ctx, t.apiURL, t.apiKey)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures[t.nodeID] = err.Error()
				perNode[t.nodeID] = nil
				return
			}
			users := make(map[string]portalUserUsage, len(fetched))
			for _, u := range fetched {
				users[u.UserID] = u
			}
			perNode[t.nodeID] = users
		}(t)
	}
	wg.Wait()
}

type constErr string

func (e constErr) Error() string { return string(e) }

const (
	errNoAPIKey   = constErr("节点未绑定 API 密钥")
	errKeyDecrypt = constErr("API 密钥解密失败")
)

// usageClient 是读节点 users/list 用的 HTTP 客户端。
//
// 与 nodeapi_probe.go 里那条探测链路**共用同一套安全姿态**（safeDialContext）：
// 逐 IP 校验公网、用解析后的 IP 连接、拒绝链路本地。
// 不复用 httpapi.portalClient 的理由见 nodeapi_probe.go 的文件头（包依赖方向）。
// usageResponseLimit 与 httpapi.portalResponseLimit 一致。
const usageResponseLimit = 8 << 20

var usageClient = &http.Client{
	Timeout: 15 * time.Second,
	// 2026-11-03：拒绝重定向。原来没设 CheckRedirect，30x 会把带 Bearer 的请求带到别处
	// （同主机名换端口时 Go 会继续带 Authorization）；与 httpapi.portalClient 一致。
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DialContext:         safeDialContext(10 * time.Second),
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		DisableKeepAlives:   true,
	},
}

// fetchPortalUsers 读一个节点的 /api/v1/users/list。
//
// 端点归一（补 /api/v1 前缀、处理带 token 的路径）复用
// nodeapi_probe.go 的 metaEndpointFromAPIURL —— 那是"端点拼法"的唯一出处，
// 这里只把它结尾的 node/meta 换成 users/list，避免两处各写一遍前缀逻辑。
func fetchPortalUsers(ctx context.Context, apiURL, apiKey string) ([]portalUserUsage, error) {
	metaURL, err := metaEndpointFromAPIURL(apiURL)
	if err != nil {
		return nil, err
	}
	// metaEndpointFromAPIURL 返回的是 .../api/v1/node/meta，替换尾段即可。
	listURL := strings.TrimSuffix(metaURL, "/node/meta") + "/users/list"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client, err := portaltls.Client(usageClient, req.URL.String())
	if err != nil {
		return nil, err
	}
	// 当命中 TLS 证书指纹时 portaltls.Client 会克隆一次性 Transport；先注册回收空闲连接，
	// 利用 defer 后进先出顺序在 resp.Body.Close() 之后彻底关闭可能残留的 HTTP/2 读循环与连接。
	if client != usageClient {
		defer client.CloseIdleConnections()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, constErr(shortErr(err))
	}
	defer resp.Body.Close()

	// 2026-11-03：原来上限 1 MiB 且不检测截断，超过就报「响应不是合法 JSON」——
	// 与 httpapi.portalCall 在 2026-10-28 修掉的是同一个坑（东京 users/list 628 KB）。
	// 截断后整台节点每轮都判采集失败，用量和在线 IP 不再更新。与 portalCall 同用 8 MiB。
	body, err := io.ReadAll(io.LimitReader(resp.Body, usageResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > usageResponseLimit {
		return nil, constErr("响应过大（超过 8 MiB），已拒绝解析")
	}
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return nil, constErr(fmt.Sprintf("节点 API 返回重定向（HTTP %d），已拒绝跟随", resp.StatusCode))
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, constErr("api_key 不被接受 (401)")
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, constErr("api_key 无权限访问 (403)")
	}
	var parsed portalUsageEnvelope
	if err := json.Unmarshal(body, &parsed); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, constErr(fmt.Sprintf("节点 API 返回 HTTP %d", resp.StatusCode))
		}
		return nil, constErr("响应不是合法 JSON")
	}
	if !parsed.OK {
		// 老节点没有这个端点时回 {"ok":false,"error":"API endpoint not found"}。
		// 把它的原话带上去，让运维能分辨"端点不存在"与"密钥不对"。
		if parsed.Error != "" {
			return nil, constErr("节点 API 报错: " + parsed.Error)
		}
		return nil, constErr("节点 API 未返回 ok")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, constErr(fmt.Sprintf("节点 API 返回异常状态码 HTTP %d", resp.StatusCode))
	}
	applyEnvelopeGaps(parsed.Users, parsed.MeasurementGaps)
	applyEnvelopeSources(parsed.Users, parsed.MeasurementSources)
	return parsed.Users, nil
}

// applyEnvelopeSources 用户自己没带（或带的是空/非法对象）measurement_sources 时，填入信封里的节点级合法非空对象。
func applyEnvelopeSources(users []portalUserUsage, sources json.RawMessage) {
	trimmed := strings.TrimSpace(string(sources))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" || !isValidJSONObject(trimmed) {
		return
	}
	for i := range users {
		own := strings.TrimSpace(string(users[i].MeasurementSources))
		if own == "" || own == "{}" || own == "null" || !isValidJSONObject(own) {
			users[i].MeasurementSources = sources
		}
	}
}

// applyEnvelopeGaps 用户自己没带（或为 null/非法数组）measurement_gaps 时，填入信封里的节点级合法数组。
func applyEnvelopeGaps(users []portalUserUsage, gaps json.RawMessage) {
	trimmed := strings.TrimSpace(string(gaps))
	if trimmed == "" || trimmed == "null" || !isValidJSONArray(trimmed) {
		return
	}
	for i := range users {
		own := strings.TrimSpace(string(users[i].MeasurementGaps))
		if own == "" || own == "null" || !isValidJSONArray(own) {
			users[i].MeasurementGaps = gaps
		}
	}
}

func isValidJSONObject(trimmed string) bool {
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid([]byte(trimmed))
}

func isValidJSONArray(trimmed string) bool {
	return len(trimmed) >= 2 && trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']' && json.Valid([]byte(trimmed))
}

// normalizeJSONObject 与 normalizeJSONArray 确保落入 jsonb 列的始终是合法 JSON 对象 / 数组，
// 防止 null、标量或类型错配写入数据库后引发下游 jsonb_each / jsonb_array_elements 报错。
func normalizeJSONObject(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if !isValidJSONObject(trimmed) {
		return json.RawMessage(`{}`)
	}
	return raw
}

func normalizeJSONArray(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if !isValidJSONArray(trimmed) {
		return json.RawMessage(`[]`)
	}
	return raw
}

// 兼容旧门户；新版明确区分历史值，绝不把真实累计再写成历史。
func legacyUsageValue(u portalUserUsage) int64 {
	if u.LegacyAuthValue != nil {
		if *u.LegacyAuthValue >= 0 {
			return *u.LegacyAuthValue
		}
		return 0
	}
	if u.MeasuredBytes != nil {
		return 0
	}
	if u.TrafficUsed < 0 {
		return 0
	}
	return u.TrafficUsed
}

// Reserve up to five seconds (half of a short remaining budget) for persistence.
func usageFetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		reserve := min(5*time.Second, max(remaining/2, 0))
		return context.WithDeadline(ctx, deadline.Add(-reserve))
	}
	return context.WithCancel(ctx)
}

// Validate mixed-type maps once per subscription, before expanding node rows.
const usageMappingSQL = `
WITH valid_subscriptions AS MATERIALIZED (
 SELECT st.id, st.node_accounts, st.deleted_at FROM subscription_tokens st
 WHERE st.deleted_at IS NULL AND NOT EXISTS (
  SELECT 1 FROM jsonb_each(st.node_accounts) bad
  WHERE jsonb_typeof(bad.value) NOT IN ('object', 'null')
     OR EXISTS (
      SELECT 1 FROM jsonb_each(CASE WHEN jsonb_typeof(bad.value)='object' THEN bad.value ELSE '{}'::jsonb END) field
      WHERE lower(field.key)='user_id' AND jsonb_typeof(field.value) NOT IN ('string', 'null')
     )
 )
)
SELECT st.id, na.account,
       COALESCE(n.api_url, ''), COALESCE(n.api_key, ''::bytea), COALESCE(s.name, ''),
       na.node_id::text
FROM valid_subscriptions st
JOIN LATERAL (
    SELECT CASE
               WHEN key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
               THEN key::uuid
           END AS node_id,
           value AS account
    FROM jsonb_each(st.node_accounts) AS e(key, value)
) na ON na.node_id IS NOT NULL
JOIN hysteria_nodes n ON n.id = na.node_id
JOIN servers s ON s.id = n.server_id
WHERE st.deleted_at IS NULL
  AND n.deleted_at IS NULL
  AND s.deleted_at IS NULL
  AND COALESCE(n.api_url, '') <> ''`
