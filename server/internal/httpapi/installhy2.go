package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
	"nodeops/server/internal/ippurity"
	"nodeops/server/internal/sshclient"
)

// 一键安装 hysteria2-installer 并自动接入控制台（2026-10-15）。
//
// 原来把一台新 VPS 变成面板里的节点要：SSH 上去跑安装脚本、回答一串交互问题、
// 把 portal-access.json 复制出来、回面板粘贴进「接入节点」。这里合成一步：
//
//   1. 预检：主机有 SSH 凭据且开启写操作授权；面板里该主机还没有节点；
//      域名解析到这台主机（否则 Let's Encrypt 申请证书必然失败，白等几分钟）。
//   2. SSH 执行官方安装脚本的免交互模式：Agent 模式（HY2_NODE_MODE=2）+
//      Let's Encrypt 证书（HY2_CERT_TYPE=3）。
//   3. 读 /etc/hysteria/portal-access.json，**走与「接入节点」完全相同的处理器**
//      完成预检、创建与绑定 —— 校验、加密存储、审计一处都不重写。
//      密钥只在服务端流转，不经过浏览器。
//
// 安装要几分钟，所以是后台任务：POST 立即返回任务号，前端轮询进度。
// 任务只存在内存里 —— 面板重启会丢失进度，但节点侧的安装不受影响，
// 之后仍可用「接入节点」手工补接。

// hy2InstallScriptURL 读取环境变量中的 Hysteria2 节点安装脚本地址。
// 开源版不硬编码特定个人仓库，部署时按需通过 HY2_INSTALL_SCRIPT_URL 配置可信 HTTPS 地址。
func hy2InstallScriptURL() (string, error) {
	raw := strings.TrimSpace(os.Getenv("HY2_INSTALL_SCRIPT_URL"))
	if raw == "" {
		return "", errors.New("未配置节点安装脚本地址，请设置环境变量 HY2_INSTALL_SCRIPT_URL（HTTPS 地址）")
	}
	if strings.ContainsAny(raw, " \t\r\n'\"") || controlChars.MatchString(raw) {
		return "", errors.New("环境变量 HY2_INSTALL_SCRIPT_URL 包含非法空白或控制字符")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return "", errors.New("环境变量 HY2_INSTALL_SCRIPT_URL 必须为不含凭据的合法 HTTPS 脚本地址")
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if !ippurity.IsPublicIP(host) {
			return "", errors.New("环境变量 HY2_INSTALL_SCRIPT_URL 不能指向内网或回环地址")
		}
	} else if host == "localhost" || !strings.Contains(host, ".") {
		return "", errors.New("环境变量 HY2_INSTALL_SCRIPT_URL 必须使用公网可访问域名")
	}
	return raw, nil
}

