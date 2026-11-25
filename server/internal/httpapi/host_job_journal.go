package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"
)

func (s *Server) restoreInstallJobs() {
	if s.db == nil {
		return
	}
	installJobs.mu.Lock()
	defer installJobs.mu.Unlock()
	installJobs.db = s.db
	installJobs.owner = fmt.Sprintf("api-%d", time.Now().UnixNano())
	installJobs.ready = false
	if err := installJobs.restoreLocked(context.Background()); err != nil {
		s.logger.Error("host job journal unavailable", "error", err)
		return
	}
	installJobs.ready = true
}
func (st *installJobStore) restoreLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := st.db.Exec(ctx, `UPDATE host_operation_jobs SET status='interrupted',payload=jsonb_set(jsonb_set(jsonb_set(payload,'{status}','"interrupted"'),'{step}','"任务进程已中断，需要核验远端状态"'),'{error}','"远端操作可能仍在运行，确认已停止后才能解除互斥"'),updated_at=now() WHERE status='running' AND heartbeat<now()-interval '90 seconds'`)
	if err != nil {
		return err
	}
	rows, err := st.db.Query(ctx, `SELECT payload FROM host_operation_jobs ORDER BY (status IN ('running','interrupted')) DESC,updated_at DESC LIMIT 500`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var job installJob
		if err := json.Unmarshal(raw, &job); err != nil {
			return fmt.Errorf("decode host operation job: %w", err)
		}
		// 本进程自己跑的任务以内存为准（里面有完整日志和一次性的登录信息），不要被库里打过码的副本覆盖。
		if _, ok := st.jobs[job.ID]; ok && st.owned[job.ID] {
			continue
		}
		// 结束超过保留期的任务不再捞回内存（见 evictLocked），否则淘汰形同虚设。
		if job.EndedAt != nil && job.Status != "running" && job.Status != "interrupted" && time.Since(*job.EndedAt) > installJobRetention {
			continue
		}
		cp := job
		st.jobs[job.ID] = &cp
		for _, id := range job.Servers {
			if job.Status == "running" || job.Status == "interrupted" {
				st.byServer[id] = job.ID
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	st.evictLocked(time.Now())
	return nil
}
func (st *installJobStore) createLocked(ctx context.Context, job *installJob, servers []string) error {
	if !st.ready || st.db == nil {
		return errors.New("任务日志未就绪，禁止开始新的主机操作")
	}
	job.Servers = servers
	stored := *job
	stored.Access = nil
	stored.Error = maskHostJobOutput(job.Error)
	stored.Log = make([]string, len(job.Log))
	for i, line := range job.Log {
		stored.Log[i] = maskHostJobOutput(line)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	tx, err := st.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// 2026-11-06：调用方持有 installJobs.mu。主机行锁原来无限期等待：换 IP 持锁调 Cloudflare（每个节点最多 30 秒）时，
	// 这里一等就是一两分钟，期间所有安装 / 迁移任务的日志写入、进度查询、心跳巡检都卡在 installJobs.mu 上。
	// 只在本事务内设锁等待上限，等不到就当「主机正被其它操作占用」。
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	// 与 executor 的领取使用同一主机行锁；按 id 排序防止双主机迁移互相死锁。
	rows, err := tx.Query(ctx, `SELECT id FROM servers WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, servers)
	if err != nil {
		if isLockNotAvailable(err) {
			return errHostJobConflict{"主机正被其它操作占用（例如换 IP 同步），请稍后再试"}
		}
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		if isLockNotAvailable(err) {
			return errHostJobConflict{"主机正被其它操作占用（例如换 IP 同步），请稍后再试"}
		}
		return err
	}
	var running bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operation_tasks WHERE server_id=ANY($1::uuid[]) AND status='running')`, servers).Scan(&running); err != nil {
		return err
	}
	if running {
		return errHostJobConflict{"主机已有执行中的操作任务，请完成后再安装或迁移"}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO host_operation_jobs(id,payload,status,owner) VALUES($1,$2::jsonb,'running',$3)`, job.ID, string(raw), st.owner); err != nil {
		return err
	}
	for _, id := range servers {
		if _, err = tx.Exec(ctx, `INSERT INTO host_operation_leases(server_id,job_id) VALUES($1::uuid,$2)`, id, job.ID); err != nil {
			// 2026-11-04：只有主键冲突才是「已有租约」；连接断开、外键错误之类原样上报，
			// 不能让操作者去核验一个根本不存在的原任务。
			if isUniqueViolation(err) {
				return errHostJobConflict{"主机已有持久化任务租约，请先核验原任务"}
			}
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if st.owned == nil {
		st.owned = map[string]bool{}
	}
	st.owned[job.ID] = true
	return nil
}
func (st *installJobStore) persistLocked(job *installJob) error {
	if st.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if len(job.Log) > 200 {
		job.Log = job.Log[len(job.Log)-200:]
	}
	// 只对**落库的副本**打码：内存里的日志保持完整，安装窗口能看到完整的后台地址与账号密码；
	// 数据库里长期留存的历史不含凭据。（原来直接改 job.Log，窗口里也被省略了。）
	stored := *job
	stored.Access = nil // 账号密码只给安装窗口看，不落库
	stored.Error = maskHostJobOutput(job.Error)
	stored.Log = make([]string, len(job.Log))
	for i, line := range job.Log {
		stored.Log[i] = maskHostJobOutput(line)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	tx, err := st.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE host_operation_jobs SET payload=$2::jsonb,status=$3,heartbeat=now(),updated_at=now() WHERE id=$1`, job.ID, string(raw), job.Status); err != nil {
		return err
	}
	if job.Status == "failed" || job.Status == "success" {
		if _, err = tx.Exec(ctx, `DELETE FROM host_operation_leases WHERE job_id=$1`, job.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// errHostJobConflict 表示「这台主机已经有任务」——是客户端可处理的冲突（409），不是服务不可用。
type errHostJobConflict struct{ msg string }

func (e errHostJobConflict) Error() string { return e.msg }

// writeHostJobCreateError 把 createLocked 的失败映射成正确的状态码（2026-11-04）：
// 原来一律 503，前端 / Telegram 按「服务暂时不可用」处理或自动重试。
func (s *Server) writeHostJobCreateError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict errHostJobConflict
	if errors.As(err, &conflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": conflict.msg})
		return
	}
	s.databaseError(w, r, err)
}

func (s *Server) StartHostJobJournal(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		tick := func() {
			defer func() {
				if rec := recover(); rec != nil && s.logger != nil {
					s.logger.Error("host job journal panic", "panic", rec, "stack", string(debug.Stack()))
				}
			}()
			installJobs.mu.Lock()
			defer installJobs.mu.Unlock()
			if installJobs.db != nil {
				installJobs.retryUnsavedLocked()
				for id := range installJobs.owned {
					if j := installJobs.jobs[id]; j != nil && j.Status == "running" {
						// 2026-11-04：带超时。这里持有全局任务锁，与 persistLocked 一样限 5 秒。
						hbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
						if _, err := installJobs.db.Exec(hbCtx, `UPDATE host_operation_jobs SET heartbeat=now() WHERE id=$1 AND owner=$2 AND status='running'`, id, installJobs.owner); err != nil {
							s.logger.Warn("host job heartbeat failed", "job", id, "error", err)
						}
						cancel()
					}
				}
				if err := installJobs.restoreLocked(ctx); err != nil {
					installJobs.ready = false
					s.logger.Warn("host task recovery unavailable", "error", err)
				} else {
					installJobs.ready = true
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tick()
			}
		}
	}()
}
func (s *Server) resolveInstallJob(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, 428, map[string]string{"error": "请确认远端安装/迁移进程确已停止，再解除任务互斥"})
		return
	}
	var in struct {
		RemoteStopped bool `json:"confirm_remote_stopped"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) != nil || !in.RemoteStopped {
		writeJSON(w, 400, map[string]string{"error": "缺少远端进程已停止的确认"})
		return
	}
	id := r.PathValue("id")
	installJobs.mu.Lock()
	job, ok := installJobs.jobs[id]
	if !ok || job.Status != "interrupted" {
		installJobs.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "只有已中断且完成远端核验的任务可以结案"})
		return
	}
	prevErr, prevEnded := job.Error, job.EndedAt
	job.Status = "failed"
	job.Error = "操作者确认远端进程停止后结案"
	now := time.Now()
	job.EndedAt = &now
	if err := installJobs.persistLocked(job); err != nil {
		// 2026-11-06：整份还原。原来只还原 Status，任务列表上留着一条写着「已结案」的 interrupted 任务。
		job.Status, job.Error, job.EndedAt = "interrupted", prevErr, prevEnded
		installJobs.mu.Unlock()
		s.databaseError(w, r, err)
		return
	}
	serverID := job.ServerID
	for _, sid := range job.Servers {
		if installJobs.byServer[sid] == id {
			delete(installJobs.byServer, sid)
		}
	}
	installJobs.mu.Unlock()
	s.audit(r.Context(), r, "resolve_interrupted_host_job", "server", serverID, map[string]any{"job_id": id})
	writeJSON(w, 200, map[string]string{"status": "resolved"})
}
func (s *Server) listInstallJobs(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT payload FROM host_operation_jobs ORDER BY updated_at DESC LIMIT 100`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	jobs := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			s.databaseError(w, r, err)
			return
		}
		jobs = append(jobs, json.RawMessage(raw))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}
