package httpapi

// 批次在服务端执行，关闭页面不影响；重启控制台会明确标记中断，绝不假报成功。
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

var portalBundleFiles = []string{"portal.py", "portal_assets.py", "usage-meter-core-20261007-v1.py", "usage-meter-readers-20261007-v1.py", "usage-meter-runtime-20261007-v1.py", "usage-meter-xray-config-20261007-v1.py"}

type portalBatchItem struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Stage  string `json:"stage"`
	Error  string `json:"error,omitempty"`
	// Note 是不影响成败的提示（例如更新前就存在的历史统计缺口）。
	Note string `json:"-"`
}
type portalBatch struct {
	CancelRequested bool              `json:"cancel_requested"`
	ID              string            `json:"id"`
	CreatedBy       string            `json:"-"`
	Status          string            `json:"status"`
	Results         []portalBatchItem `json:"results"`
	Expected        map[string]string `json:"expected"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}
type portalBundle struct {
	Files            map[string]string `json:"files"`
	RuntimeRevision  string            `json:"runtime_revision"`
	MeterInitialized bool              `json:"meter_initialized"`
	Measurement      struct {
		OK      bool `json:"measurement_ok"`
		Sources map[string]struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"measurement_sources"`
	} `json:"measurement"`
}

func validatePortalBatchIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > 20 {
		return nil, errors.New("每批请选择 1–20 个节点")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if !uuidPattern.MatchString(id) {
			return nil, errors.New("节点 ID 不合法")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// 复用连接池拉取 GitHub 上游提交与文件指纹，避免每次发起批量更新都新建客户端与连接。
var portalBundleHTTPClient = &http.Client{
	Timeout:       8 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	},
}

var portalRepoSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

// portalInstallerRepo 读取节点门户更新来源仓库（格式 owner/repo），拆段严格校验防路径穿越。
func portalInstallerRepo() (string, error) {
	repo := strings.TrimSpace(os.Getenv("HY2_INSTALLER_REPO"))
	if repo == "" {
		return "", errors.New("未配置节点门户更新仓库，请设置环境变量 HY2_INSTALLER_REPO（格式如 owner/hysteria2-installer）")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", errors.New("环境变量 HY2_INSTALLER_REPO 格式不合法，应为 owner/repo")
	}
	for _, seg := range parts {
		if !portalRepoSegmentPattern.MatchString(seg) || strings.Contains(seg, "..") || strings.HasSuffix(seg, ".") {
			return "", errors.New("环境变量 HY2_INSTALLER_REPO 格式不合法，应为 owner/repo")
		}
	}
	return parts[0] + "/" + parts[1], nil
}

// 指纹和下载固定同一个不可变提交，不能一边比快照一边下载 main。
func fetchPortalBundle(ctx context.Context) (map[string]string, error) {
	repo, err := portalInstallerRepo()
	if err != nil {
		return nil, err
	}
	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/"+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "Teyir-portal-batch-update")
		resp, err := portalBundleHTTPClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("GitHub 版本读取失败（HTTP %d），未下发更新", resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := get("commits/main", &commit); err != nil {
		return nil, err
	}
	if !validBundleSHA(commit.SHA) {
		return nil, errors.New("GitHub 提交指纹不合法")
	}
	var rows []struct {
		Name string `json:"name"`
		SHA  string `json:"sha"`
	}
	if err := get("contents/?ref="+commit.SHA, &rows); err != nil {
		return nil, err
	}
	all := map[string]string{}
	for _, row := range rows {
		all[row.Name] = row.SHA
	}
	expected := map[string]string{"_commit": commit.SHA}
	for _, name := range portalBundleFiles {
		if !validBundleSHA(all[name]) {
			return nil, fmt.Errorf("GitHub 缺少 %s 指纹", name)
		}
		expected[name] = all[name]
	}
	return expected, nil
}
func validBundleSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, c := range sha {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Server) startPortalBatch(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, 428, map[string]string{"error": "需要确认批量门户更新"})
		return
	}
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		NodeIDs []string `json:"node_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid payload"})
		return
	}
	ids, err := validatePortalBatchIDs(input.NodeIDs)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// 先过危险确认、写限流与参数校验，再执行租约过期清理写库，防止无效请求放大写库压力。
	s.expirePortalBatches(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	expected, err := fetchPortalBundle(ctx)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	batch := portalBatch{CreatedBy: claims.Subject, Status: "running", Expected: expected, Results: []portalBatchItem{}}
	for _, id := range ids {
		target, e := s.loadPortalWebTarget(ctx, id)
		if e != nil {
			batch.Results = append(batch.Results, portalBatchItem{NodeID: id, Name: id, Status: "skipped", Stage: "凭据检查", Error: e.Error()})
			continue
		}
		batch.Results = append(batch.Results, portalBatchItem{NodeID: id, Name: target.NodeName, Status: "queued", Stage: "等待更新"})
	}
	results, _ := json.Marshal(batch.Results)
	hashes, _ := json.Marshal(expected)
	err = s.db.QueryRow(ctx, `INSERT INTO portal_update_batches(created_by,status,results,expected) VALUES($1::uuid,'running',$2::jsonb,$3::jsonb) RETURNING id::text,created_at,updated_at`, claims.Subject, string(results), string(hashes)).Scan(&batch.ID, &batch.CreatedAt, &batch.UpdatedAt)
	if err != nil {
		// 2026-11-06：按 SQLSTATE + 约束名判断，不按错误文本。文本里带约束名是驱动实现细节，
		// 一变就落到 500「无法创建更新任务」，而真实原因是「已有批量在跑」（客户端该等，不是服务端坏了）。
		if isUniqueViolationOn(err, "portal_update_one_running") {
			writeJSON(w, 409, map[string]string{"error": "已有批量门户更新正在执行，请先查看该任务"})
		} else {
			s.databaseError(w, r, err)
		}
		return
	}
	s.audit(r.Context(), r, "portal_batch_update", "portal_update_batch", batch.ID, map[string]any{"node_ids": ids, "expected": expected})
	writeJSON(w, http.StatusAccepted, batch)
	go s.runPortalBatch(batch)
}

func (s *Server) getPortalBatch(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id != "latest" {
		norm, ok := canonicalUUID(id)
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "任务 ID 不合法"})
			return
		}
		id = norm
	}
	s.expirePortalBatches(r.Context())
	q := `SELECT id::text,created_by::text,status,results,expected,created_at,updated_at,cancel_requested FROM portal_update_batches WHERE id=$1::uuid AND (created_by=$2::uuid OR $3) LIMIT 1`
	if id == "latest" {
		q = `SELECT id::text,created_by::text,status,results,expected,created_at,updated_at,cancel_requested FROM portal_update_batches WHERE (created_by=$2::uuid OR $3) AND $1::text='latest' ORDER BY created_at DESC LIMIT 1`
	}
	var b portalBatch
	var results, expected []byte
	err := s.db.QueryRow(r.Context(), q, id, claims.Subject, claims.Role == "admin").Scan(&b.ID, &b.CreatedBy, &b.Status, &results, &expected, &b.CreatedAt, &b.UpdatedAt, &b.CancelRequested)
	if err != nil {
		if isNoRows(err) {
			writeJSON(w, 404, map[string]string{"error": "没有可查看的更新任务"})
			return
		}
		// 2026-11-06：数据库错误走统一出口（记日志、不把原文回给前端）。原来只回一句「读取任务失败」，
		// 日志里什么都没有 —— 批量更新卡住时无从确认是不是库的问题。
		s.databaseError(w, r, err)
		return
	}
	if json.Unmarshal(results, &b.Results) != nil || json.Unmarshal(expected, &b.Expected) != nil {
		writeJSON(w, 500, map[string]string{"error": "任务记录损坏"})
		return
	}
	writeJSON(w, 200, b)
}

func (s *Server) savePortalBatch(ctx context.Context, b *portalBatch) error {
	encoded, err := json.Marshal(b.Results)
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE portal_update_batches SET status=$2,results=$3::jsonb,updated_at=now(),lease_expires_at=now()+interval '3 minutes' WHERE id=$1::uuid AND status='running'`, b.ID, b.Status, string(encoded))
	if err == nil && tag.RowsAffected() != 1 {
		return errors.New("任务已中断")
	}
	return err
}