type installJob struct {
	Servers   []string   `json:"server_ids,omitempty"`
	ID        string     `json:"id"`
	ServerID  string     `json:"server_id"`
	Status    string     `json:"status"` // running / success / failed
	Step      string     `json:"step"`
	Log       []string   `json:"log"`
	Error     string     `json:"error,omitempty"`
	NodeID    string     `json:"node_id,omitempty"`
	NodeName  string     `json:"node_name,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// Access 是新节点的后台地址与登录账号，只在内存里给安装窗口展示一次，从不写入数据库（见 persistLocked）。
	Access *installAccess `json:"access,omitempty"`
	// cleanCount / cleanError 记录已脱敏进度，避免每次轮询或清理都对历史日志重复执行全量正则匹配。
	cleanCount int
	cleanError string
}

type installAccess struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type installJobStore struct {
	db    *pgxpool.Pool
	owner string
	ready bool
	owned map[string]bool
	// unsaved 记录最近一次落库失败的本进程任务，由日志巡检（15 秒一次）重试。
	unsaved map[string]bool
	mu      sync.Mutex
	jobs    map[string]*installJob
	// 同一台主机同一时间只允许一个安装任务。
	byServer map[string]string
}

var installJobs = &installJobStore{jobs: map[string]*installJob{}, byServer: map[string]string{}}

func (st *installJobStore) update(id string, fn func(j *installJob)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if j, ok := st.jobs[id]; ok {
		fn(j)
		if err := st.persistLocked(j); err != nil {
			st.ready = false
			// 2026-11-03：最后一次（结束态）落库失败时，库里仍是 running + 主机互斥租约。
			// 这里原来只置 ready=false：内存里任务已结束、不会再写库，租约只有 persistLocked
			// 会删 ⇒ 这台主机的运维任务、安装、迁移、换 IP 一直被拒，直到进程重启。登记重试。
			if st.unsaved == nil {
				st.unsaved = map[string]bool{}
			}
			st.unsaved[id] = true
		} else {
			delete(st.unsaved, id)
		}
	}
}

// retryUnsavedLocked 重试落库失败的任务（调用方持有 st.mu）。
func (st *installJobStore) retryUnsavedLocked() {
	for id := range st.unsaved {
		j, ok := st.jobs[id]
		if !ok {
			delete(st.unsaved, id)
			continue
		}
		if err := st.persistLocked(j); err == nil {
			delete(st.unsaved, id)
		}
	}
}

// 已结束任务的内存保留策略（2026-10-23）：
//   - 结束超过 installJobRetention 的任务从内存淘汰（读取任务、新建任务、15 秒一次的
//     restoreLocked 都会顺带清理，restoreLocked 也不再把它们从库里捞回来）；
//     历史仍可从 GET /api/install-jobs 的库存列表查看（库里本就不含密码）。
//   - 新节点后台密码只给安装窗口看一次：任务结束后第一次被 GET 返回就从内存抹掉
//     （安装窗口看到非 running 状态后即停止轮询，正好拿到这最后一份）；
//     没人来取的，结束 installPasswordTTL 后也抹掉。
const (
	installJobRetention = time.Hour
	installPasswordTTL  = 30 * time.Minute
)

// appendLogs 批量脱敏并一次落库多行任务日志，避免安装脚本输出数十行时循环持锁写库几十次。
func (st *installJobStore) appendLogs(id string, rawLines []string) {
	if len(rawLines) == 0 {
		return
	}
	cleaned := make([]string, 0, len(rawLines))
	for _, raw := range rawLines {
		for _, part := range strings.Split(raw, "\n") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			cleaned = append(cleaned, maskHostJobOutput(maskInstallSecrets(ansiEscape.ReplaceAllString(part, ""))))
		}
	}
	if len(cleaned) == 0 {
		return
	}
	st.update(id, func(j *installJob) {
		prevLen := len(j.Log)
		j.Log = append(j.Log, cleaned...)
		if j.cleanCount == prevLen {
			j.cleanCount = len(j.Log)
		}
	})
}

// evictLocked 淘汰过期的已结束任务、抹掉过期的一次性密码。调用方持有 st.mu。
func (st *installJobStore) evictLocked(now time.Time) {
	for id, j := range st.jobs {
		if j.cleanCount > len(j.Log) {
			j.cleanCount = 0
		}
		for i := j.cleanCount; i < len(j.Log); i++ {
			j.Log[i] = maskHostJobOutput(j.Log[i])
		}
		j.cleanCount = len(j.Log)
		if j.Error != j.cleanError {
			j.Error = maskHostJobOutput(j.Error)
			j.cleanError = j.Error
		}
		if j.EndedAt == nil || j.Status == "running" || j.Status == "interrupted" {
			continue
		}
		age := now.Sub(*j.EndedAt)
		if age > installJobRetention {
			delete(st.jobs, id)
			delete(st.owned, id)
			continue
		}
		if age > installPasswordTTL && j.Access != nil {
			j.Access.Password = ""
		}
	}
}

// snapshotForClient 是 GET /api/install-jobs/{id} 用的读取：先清理过期任务；
// 任务已结束时，密码随这一次响应带出后立刻从内存抹掉。
func (st *installJobStore) snapshotForClient(id string, now time.Time) (installJob, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.evictLocked(now)
	j, ok := st.jobs[id]
	if !ok {
		return installJob{}, false
	}
	cp := *j
	cp.Log = append([]string(nil), j.Log...)
	if j.Access != nil {
		acc := *j.Access
		cp.Access = &acc
		if j.Status != "running" && j.EndedAt != nil {
			j.Access.Password = ""
		}
	}
	return cp, true
}

var installDomainPattern = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

type installHy2Request struct {
	Domain  string `json:"domain"`
	Email   string `json:"email"`
	Port    int    `json:"port"`
	Name    string `json:"name"`
	InStore *bool  `json:"in_store"`
	// AutoDNS 为真时先用 Cloudflare 把域名的 A 记录指向这台主机（不走代理）。
	AutoDNS bool `json:"auto_dns"`
}

// POST /api/servers/{id}/install-hy2
func (s *Server) startInstallHy2(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	serverID := r.PathValue("id")
	if norm, ok := canonicalUUID(serverID); ok {
		serverID = norm
	}
	var in installHy2Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	in.Domain = strings.ToLower(strings.TrimSpace(in.Domain))
	in.Email = strings.TrimSpace(in.Email)
	in.Name = strings.TrimSpace(in.Name)
	if !installDomainPattern.MatchString(in.Domain) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请填写已解析到这台主机的域名（申请 Let's Encrypt 证书需要）"})
		return
	}
	// 邮箱必填：安装脚本申请 Let's Encrypt 证书时必须有邮箱，没给就会停在交互输入上然后失败。
	if in.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请填写证书通知邮箱（申请 Let's Encrypt 证书必须）"})
		return
	}
	if !validCertEmail(in.Email) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "邮箱格式不正确"})
		return
	}
	if in.Port != 0 && (in.Port < 1 || in.Port > 65535) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "端口需为 1-65535，留空则自动选取"})
		return
	}
	if _, err := hy2InstallScriptURL(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var host, serverName string
	var allowWrite, hasCred bool
	var existingNodes int
	err := s.db.QueryRow(r.Context(), `
SELECT s.host, s.name, s.allow_write_ops,
       EXISTS (SELECT 1 FROM server_credentials c WHERE c.server_id = s.id),
       (SELECT count(*) FROM hysteria_nodes n WHERE n.server_id = s.id AND n.deleted_at IS NULL)
FROM servers s WHERE s.id = $1::uuid AND s.deleted_at IS NULL`, serverID).Scan(&host, &serverName, &allowWrite, &hasCred, &existingNodes)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	switch {
	case !hasCred:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该主机还没有 SSH 凭据，先在「编辑主机」里上传私钥或密码"})
		return
	case !allowWrite:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该主机未开启写操作授权，先在「编辑主机」里开启"})
		return
	case existingNodes > 0:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "这台主机在面板里已经有节点了；如需重装请先删除该节点"})
		return
	}
	if in.Name == "" {
		in.Name = serverName
	}
	// 域名已被别的主机上的在用节点占用：自动 DNS 会把那个节点的域名改指到这台机器，
	// 直接切断它的全部客户。必须在任何 DNS 改动之前拦下（同一台主机上的节点不算冲突）。
	var domainTaken bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM hysteria_nodes WHERE deleted_at IS NULL AND lower(rtrim(domain,'.')) = lower(rtrim($1,'.')) AND server_id <> $2::uuid)`, in.Domain, serverID).Scan(&domainTaken); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if domainTaken {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该域名已被其他主机上的在用节点使用；请换一个域名，或用「迁移节点」把那个节点搬过来"})
		return
	}

	installJobs.mu.Lock()
	installJobs.evictLocked(time.Now())
	if running, ok := installJobs.byServer[serverID]; ok {
		installJobs.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "这台主机已有安装任务在进行", "job_id": running})
		return
	}
	idBytes := make([]byte, 8)
	_, _ = rand.Read(idBytes)
	job := &installJob{ID: hex.EncodeToString(idBytes), ServerID: serverID, Status: "running", Step: "准备中", StartedAt: time.Now()}
	if err := installJobs.createLocked(r.Context(), job, []string{serverID}); err != nil {
		installJobs.mu.Unlock()
		s.writeHostJobCreateError(w, r, err)
		return
	}
	installJobs.jobs[job.ID] = job
	installJobs.byServer[serverID] = job.ID
	installJobs.mu.Unlock()

	s.audit(r.Context(), r, "install_hy2", "server", serverID, map[string]any{"domain": in.Domain, "port": in.Port})

	// 任务比请求活得久：用不随请求取消的 context，但保留其中的登录身份，
	// 后面复用「接入节点」处理器时它们需要同一个操作者身份与审计归属。
	base := context.WithoutCancel(r.Context())
	remoteAddr, userAgent := r.RemoteAddr, r.UserAgent()
	go s.runInstallHy2(base, job.ID, serverID, host, in, remoteAddr, userAgent)

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID})
}

