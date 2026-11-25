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
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
	"nodeops/server/internal/sshclient"
)

// 节点一键迁移到新 VPS（2026-10-15）。
//
// 场景：节点被墙、IP 太脏、主机到期不续 —— 要换一台机器，但客户手里的订阅不能失效。
// 节点的全部状态都在 /etc/hysteria（用户、门户凭据、API Key、订阅端口、Reality 密钥），
// 订阅链接用的是**域名 + 订阅端口**。所以迁移 = 把 /etc/hysteria 原样搬到新机器、
// 域名改指新 IP、用安装脚本的「沿用现有门户」模式重装，客户无感。
//
// 步骤：
//   1. 预检：节点有域名；旧主机能 SSH；新主机有凭据 + 写操作授权 + 尚无节点。
//   2. 从旧主机打包 /etc/hysteria（去掉 .bak 与缓存）。旧主机被墙只是国内连不上，
//      面板（海外）通常仍能 SSH 上去。
//   3. 解包到新主机的 /etc/hysteria。
//   4. 域名改指新 IP（Cloudflare 自动，或等待你手工改好，最多等 10 分钟）。
//   5. 新主机执行安装脚本：HY2_PRESERVE_PORTAL=1 沿用门户，HY2_PORT 沿用旧监听端口，
//      证书按域名重新申请（必须在 DNS 切过来之后）。
//   6. 面板把节点挂到新主机；旧机器上有 Reality 的，在新机器装好 Xray（沿用原密钥）。
//   7. 经节点 API 复核：门户能连通、用户数一致。
// 旧主机上的服务不动 —— 确认新节点一切正常后，再自己去停旧机或用「卸载」预设。

type migrateRequest struct {
	TargetServerID string `json:"target_server_id"`
	AutoDNS        bool   `json:"auto_dns"`
	Email          string `json:"email"`
}

// POST /api/nodes/{id}/migrate
func (s *Server) startMigrateNode(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	nodeID := r.PathValue("id")
	var in migrateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in); err != nil || in.TargetServerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择目标主机"})
		return
	}
	// 邮箱必填：新主机要重新申请 Let's Encrypt 证书，安装脚本没有邮箱会失败。
	if strings.TrimSpace(in.Email) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请填写证书通知邮箱（新主机申请 Let's Encrypt 证书必须）"})
		return
	}
	in.Email = strings.TrimSpace(in.Email)
	if !validCertEmail(in.Email) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "邮箱格式不正确"})
		return
	}

	var nodeName, domain, oldServerID, oldHost string
	var listenPort int
	err := s.db.QueryRow(r.Context(), `
SELECT n.name, COALESCE(n.domain,''), n.listen_port, n.server_id::text, s.host
FROM hysteria_nodes n JOIN servers s ON s.id = n.server_id
WHERE n.id = $1::uuid AND n.deleted_at IS NULL`, nodeID).Scan(&nodeName, &domain, &listenPort, &oldServerID, &oldHost)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if !installDomainPattern.MatchString(domain) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "节点没有域名（只有 IP），迁移后客户订阅里的地址必然变化，无法无感迁移"})
		return
	}
	if in.TargetServerID == oldServerID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "目标主机就是节点当前所在的主机"})
		return
	}
	var newHost string
	var allowWrite, hasCred bool
	var existing int
	err = s.db.QueryRow(r.Context(), `
SELECT s.host, s.allow_write_ops,
       EXISTS (SELECT 1 FROM server_credentials c WHERE c.server_id = s.id),
       (SELECT count(*) FROM hysteria_nodes n WHERE n.server_id = s.id AND n.deleted_at IS NULL)
FROM servers s WHERE s.id = $1::uuid AND s.deleted_at IS NULL`, in.TargetServerID).Scan(&newHost, &allowWrite, &hasCred, &existing)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "目标主机不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	switch {
	case !hasCred:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "目标主机还没有 SSH 凭据"})
		return
	case !allowWrite:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "目标主机未开启写操作授权"})
		return
	case existing > 0:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "目标主机上已有节点，请选一台空机器"})
		return
	}
	// 迁移只允许切换本节点的域名；同名的其他在用节点不能随迁移一起被切流。
	var domainTaken bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM hysteria_nodes WHERE deleted_at IS NULL AND lower(rtrim(domain,'.'))=lower(rtrim($1,'.')) AND id<>$2::uuid)`, domain, nodeID).Scan(&domainTaken); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if domainTaken {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该域名仍被其他在用节点使用，不能切换 DNS"})
		return
	}
	var oldHasCred bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM server_credentials WHERE server_id=$1::uuid)`, oldServerID).Scan(&oldHasCred); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if !oldHasCred {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "旧主机没有 SSH 凭据，无法读取节点数据；先在 VPS 主机页给旧主机上传凭据"})
		return
	}

	installJobs.mu.Lock()
	installJobs.evictLocked(time.Now())
	for _, busyID := range []string{in.TargetServerID, oldServerID} {
		if running, ok := installJobs.byServer[busyID]; ok {
			installJobs.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{"error": "相关主机已有安装/迁移任务在进行", "job_id": running})
			return
		}
	}
	idBytes := make([]byte, 8)
	_, _ = rand.Read(idBytes)
	job := &installJob{ID: hex.EncodeToString(idBytes), ServerID: in.TargetServerID, Status: "running", Step: "准备中", StartedAt: time.Now(), NodeID: nodeID, NodeName: nodeName}
	if err := installJobs.createLocked(r.Context(), job, []string{in.TargetServerID, oldServerID}); err != nil {
		installJobs.mu.Unlock()
		s.writeHostJobCreateError(w, r, err)
		return
	}
	installJobs.jobs[job.ID] = job
	installJobs.byServer[in.TargetServerID] = job.ID
	installJobs.byServer[oldServerID] = job.ID
	installJobs.mu.Unlock()

	s.audit(r.Context(), r, "migrate_node", "hysteria_node", nodeID, map[string]any{
		"from_server": oldServerID, "from_ip": oldHost, "to_server": in.TargetServerID, "to_ip": newHost, "domain": domain})
	go s.runMigrateNode(context.WithoutCancel(r.Context()), job.ID, nodeID, nodeName, domain, listenPort, oldServerID, in.TargetServerID, oldHost, newHost, in)
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID})
}