// 启动时恢复中断标记；不自动重放写操作，避免无法判断上一轮是否已执行。
// 持续读取/创建任务时也检查租约，不需要重启控制台来释放孤儿任务。
func (s *Server) expirePortalBatches(ctx context.Context) {
	if s.db == nil {
		return
	}
	_, err := s.db.Exec(ctx, `UPDATE portal_update_batches SET status='interrupted',updated_at=now() WHERE status='running' AND lease_expires_at<now()`)
	if err != nil {
		s.logger.Warn("portal batch lease check failed", "error", err)
	}
}
func (s *Server) interruptPortalBatches() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.expirePortalBatches(ctx)
}

// portalBatchNodeBudget 是单台节点最坏耗时。2026-11-06：下发一轮的预算提到 portalHeavyInstallTimeout（620 秒，
// 盖住门户 do-upgrade 自己的 600 秒死线），单台最坏 = 两轮 ×（620 秒下发 + 4 秒等待 + 180 秒核验）+ 末尾探测
// ≈ 27 分钟；取 28 分钟留余量。
const portalBatchNodeBudget = 28 * time.Minute


// portalBatchBudget 按台数给整批预算（2026-11-03）。原来固定 90 分钟，
// 20 台慢节点最坏要 ~210 分钟：超时后在跑的节点失败，接着保存与取消检查都因 ctx 过期报错、
// 整批不再落库，租约过期后显示「中断」，剩下的节点一直停在 queued 且没有任何原因。
func portalBatchBudget(nodes int) time.Duration {
	if nodes < 1 {
		nodes = 1
	}
	return time.Duration(nodes)*portalBatchNodeBudget + 5*time.Minute
}