// GET /api/install-jobs/{id}
func (s *Server) getInstallJob(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	job, ok := installJobs.snapshotForClient(r.PathValue("id"), time.Now())
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "任务不存在或已超过历史保留范围"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) runInstallHy2(ctx context.Context, jobID, serverID, host string, in installHy2Request, remoteAddr, userAgent string) {
	// 2026-11-06：本任务自己持有这台主机的租约，接入节点时内部调用的 bindNodeAPI 会被 nodeMaintenance
	// 当成「主机正在安装」拒绝（409）—— 2026-11-03 给 bindNodeAPI 加维护检查后，每一次一键安装都在最后一步
	// 失败：节点已入库却没绑网页凭据，重试又因「主机上已有节点」被拒。与 runMigrateNode 同样豁免自己的内部调用。
	ctx = context.WithValue(ctx, migrationRequestKey{}, true)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	logf := func(format string, a ...any) {
		installJobs.appendLogs(jobID, []string{fmt.Sprintf(format, a...)})
	}
	step := func(name string) {
		installJobs.update(jobID, func(j *installJob) { j.Step = name })
		logf("==> %s", name)
	}
	finish := func(err error, nodeID, nodeName string) {
		now := time.Now()
		installJobs.update(jobID, func(j *installJob) {
			j.EndedAt = &now
			if err != nil {
				j.Status, j.Error = "failed", maskHostJobOutput(err.Error())
			} else {
				j.Status, j.Step, j.NodeID, j.NodeName = "success", "完成", nodeID, nodeName
			}
		})
		installJobs.mu.Lock()
		delete(installJobs.byServer, serverID)
		installJobs.mu.Unlock()
		if err != nil {
			s.logger.Warn("install hy2 failed", "server", serverID, "error", maskHostJobOutput(err.Error()))
		}
	}
	// 捕获后台安装协程异常，防止 panic 永久锁死主机安装租约。
	defer func() {
		if rec := recover(); rec != nil {
			if s.logger != nil {
				s.logger.Error("install hy2 panic", "server", serverID, "panic", rec, "stack", string(debug.Stack()))
			}
			finish(fmt.Errorf("一键安装发生内部异常: %v", rec), "", "")
		}
	}()
	// interrupt 用于「远端可能还在跑」的结局：记 interrupted、**保留**主机租约与 byServer 互斥，
	// 交给已有的「确认远端已停止后解除互斥」流程（resolveInstallJob）。
	interrupt := func(reason string) {
		installJobs.update(jobID, func(j *installJob) {
			j.Status, j.Step, j.Error = "interrupted", "远端可能仍在运行，需要核验", reason
		})
		s.logger.Warn("install hy2 interrupted", "server", serverID, "reason", reason)
	}

	// 入口预检之后可能等待换 IP 的行锁；持久化租约拿到后以当前地址为准。
	if err := s.db.QueryRow(ctx, `SELECT host FROM servers WHERE id=$1::uuid AND deleted_at IS NULL`, serverID).Scan(&host); err != nil {
		finish(errors.New("读取安装主机当前地址失败"), "", "")
		return
	}
	if in.AutoDNS {
		step("配置 Cloudflare DNS")
		res, err := s.cloudflareClient().UpsertARecords(ctx, in.Domain, host)
		if err != nil {
			finish(fmt.Errorf("自动配置 DNS 失败：%w", err), "", "")
			return
		}
		logf("A 记录 %s → %s：%s", in.Domain, host, res.Action)
		if warn := res.Warning(); warn != "" {
			logf("注意：%s", warn)
		}
	}
	step("检查域名解析")
	// 刚改的记录要等递归 DNS 刷新：自动配置时最多等 2 分钟。
	var dnsErr error
	for attempt := 0; attempt < 12; attempt++ {
		if dnsErr = checkDomainPointsTo(ctx, in.Domain, host); dnsErr == nil || !in.AutoDNS {
			break
		}
		logf("解析尚未生效，10 秒后重试（%d/12）", attempt+1)
		select {
		case <-ctx.Done():
			finish(ctx.Err(), "", "")
			return
		case <-time.After(10 * time.Second):
		}
	}
	if dnsErr != nil {
		finish(dnsErr, "", "")
		return
	}
	logf("%s 已解析到 %s", in.Domain, host)

	step("SSH 连接主机")
	client, user, err := s.dialServer(ctx, serverID)
	if err != nil {
		finish(err, "", "")
		return
	}
	defer client.Close()
	if want := forcedLoginUser(ctx, client); want != "" {
		// 云镜像禁止 root 执行命令：自动改用它要求的用户（同一把私钥）再连。
		client.Close()
		if err := s.switchSSHUser(ctx, serverID, want); err != nil {
			finish(errors.New("切换 SSH 用户失败"), "", "")
			return
		}
		logf("主机要求用 %s 登录，已自动把 SSH 用户从 %s 改为 %s", want, user, want)
		if client, user, err = s.dialServer(ctx, serverID); err != nil {
			finish(fmt.Errorf("改用 %s 后连接失败：%w", want, err), "", "")
			return
		}
		defer client.Close()
	}
	sudo := "sudo -n "
	if user == "root" {
		sudo = ""
	} else {
		// 非 root 用户（AWS 的 admin、Ubuntu 的 ubuntu 等）必须有免密 sudo：
		// 安装脚本要写 /etc、装 systemd 服务。先探一次，别等装到一半才报
		// 「a password is required」。
		_, _, code, err := sshclient.Run(ctx, client, "sudo -n true")
		if err != nil || code != 0 {
			finish(fmt.Errorf("SSH 用户 %s 不是 root，且没有免密 sudo。请改用 root 登录，或在主机上执行 echo '%s ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/90-%s 后重试", user, user, user), "", "")
			return
		}
		logf("SSH 用户 %s 具备免密 sudo", user)
	}

	// 已经装好（比如上次装完但接入失败）：不重装，直接读接入信息去接入。
	if out, _, code, err := sshclient.Run(ctx, client, sudo+"sh -c 'test -s /etc/hysteria/portal-access.json && systemctl is-active --quiet hysteria-server && echo installed'"); err == nil && code == 0 && strings.Contains(out, "installed") {
		logf("检测到这台主机已装好 HY2，跳过安装，直接接入")
	} else {
		step("执行安装脚本（约 2-5 分钟）")
		if !s.runInstallScript(ctx, client, sudo, in, logf, finish, interrupt) {
			return
		}
	}

	step("读取节点接入信息")
	access, _, exit, err := sshclient.Run(ctx, client, sudo+"cat /etc/hysteria/portal-access.json")
	access = strings.TrimSpace(access)
	if err != nil || exit != 0 || !strings.HasPrefix(access, "{") {
		finish(errors.New("安装完成但读不到 /etc/hysteria/portal-access.json；可在节点上执行 bash install.sh info 后用「接入节点」手工接入"), "", "")
		return
	}

	var acc installAccess
	if json.Unmarshal([]byte(access), &acc) == nil && acc.URL != "" {
		installJobs.update(jobID, func(j *installJob) { j.Access = &acc })
	}

	step("接入控制台")
	// 安装脚本刚结束时，节点门户服务可能还在启动、证书还在签发，
	// 立刻连会拿到 connection refused / 超时 / TLS 错误。这类临时错误等一等再试（最多约 2 分钟），
	// 只有节点还没创建（nodeID 为空）时才重试，避免重复建节点。
	var nodeID, nodeName string
	for attempt := 1; ; attempt++ {
		nodeID, nodeName, err = s.importInstalledNode(ctx, serverID, in, access, remoteAddr, userAgent)
		if err == nil || nodeID != "" || attempt >= installJoinAttempts || !transientJoinError(err) {
			break
		}
		logf("节点服务还没就绪（%s），%d 秒后重试（%d/%d）", shortJoinError(err), int(installJoinWait.Seconds()), attempt, installJoinAttempts-1)
		select {
		case <-ctx.Done():
			finish(ctx.Err(), "", "")
			return
		case <-time.After(installJoinWait):
		}
	}
	if err != nil {
		finish(fmt.Errorf("节点已装好，但自动接入失败：%w（可用「接入节点」粘贴 portal-access.json 手工接入）", err), "", "")
		return
	}
	logf("已接入：%s", nodeName)
	finish(nil, nodeID, nodeName)
}