func (s *Server) runMigrateNode(ctx context.Context, jobID, nodeID, nodeName, domain string, listenPort int, oldServerID, newServerID, oldHost, newHost string, in migrateRequest) {
	ctx = context.WithValue(ctx, migrationRequestKey{}, true)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	logf := func(format string, a ...any) {
		// 与一键安装同样处理：去掉安装脚本的颜色转义，通信密钥打码。
		line := maskHostJobOutput(maskInstallSecrets(ansiEscape.ReplaceAllString(fmt.Sprintf(format, a...), "")))
		installJobs.update(jobID, func(j *installJob) { j.Log = append(j.Log, line) })
	}
	step := func(name string) {
		installJobs.update(jobID, func(j *installJob) { j.Step = name })
		logf("==> %s", name)
	}
	var rollback func()
	cutover, committed := false, false
	finish := func(err error) {
		if err != nil && rollback != nil {
			rollback()
			rollback = nil
		}
		now := time.Now()
		installJobs.update(jobID, func(j *installJob) {
			j.EndedAt = &now
			if err != nil {
				j.Status, j.Error = "failed", maskHostJobOutput(err.Error())
			} else {
				j.Status, j.Step = "success", "完成"
			}
		})
		installJobs.mu.Lock()
		delete(installJobs.byServer, newServerID)
		delete(installJobs.byServer, oldServerID)
		installJobs.mu.Unlock()
		if err != nil {
			s.logger.Warn("migrate node failed", "node", nodeID, "error", maskHostJobOutput(err.Error()))
		}
	}

	// 请求预检可能早于换 IP 事务提交；取得租约后重新读取目标地址。
	if err := s.db.QueryRow(ctx, `SELECT host FROM servers WHERE id=$1::uuid AND deleted_at IS NULL`, newServerID).Scan(&newHost); err != nil {
		finish(errors.New("读取迁移目标当前地址失败"))
		return
	}
	// 2026-11-03：旧主机地址同样要在租约后重读。它用于失败时的 DNS 回退；
	// 原来用的是加锁前读到的值，与在途的「旧主机换 IP」交错时会把域名指回一个已不用的 IP。
	if err := s.db.QueryRow(ctx, `SELECT host FROM servers WHERE id=$1::uuid AND deleted_at IS NULL`, oldServerID).Scan(&oldHost); err != nil {
		finish(errors.New("读取旧主机当前地址失败"))
		return
	}
	step("连接旧主机并打包节点数据")
	oldClient, oldUser, err := s.dialServer(ctx, oldServerID)
	if err != nil {
		finish(fmt.Errorf("旧主机：%w", err))
		return
	}
	defer oldClient.Close()
	oldSudo := sudoPrefix(oldUser)
	if oldSudo != "" {
		if _, _, code, err := sshclient.Run(ctx, oldClient, "sudo -n true"); err != nil || code != 0 {
			finish(fmt.Errorf("旧主机的 SSH 用户 %s 不是 root 且没有免密 sudo", oldUser))
			return
		}
	}
	tlsData, err := migrationCertificate(ctx, oldClient, oldSudo)
	if err != nil {
		finish(fmt.Errorf("迁移准备失败：%w", err))
		return
	}
	oldTarget, err := s.loadPortalTarget(ctx, nodeID)
	if err != nil {
		finish(err)
		return
	}
	oldMeta, err := fetchPortalMeta(ctx, oldTarget.BaseURL, oldTarget.APIKey)
	if err != nil {
		finish(fmt.Errorf("旧节点核验失败：%w", err))
		return
	}
	oldAPIURL := oldTarget.BaseURL
	u, _ := url.Parse(oldAPIURL)
	if tlsData.Insecure && !strings.EqualFold(u.Hostname(), domain) {
		finish(errors.New("自签证书且API使用IP地址，请先以节点域名重新绑定API并登记证书指纹，再迁移"))
		return
	}
	archive, err := runHostRead(ctx, oldClient, oldSudo+"tar czf - -C /etc --exclude='hysteria/*.bak*' --exclude='hysteria/__pycache__' --exclude='hysteria/meter-backup-*' --exclude='hysteria/core-backups' --exclude='hysteria/backups' hysteria", 32<<20)
	if err != nil || len(archive) < 512 {
		finish(errors.New("旧节点打包失败或超过32MiB安全上限"))
		return
	}

	hadXray := false
	if out, _, c, _ := sshclient.Run(ctx, oldClient, oldSudo+"test -f /etc/hysteria/xray.json && echo yes"); c == 0 && strings.TrimSpace(out) == "yes" {
		hadXray = true
	}
	logf("已打包 %.1f MB%s", float64(len(archive))/1024/1024, map[bool]string{true: "（含 Reality 配置）", false: ""}[hadXray])

	step("连接新主机并还原节点数据")
	newClient, newUser, err := s.dialServer(ctx, newServerID)
	if err != nil {
		finish(fmt.Errorf("新主机：%w", err))
		return
	}
	defer newClient.Close()
	newSudo := sudoPrefix(newUser)
	if newSudo != "" {
		if _, _, c, err := sshclient.Run(ctx, newClient, "sudo -n true"); err != nil || c != 0 {
			finish(fmt.Errorf("新主机的 SSH 用户 %s 不是 root 且没有免密 sudo", newUser))
			return
		}
	}
	if out, _, c, _ := sshclient.Run(ctx, newClient, newSudo+"test -e /etc/hysteria/config.yaml && echo exists"); c == 0 && strings.TrimSpace(out) == "exists" {
		finish(errors.New("新主机上已经装过 Hysteria（/etc/hysteria/config.yaml 存在），为免覆盖，请换一台干净的机器或先卸载"))
		return
	}
	if err := runWithStdin(ctx, newClient, newSudo+"mkdir -p /etc && "+newSudo+"tar xzf - -C /etc", []byte(archive)); err != nil {
		finish(fmt.Errorf("还原到新主机失败：%w", err))
		return
	}
	logf("已还原到新主机 /etc/hysteria")
	if err := transferMigrationTLS(ctx, newClient, newSudo, tlsData); err != nil {
		finish(fmt.Errorf("迁移证书传输失败：%w", err))
		return
	}
	rollback = func() {
		if committed {
			return
		}
		rollbackCtx, done := context.WithTimeout(context.Background(), 45*time.Second)
		defer done()
		// 2026-11-03：手动切 DNS 的模式下，回退原来只改库、不提 DNS ——
		// 操作者已把域名指到新主机，面板却回到旧主机开户 / 回收，新买家认证失败、回收漏掉实际在用的机器。
		if cutover && !in.AutoDNS {
			logf("⚠ 必须手工操作：请立即把 %s 的 DNS 改回旧主机 %s（节点归属已回到旧主机）", domain, oldHost)
		}
		if cutover && in.AutoDNS {
			if net.ParseIP(oldHost) == nil {
				logf("旧主机地址无效（%q），DNS未回退；请立即手工将 %s 恢复到旧节点 IP", oldHost, domain)
			} else if _, err := s.cloudflareClient().UpsertA(rollbackCtx, domain, oldHost); err != nil {
				logf("DNS回退失败：%v，请立即手工将 %s 恢复到旧节点 IP %s", err, domain, oldHost)
			} else {
				logf("DNS已回退旧节点 %s", oldHost)
			}
		}
		// ⚠️ 2026-10-31：回滚写库失败必须吼出来。
		// 迁移失败后这一步是把节点指回旧主机/旧 API 地址；写失败而没人知道，
		// 库里就留下「节点挂在新主机、但新主机上什么都没装」的状态 ——
		// 比迁移本身失败更难查。原来用 `_, _ =` 完全静默。
		// 2026-11-06：指回旧机器时一并清掉计量基线（中途若有一轮计量把基线换成了新机器，
		// 切回后会把旧机器几个月的累计一次算进本周期）。与计量互斥，避免在途的一轮把旧基线写回来。
		unlockMeter, lockErr := lockTrafficMeterCtx(rollbackCtx, nodeID)
		if lockErr != nil {
			logf("回滚时等待流量计量锁超时：%v", lockErr)
			unlockMeter = func() {}
		}
		_, err := s.db.Exec(rollbackCtx, `UPDATE hysteria_nodes SET server_id=$2::uuid,api_url=$3,`+trafficBaselineResetSQL+`,updated_at=now() WHERE id=$1::uuid`, nodeID, oldServerID, oldAPIURL)
		unlockMeter()
		if err != nil {
			logf("回滚节点归属失败：%v —— 节点仍指向新主机 %s，请手工改回旧主机并把 api_url 恢复为 %s", err, oldServerID, oldAPIURL)
		} else {
			invalidateHealthGateCache()
			InvalidateSubscriptionCache()
			logf("节点归属已回滚到旧主机")
		}
	}
	defer func() {
		if rollback != nil {
			rollback()
		}
	}()

	step("新主机执行安装脚本（沿用门户，约 2-5 分钟）")
	env := [][2]string{{"HOME", "/root"}, {"HY2_NODE_MODE", "2"}, {"HY2_CERT_TYPE", "2"},
		{"HY2_CUSTOM_CERT_FILE", "/etc/hysteria/cert/migration.crt"}, {"HY2_CUSTOM_KEY_FILE", "/etc/hysteria/cert/migration.key"},
		{"HY2_CUSTOM_INSECURE", fmt.Sprint(tlsData.Insecure)}, {"HY2_PRESERVE_PORTAL", "1"}, {"HY2_DOMAIN", domain}}
	if listenPort > 0 {
		env = append(env, [2]string{"HY2_PORT", fmt.Sprint(listenPort)})
	}
	if in.Email != "" {
		env = append(env, [2]string{"HY2_EMAIL", in.Email})
	}
	stdout, stderr, code, err := sshclient.Run(ctx, newClient, installScriptCommand(newSudo, env))
	if cleaned := strings.Split(sshclient.CleanCommandOutput(stdout), "\n"); len(cleaned) > 0 {
		batch := make([]string, 0, len(cleaned))
		for _, line := range cleaned {
			if line = strings.TrimSpace(line); line != "" {
				batch = append(batch, line)
			}
		}
		if len(batch) > 0 {
			installJobs.appendLogs(jobID, batch)
		}
	}
	// 2026-11-04：与一键安装同理，脚本超时 / 被取消时远端安装进程可能仍在运行。原来按 failed 结案：
	// 主机租约与内存互斥随即解除，下一次安装 / 迁移会与仍在跑的旧脚本同时改写 /etc/hysteria。
	// 现在先回退（此时尚未切 DNS，只把节点归属指回旧主机），再记 interrupted、保留互斥，
	// 等操作者确认远端已停止后在任务列表里结案。
	// 2026-11-06：任何没拿到远端退出码的错误（连接重置、EOF 也一样）都按「远端可能仍在跑」处理，见 runInstallHy2。
	if err != nil {
		if rollback != nil {
			rollback()
			rollback = nil
		}
		now := time.Now()
		installJobs.update(jobID, func(j *installJob) {
			j.EndedAt = &now
			j.Status, j.Step, j.Error = "interrupted", "远端可能仍在运行，需要核验", "新主机安装脚本没有正常结束（"+shortErr(err.Error())+"）；远端安装进程可能仍在运行，确认已停止后再解除互斥"
		})
		s.logger.Warn("migrate node interrupted", "node", nodeID, "target", newServerID)
		return
	}
	if err != nil || code != 0 {
		finish(fmt.Errorf("新主机安装失败（退出码 %d）：%s", code, firstNonEmpty(strings.TrimSpace(stderr), "详见日志")))
		return
	}

	if _, err := runHostRead(ctx, newClient, newSudo+"cp /etc/hysteria/config.migration-original.yaml /etc/hysteria/config.yaml && "+newSudo+"systemctl restart hysteria-server"); err != nil {
		finish(fmt.Errorf("恢复原节点配置失败：%w", err))
		return
	}
	readyCtx := context.WithValue(ctx, migrationIPKey{}, newHost)
	if hadXray {
		step("切流前安装并验证新主机Reality")
		webTarget, err := s.loadPortalWebTarget(ctx, nodeID)
		if err != nil {
			finish(err)
			return
		}
		if _, err := portalWebCall(readyCtx, webTarget, http.MethodPost, "install-xray", map[string]string{}); err != nil {
			finish(fmt.Errorf("新节点Reality准备失败：%w", err))
			return
		}
		if _, err := runHostRead(ctx, newClient, newSudo+"systemctl is-active --quiet xray"); err != nil {
			finish(errors.New("新主机Xray未运行，未切换DNS"))
			return
		}
	}
	step("直接核验新主机，尚未切换DNS")
	meta, err := fetchPortalMeta(readyCtx, oldTarget.BaseURL, oldTarget.APIKey)
	if err != nil || meta.Meta.PublicIP != newHost || meta.Meta.ListenPort != listenPort || meta.UsersCount != oldMeta.UsersCount {
		finish(errors.New("新主机身份、端口或用户数未通过核验，未切换DNS"))
		return
	}
	if in.AutoDNS {
		step("新节点就绪，切换DNS")
		cutover = true
		res, err := s.cloudflareClient().UpsertARecords(ctx, domain, newHost)
		if err != nil {
			finish(fmt.Errorf("DNS切换失败：%w", err))
			return
		}
		if warn := res.Warning(); warn != "" {
			logf("注意：%s", warn)
		}
	} else {
		step("新节点已就绪，请手工切换DNS（等待最多10分钟）")
		cutover = true
	}
	waitUntil := time.Now().Add(10 * time.Minute)
	for {
		if err := checkDomainPointsTo(ctx, domain, newHost); err == nil {
			break
		}
		if time.Now().After(waitUntil) {
			finish(errors.New("DNS切换等待超时；新节点仍保留，自动模式将回退旧IP"))
			return
		}
		select {
		case <-ctx.Done():
			finish(ctx.Err())
			return
		case <-time.After(10 * time.Second):
		}
	}
	newAPI, err := migrationEndpoint(oldAPIURL, newHost, domain)
	if err != nil {
		finish(err)
		return
	}
	// 2026-11-06：切到新机器时清掉计量基线并与计量互斥（同换绑节点 API，见 bindNodeAPI）。原来保留旧机器的
	// 计数器：下一轮计量 boot_id 不同、网卡名相同，新机器开机以来的全部流量一次算进本周期，可能当场超额停机。
	unlockMeter, lockErr := lockTrafficMeterCtx(ctx, nodeID)
	if lockErr != nil {
		finish(fmt.Errorf("等待流量计量锁失败：%w", lockErr))
		return
	}
	_, err = s.db.Exec(ctx, `UPDATE hysteria_nodes SET server_id=$2::uuid,api_url=$3,`+trafficBaselineResetSQL+`,updated_at=now() WHERE id=$1::uuid`, nodeID, newServerID, newAPI)
	unlockMeter()
	if err != nil {
		finish(err)
		return
	}
	final, err := fetchPortalMeta(ctx, newAPI, oldTarget.APIKey)
	if err != nil || final.Meta.PublicIP != newHost {
		finish(errors.New("最终新节点身份核验失败，执行回退"))
		return
	}
	committed = true
	invalidateHealthGateCache()
	InvalidateSubscriptionCache()
	logf("新节点身份、监听端口、用户数及所需Reality已核验；DNS已切换")

	logf("旧主机上的服务未改动：确认新节点正常后，可在 VPS 主机页对旧主机执行「卸载 Hysteria 2」或直接退订")
	finish(nil)
}

func sudoPrefix(user string) string {
	if user == "root" {
		return ""
	}
	return "sudo -n "
}

// runWithStdin 在远端执行命令并把 data 作为标准输入（用于传 tar 包）。
func runWithStdin(ctx context.Context, client *ssh.Client, command string, data []byte) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	var stderr bytes.Buffer
	session.Stdin = bytes.NewReader(data)
	session.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%v %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
}