func (s *Server) runPortalBatch(b portalBatch) {
	defer func() {
		if rec := recover(); rec != nil {
			if s.logger != nil {
				s.logger.Error("portal batch panic", "batch_id", b.ID, "panic", rec, "stack", string(debug.Stack()))
			}
			b.Status = "interrupted"
			saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.savePortalBatchRetry(saveCtx, &b)
			saveCancel()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), portalBatchBudget(len(b.Results)))
	defer cancel()
	// 独立心跳避免长下载被误判过期；退出时停止，数据库故障后最多等一个租约。
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go s.portalBatchHeartbeat(heartbeatCtx, b.ID)

	for i := range b.Results {
		item := &b.Results[i]
		if item.Status != "queued" {
			continue
		}
		if ctx.Err() != nil {
			// 预算用完：剩下的节点明确标「未下发」，并用独立预算落库（ctx 已过期）。
			for j := i; j < len(b.Results); j++ {
				if b.Results[j].Status == "queued" || b.Results[j].Status == "running" {
					b.Results[j].Status = "failed"
					b.Results[j].Stage = "批次超时，未下发"
				}
			}
			break
		}
		var stopped bool
		if err := s.db.QueryRow(ctx, `SELECT cancel_requested FROM portal_update_batches WHERE id=$1::uuid`, b.ID).Scan(&stopped); err != nil {
			s.logger.Error("portal batch cancel check failed", "error", err)
			return
		}
		if stopped {
			for j := i; j < len(b.Results); j++ {
				if b.Results[j].Status == "queued" {
					b.Results[j].Status = "cancelled"
					b.Results[j].Stage = "未下发，已取消"
				}
			}
			break
		}
		item.Status = "running"
		item.Stage = "第一轮：升级门户"
		if err := s.savePortalBatchRetry(ctx, &b); err != nil {
			s.logger.Error("portal batch save failed", "error", err)
			return
		}
		err := s.updatePortalBatchNode(ctx, item, b.Expected, func(stage string) error { item.Stage = stage; return s.savePortalBatchRetry(ctx, &b) })
		if err != nil {
			item.Status = "failed"
			item.Error = err.Error()
			item.Stage = "更新失败"
		} else if item.Error != "" {
			item.Status = "warning"
			item.Stage = "文件已核验，计量有告警"
		} else {
			item.Status = "success"
			item.Stage = "两轮完成，文件及计量已核验"
			if item.Note != "" {
				item.Stage += "；" + item.Note
			}
		}
		// 本台的结论用独立预算落库：整批 ctx 可能恰在这台跑完时到期。
		saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = s.savePortalBatchRetry(saveCtx, &b)
		saveCancel()
		if err != nil {
			s.logger.Error("portal batch save failed", "error", err)
			return
		}
	}
	b.Status = "completed"
	invalidatePortalUpdateStatus()
	// 整体超时后仍用独立预算保存结果。
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finalCancel()
	if err := s.savePortalBatchRetry(finalCtx, &b); err != nil {
		s.logger.Error("portal batch completion save failed", "error", err)
	}
}