// certEmailPattern 是证书通知邮箱的白名单：只放行常规邮箱字符，制表符、引号、空白与
// shell 元字符一律拒绝（原来的黑名单漏了 \t 等字符，而邮箱会拼进远端 bash 命令）。
var certEmailPattern = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

func validCertEmail(email string) bool {
	if len(email) > 120 || !certEmailPattern.MatchString(email) {
		return false
	}
	local, domain, _ := strings.Cut(email, "@")
	if len(local) > 64 || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
}

// installScriptCommand 拼出在远端执行安装脚本的完整命令。白名单校验之外再加一层防御：
// 每个环境变量值都单引号转义，整条内层命令再作为单个参数交给 bash -c。
func installScriptCommand(sudo string, env [][2]string) string {
	scriptURL, err := hy2InstallScriptURL()
	if err != nil {
		scriptURL = "https://example.invalid/install.sh"
	}
	parts := make([]string, 0, len(env))
	for _, kv := range env {
		parts = append(parts, kv[0]+"="+shellQuote(kv[1]))
	}
	inner := fmt.Sprintf(
		"set -o pipefail; curl -fsSL %s -o /tmp/hy2-install.sh && %senv %s bash /tmp/hy2-install.sh install </dev/null 2>&1 | tail -n 40",
		shellQuote(scriptURL), sudo, strings.Join(parts, " "))
	return "bash -c " + shellQuote(inner)
}

// 安装脚本带颜色输出（ESC[0;32m 之类），网页日志里会显示成乱码，入日志前去掉。
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// runInstallScript 在主机上执行官方安装脚本；失败时已调用 finish，返回 false。
func (s *Server) runInstallScript(ctx context.Context, client *ssh.Client, sudo string, in installHy2Request, logf func(string, ...any), finish func(error, string, string), interrupt func(string)) bool {
	if _, err := hy2InstallScriptURL(); err != nil {
		finish(err, "", "")
		return false
	}
	// HOME 钉成 /root：安装脚本把 acme.sh 装在 $HOME/.acme.sh，续期任务也依赖它；
	// 部分 sudo 配置会保留调用者的 HOME（/home/admin），证书就会落到别处。
	env := [][2]string{{"HOME", "/root"}, {"HY2_NODE_MODE", "2"}, {"HY2_CERT_TYPE", "3"}, {"HY2_DOMAIN", in.Domain}}
	if in.Email != "" {
		env = append(env, [2]string{"HY2_EMAIL", in.Email})
	}
	if in.Port != 0 {
		env = append(env, [2]string{"HY2_PORT", fmt.Sprint(in.Port)})
	}
	stdout, stderr, exit, err := sshclient.Run(ctx, client, installScriptCommand(sudo, env))
	if cleaned := strings.TrimSpace(sshclient.CleanCommandOutput(stdout)); cleaned != "" {
		logf("%s", cleaned)
	}
	// 2026-11-06：任何没拿到远端退出码的错误都按「远端可能仍在跑」处理（sshclient.Run 只在拿到退出码时 err 为 nil）。
	// 原来只认超时 / 取消：连接被重置、EOF（*ssh.ExitMissingError）走 failed 释放租约，而无 pty 的会话断开后
	// sshd 不会给子进程发 SIGHUP，安装脚本照样在写 /etc/hysteria。
	if err != nil {
		// 2026-11-04：超时 / 取消时不能当成普通失败。SSH 端只杀得到最外层 shell：sudo、bash install.sh、
		// curl 会被托孤继续跑。按 failed 收尾会立刻释放主机租约，第二次安装或排队的运维任务就会
		// 和还在写 /etc/hysteria 的第一次安装撞在一起。
		interrupt("安装脚本没有正常结束（" + shortErr(err.Error()) + "）；远端安装进程可能仍在运行，确认已停止后再解除互斥")
		return false
	}
	if err != nil || exit != 0 {
		msg := strings.TrimSpace(sshclient.CleanCommandOutput(stderr))
		if err != nil {
			msg = err.Error()
		}
		finish(fmt.Errorf("安装脚本失败（退出码 %d）：%s", exit, firstNonEmpty(msg, "详见日志")), "", "")
		return false
	}
	return true
}