func verifyPortalBundle(bundle portalBundle, expected map[string]string) error {
	for _, name := range portalBundleFiles {
		want := expected[name]
		if len(want) != 40 || bundle.Files[name] != want {
			return fmt.Errorf("%s 文件指纹不匹配，不能判定更新成功", name)
		}
	}
	if bundle.MeterInitialized && bundle.RuntimeRevision != "2026-10-12-real-usage-v2" {
		return errors.New("计量运行时仍是旧版")
	}
	return nil
}

func (s *Server) updatePortalBatchNode(ctx context.Context, item *portalBatchItem, expected map[string]string, progress func(string) error) error {
	unlock, err := s.lockNodeWriteCtx(ctx, item.NodeID)
	if err != nil {
		return fmt.Errorf("等待节点写锁失败：%w", err)
	}
	defer unlock()
	target, err := s.loadPortalWebTarget(ctx, item.NodeID)
	if err != nil {
		return err
	}
	call := func(ctx context.Context, method, path string, form map[string]string) (json.RawMessage, error) {
		response, err := portalWebCall(ctx, target, method, path, form)
		if err != nil {
			return nil, errors.New(redactPortalError(err.Error(), target))
		}
		return response.Data, nil
	}
	return performPortalUpdate(ctx, item, expected, progress, call, waitPortalBatch)
}