// 通信密钥只用于商城对接，日志里打码；后台地址与账号密码单独放在 job.Access 里给安装窗口展示一次。
var (
	installSecretToken = regexp.MustCompile(`hy2_sec_[A-Za-z0-9]+`)
)

func maskInstallSecrets(line string) string {
	line = installSecretToken.ReplaceAllString(line, "hy2_sec_********")
	return line
}

// hostJobSafePhrases 是面板自己写出的、含「passwd / token」字样但不含任何凭据的提示。
//
// 2026-11-04：关键字屏蔽原来对它们一视同仁：没有免密 sudo 时给出的修复命令（含 NOPASSWD）、
// Cloudflare「Token 无效」「未配置 Cloudflare API Token」都被整行换成「含访问凭据的输出已省略」——
// 操作者看到的最终错误恰恰是那句没用的占位，真正的原因和修法全没了。
// 判断前先把这些固定措辞去掉，剩余部分仍照常检查（远端输出拼进来的内容照样会被屏蔽）。
var hostJobSafePhrases = []string{"nopasswd:all", "nopasswd", "token无效", "cloudflareapitoken"}

// 后台密码只经 Access 短暂展示，日志和错误字段不得留下另一份。
func maskHostJobOutput(text string) string {
	lines := strings.Split(ansiEscape.ReplaceAllString(text, ""), "\n")
	for i, line := range lines {
		compact := strings.ToLower(strings.Join(strings.Fields(line), ""))
		for _, phrase := range hostJobSafePhrases {
			compact = strings.ReplaceAll(compact, phrase, "")
		}
		for _, key := range []string{"password", "passwd", "api_key", "apikey", "token", "hysteria2://", "hy2://", "vless://", "用户名", "密码", "订阅链接", "privatekey", "private_key", "私钥"} {
			if strings.Contains(compact, key) {
				lines[i] = "（含访问凭据的输出已省略）"
				break
			}
		}
		lines[i] = maskInstallSecrets(lines[i])
	}
	return strings.Join(lines, "\n")
}

const installJoinAttempts = 13

var installJoinWait = 10 * time.Second

// transientJoinError 判断接入失败是不是「节点服务还没起来」这类等一等就会好的网络错误。
//
// ⚠️ 2026-10-25 五轮排查第三轮：502/503/504 按**整词**匹配。
// 原来用裸子串，`dial tcp 10.5.3.5:5040` 这类端口号、以及任何含 "502"
// 的字节数/路径都会命中 ⇒ 明明是一次确定性失败（例如密钥不对），
// 却白等 2 分钟重试 12 次，最后还报「节点服务没就绪」，把原因掩盖掉。
func transientJoinError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, k := range []string{"connection refused", "connection reset", "i/o timeout", "timeout", "no such host", "eof", "tls", "certificate", "no route to host", "network is unreachable"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	for _, code := range []string{"502", "503", "504"} {
		if hasStandaloneToken(msg, code) {
			return true
		}
	}
	return false
}

func shortJoinError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return "端口还没开始监听"
	case strings.Contains(msg, "timeout"):
		return "连接超时"
	case strings.Contains(msg, "no such host"):
		return "域名还没解析"
	case strings.Contains(msg, "tls") || strings.Contains(msg, "certificate"):
		return "证书还没就绪"
	}
	return "网络暂时不通"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// checkDomainPointsTo 确认域名的 A/AAAA 记录里有这台主机的地址。
func checkDomainPointsTo(ctx context.Context, domain, host string) error {
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 直接问公共 DNS，绕开本机解析缓存：刚创建的记录若之前被查过一次，
	// 系统解析器会把「不存在」负缓存几十分钟，导致明明已生效却一直判失败。
	var addrs []string
	var err error
	for _, ns := range []string{"1.1.1.1:53", "8.8.8.8:53", "223.5.5.5:53"} {
		ns := ns
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", ns)
		}}
		got, lerr := r.LookupHost(lookupCtx, domain)
		if lerr == nil {
			for _, a := range got {
				if a == host {
					return nil
				}
			}
			addrs, err = got, nil
		} else if len(addrs) == 0 {
			err = lerr
		}
	}
	if len(addrs) == 0 {
		addrs, err = net.DefaultResolver.LookupHost(lookupCtx, domain)
	}
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("域名 %s 解析失败，先把它的 A 记录指向 %s", domain, host)
	}
	for _, a := range addrs {
		if a == host {
			return nil
		}
	}
	return fmt.Errorf("域名 %s 解析到 %s，不是这台主机（%s）；改好 DNS 再装（若开了 CDN 代理请先关掉）", domain, strings.Join(addrs, ", "), host)
}