func performPortalUpdate(ctx context.Context, item *portalBatchItem, expected map[string]string,
	progress func(string) error,
	call func(context.Context, string, string, map[string]string) (json.RawMessage, error),
	wait func(context.Context, time.Duration) error) error {
	var err error
	for round := 1; round <= 2; round++ {
		if err = progress(fmt.Sprintf("第 %d 轮：下发门户更新", round)); err != nil {
			return err
		}
		// 2026-11-06：门户侧 do-upgrade 自己留了 600 秒（六个文件、多个下载源回退）。原来这里给 120 秒：
		// 节点到 GitHub 慢时面板判「第 1 轮下发失败」、释放写锁转向下一台，节点其实还在替换文件并重启 ——
		// 面板报的状态和节点真实状态对不上。
		callCtx, cancel := context.WithTimeout(ctx, portalHeavyInstallTimeout)
		envelope, e := call(callCtx, http.MethodPost, "do-upgrade", portalUpdateForm(expected))
		cancel()
		if e != nil {
			return fmt.Errorf("第 %d 轮下发失败：%w", round, e)
		}
		var accepted struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(envelope, &accepted) != nil || !accepted.OK {
			return fmt.Errorf("第 %d 轮节点未确认接受更新", round)
		}
		if err = progress(fmt.Sprintf("第 %d 轮：等待重启并核验", round)); err != nil {
			return err
		}
		// 回应后门户延迟 1 秒重启，先等待，避免把旧进程的响应判作已恢复。
		if err = wait(ctx, 4*time.Second); err != nil {
			return err
		}
		deadline := time.Now().Add(3 * time.Minute)
		var bundle portalBundle
		var lastErr error
		for {
			probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
			response, e := call(probeCtx, http.MethodGet, "check-version", nil)
			probeCancel()
			if e == nil {
				var v struct {
					OK     bool         `json:"ok"`
					Bundle portalBundle `json:"portal_bundle"`
				}
				if json.Unmarshal(response, &v) == nil && v.OK {
					bundle = v.Bundle
					// 第一轮至少要求运行新版门户；第二轮核验六个文件和运行时。
					if round == 1 && bundle.Files["portal.py"] == expected["portal.py"] {
						break
					}
					if round == 2 {
						lastErr = verifyPortalBundle(bundle, expected)
						if lastErr == nil {
							break
						}
					} else {
						lastErr = errors.New("门户尚未运行本批次目标版本")
					}
				} else {
					lastErr = errors.New("版本响应不完整")
				}
			} else {
				lastErr = e
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("第 %d 轮恢复/核验超时：%v", round, lastErr)
			}
			if err = progress(fmt.Sprintf("第 %d 轮：等待恢复/文件一致", round)); err != nil {
				return err
			}
			if err = wait(ctx, 5*time.Second); err != nil {
				return err
			}
		}
		if round == 2 {
			item.Error, item.Note = portalBatchMeterVerdict(bundle)
		}
	}
	return nil
}
func waitPortalBatch(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func portalUpdateForm(expected map[string]string) map[string]string {
	form := map[string]string{"target": "portal"}
	if validBundleSHA(expected["_commit"]) {
		form["ref"] = expected["_commit"]
	}
	return form
}
func redactPortalError(message string, target *portalWebTarget) string {
	for _, secret := range []string{target.PathToken, target.Password} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}
func (s *Server) savePortalBatchRetry(ctx context.Context, b *portalBatch) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = s.savePortalBatch(ctx, b); err == nil {
			return nil
		}
		if e := waitPortalBatch(ctx, time.Duration(attempt+1)*time.Second); e != nil {
			return e
		}
	}
	return err
}
func (s *Server) portalBatchHeartbeat(ctx context.Context, id string) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := s.db.Exec(ctx, `UPDATE portal_update_batches SET lease_expires_at=now()+interval '3 minutes' WHERE id=$1::uuid AND status='running'`, id)
			if err != nil {
				s.logger.Warn("portal batch heartbeat failed", "error", err)
			}
		}
	}
}
func (s *Server) cancelPortalBatch(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	id, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "任务 ID 不合法"})
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE portal_update_batches SET cancel_requested=true WHERE id=$1::uuid AND status='running' AND (created_by=$2::uuid OR $3)`, id, claims.Subject, claims.Role == "admin")
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() != 1 {
		writeJSON(w, 409, map[string]string{"error": "任务不存在、已结束或没有取消权限"})
		return
	}
	s.audit(r.Context(), r, "cancel_portal_batch", "portal_update_batch", id, nil)
	writeJSON(w, 200, map[string]string{"status": "cancel_requested", "message": "当前节点完成后取消剩余节点，不强制打断门户更新"})
}
func (s *Server) listPortalBatches(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	s.expirePortalBatches(r.Context())
	rows, err := s.db.Query(r.Context(), `SELECT id::text,status,created_at,updated_at FROM portal_update_batches WHERE created_by=$1::uuid OR $2 ORDER BY created_at DESC LIMIT 30`, claims.Subject, claims.Role == "admin")
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id, status string
		var created, updated time.Time
		if err = rows.Scan(&id, &status, &created, &updated); err != nil {
			s.databaseError(w, r, err)
			return
		}
		list = append(list, map[string]any{"id": id, "status": status, "created_at": created, "updated_at": updated})
	}
	if err = rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, 200, list)
}

// portalBatchMeterVerdict 区分「更新后计量当前不可用」（告警）与「计量有历史缺口」（仅提示）。
//
// 2026-10-14 菲律宾-马尼拉②实测：更新本身完全正常、各数据源当前都在采，
// 但 10-10 那次服务重启留下的缺口让 measurement_ok 一直是 false，
// 于是每次批量更新都报「计量告警」。历史缺口在统计周期滚过去之前
// 永远消不掉，和本次更新无关，把它当失败只会让真正的故障被淹没。
func portalBatchMeterVerdict(bundle portalBundle) (warning, note string) {
	if !bundle.MeterInitialized {
		return "门户文件已更新，但计量未初始化；请检查节点计量配置与日志", ""
	}
	var failing []string
	for name, src := range bundle.Measurement.Sources {
		if !src.OK {
			failing = append(failing, strings.TrimSpace(name+" "+src.Error))
		}
	}
	if len(failing) > 0 {
		sort.Strings(failing)
		return "门户文件已更新，但计量采集当前异常：" + strings.Join(failing, "；"), ""
	}
	if !bundle.Measurement.OK {
		return "", "计量当前正常，存在更新前的历史统计缺口（不影响本次更新）"
	}
	return "", ""
}