// dialServer 按主机 id 建立 SSH 连接（与 hostSSH 同样要求已登记指纹）。
func (s *Server) dialServer(ctx context.Context, serverID string) (*ssh.Client, string, error) {
	var host, user, pin, kind string
	var port int
	var encrypted []byte
	err := s.db.QueryRow(ctx, `SELECT s.host,s.ssh_port,s.ssh_user,COALESCE(s.host_fingerprint,''),COALESCE(sc.auth_type,''),sc.ciphertext FROM servers s LEFT JOIN LATERAL (SELECT auth_type,ciphertext FROM server_credentials WHERE server_id=s.id ORDER BY created_at DESC LIMIT 1) sc ON true WHERE s.id=$1::uuid AND s.deleted_at IS NULL`, serverID).Scan(&host, &port, &user, &pin, &kind, &encrypted)
	if err != nil {
		return nil, "", errors.New("读取主机信息失败")
	}
	if len(encrypted) == 0 || s.box == nil {
		return nil, "", errors.New("主机未配置可用 SSH 凭据")
	}
	if pin == "" {
		return nil, "", errors.New("主机尚未登记 SSH 指纹，请先在主机页完成一次连接测试")
	}
	secret, err := s.box.Open(encrypted)
	if err != nil {
		return nil, "", errors.New("SSH 凭据解密失败")
	}
	defer clear(secret)
	if kind == "" {
		kind = "ssh_key"
	}
	client, _, err := sshclient.Dial(ctx, host, port, user, sshclient.Credential{Type: kind, Secret: secret}, pin)
	if err != nil {
		return nil, "", fmt.Errorf("SSH 连接失败: %w", err)
	}
	return client, user, nil
}

// importInstalledNode 复用「接入节点」的两个处理器（createNode + bindNodeAPI），
// 让自动接入与手工接入走完全相同的校验、加密存储与审计路径。
func (s *Server) importInstalledNode(ctx context.Context, serverID string, in installHy2Request, accessJSON, remoteAddr, userAgent string) (string, string, error) {
	var parsed struct {
		URL    string `json:"url"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal([]byte(accessJSON), &parsed); err != nil || parsed.URL == "" || parsed.APIKey == "" {
		return "", "", errors.New("portal-access.json 内容不完整")
	}
	body := map[string]any{"server_id": serverID, "name": in.Name, "api_url": parsed.URL, "api_key": parsed.APIKey}
	if in.InStore != nil {
		body["in_store"] = *in.InStore
	}
	rec, err := s.callInternal(ctx, http.MethodPost, "/api/nodes", nil, body, s.createNode, remoteAddr, userAgent)
	if err != nil {
		return "", "", err
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec, &created); err != nil {
		return "", "", fmt.Errorf("创建节点响应无法解析：%w", err)
	}
	if created.ID == "" {
		return "", "", errors.New("创建节点后未返回 id")
	}
	// 绑定网页会话凭据（username / password），解锁升级、出口设置等功能。
	if _, err := s.callInternal(ctx, http.MethodPost, "/api/nodes/"+created.ID+"/api", map[string]string{"id": created.ID},
		map[string]any{"access_json": accessJSON}, s.bindNodeAPI, remoteAddr, userAgent); err != nil {
		return created.ID, in.Name, fmt.Errorf("节点已创建，但绑定门户凭据失败：%w", err)
	}
	name := created.Name
	if name == "" {
		name = in.Name
	}
	return created.ID, name, nil
}

func (s *Server) callInternal(ctx context.Context, method, path string, pathValues map[string]string, body any, h http.HandlerFunc, remoteAddr, userAgent string) ([]byte, error) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	req.Header.Set("User-Agent", userAgent)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return nil, errors.New(firstNonEmpty(e.Error, fmt.Sprintf("HTTP %d", rec.Code)))
	}
	return rec.Body.Bytes(), nil
}
