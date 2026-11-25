package httpapi

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed telegram_forms.json
var telegramFormsJSON []byte

type telegramFormField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Location string `json:"location"`
}
type telegramForm struct {
	Fields  []telegramFormField `json:"fields"`
	Browser bool                `json:"browser"`
}
type telegramChoice struct{ Label, Value string }
type telegramWizard struct {
	Pattern, Nonce string
	Fields         []telegramFormField
	Index          int
	Values         map[string]any
	Choices        []telegramChoice
	Until          time.Time
}

var telegramForms = func() map[string]telegramForm {
	var m map[string]telegramForm
	_ = json.Unmarshal(telegramFormsJSON, &m)
	return m
}()

var telegramFieldLabels = map[string]string{
	"id": "对象", "uid": "节点用户", "pid": "代理服务", "inboundID": "入站编号", "clientId": "客户端编号", "email": "邮箱", "name": "名称", "label": "名称", "note": "备注", "domain": "域名", "port": "端口", "host": "IP或域名", "ssh_port": "SSH端口", "ssh_user": "SSH用户名", "region": "地区", "provider": "供应商", "tags": "标签（逗号分隔）", "allow_write_ops": "允许主机写操作", "write_units": "授权服务（逗号分隔）", "duration_days": "有效期（天）", "extend_days": "续期天数", "expires_at": "到期时间戳（通常跳过，改填有效期天数）", "ip_limit": "设备/IP上限", "traffic_gb": "流量额度GB", "add_traffic_gb": "追加流量GB", "enabled": "启用", "auto_delete": "到期删除记录", "reset_ips": "重置设备IP", "user_email": "客户邮箱", "category_id": "分类", "server_id": "主机", "target_server_id": "目标主机", "node_ids": "节点列表（逗号分隔）", "auto_dns": "自动配置Cloudflare DNS", "in_store": "上架商城", "verb": "节点操作", "params": "操作参数（每行 参数名=值）", "operation_type": "操作类型", "unit": "服务名", "preset": "运维预设", "command": "命令", "monthly_cost": "每月成本", "revenue_monthly": "每月收入", "currency": "币种", "revenue_currency": "收入币种", "auto_renew": "自动续费", "expires_on": "到期日期 YYYY-MM-DD", "ip": "公网IP", "update_dns": "同步DNS", "key": "任务/设置名称", "chat_id": "Telegram私聊ID", "password": "密码（不会回显）", "current_password": "当前密码（不会回显）", "new_password": "新密码（不会回显）", "secret": "凭据（不会回显）", "type": "类型", "auth_type": "凭据类型", "username": "用户名", "path": "文件路径", "lines": "日志行数", "days": "天数", "limit": "条数上限", "page": "页码", "page_size": "每页条数", "sort": "排序", "status": "状态", "expected_old": "原SSH指纹", "new": "新SSH指纹", "confirm_remote_stopped": "已确认远端任务停止", "delete": "删除记录", "action": "动作", "user_id": "用户名称", "protocol": "协议", "listen": "监听地址", "target": "目标地址", "listen_port": "监听端口", "order_id": "订单编号",
}

func telegramWizardLabel(f telegramFormField) string {
	if label := telegramFieldLabels[f.Name]; label != "" {
		return label
	}
	name := strings.TrimPrefix(f.Name, "credential.")
	if label := telegramFieldLabels[name]; label != "" {
		return label
	}
	return "参数「" + f.Name + "」"
}
func (s *Server) telegramAllMenu(ctx context.Context, chat string) {
	rows := [][]map[string]string{}
	for _, g := range telegramGroups {
		rows = append(rows, []map[string]string{tgButton(g.Label, "tg:a:"+g.ID+":0")})
	}
	_ = s.telegramRich(ctx, chat, "🧩 全部功能\n选择分类，再按中文提示填写参数。终端与文件传输使用网页工作台。", rows)
}
func (s *Server) telegramWizardStart(ctx context.Context, chat string, index int) {
	if index < 0 || index >= len(s.telegramControl.Routes) {
		return
	}
	route := s.telegramControl.Routes[index]
	form, ok := telegramForms[route.Pattern]
	if !ok {
		_ = s.sendTelegram(ctx, chat, "该功能的表单尚未生成，请刷新菜单。")
		return
	}
	if form.Browser {
		var rows [][]map[string]string
		if panelURL := telegramPanelURL(); panelURL != "" {
			rows = [][]map[string]string{{{"text": "打开工作台", "url": panelURL}}}
		}
		s.telegramRich(ctx, chat, "此操作需要文件或实时交互，请使用网页工作台。", rows)
		return
	}
	fields := []telegramFormField{}
	parts := strings.SplitN(route.Pattern, " ", 2)
	for _, v := range telegramPathVars.FindAllStringSubmatch(parts[1], -1) {
		fields = append(fields, telegramFormField{v[1], "string", "path"})
	}
	fields = append(fields, form.Fields...)
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	w := &telegramWizard{Pattern: route.Pattern, Fields: fields, Values: map[string]any{}, Nonce: hex.EncodeToString(nonce), Until: time.Now().Add(10 * time.Minute)}
	st := &s.telegramControl
	st.Lock()
	if st.Wizards == nil {
		st.Wizards = map[string]*telegramWizard{}
	}
	st.Wizards[chat] = w
	delete(st.Drafts, chat)
	st.Unlock()
	s.telegramWizardPrompt(ctx, chat, w)
}
func (s *Server) telegramWizardOptions(ctx context.Context, w *telegramWizard, f telegramFormField) []telegramChoice {
	if f.Type == "bool" {
		return []telegramChoice{{"是", "true"}, {"否", "false"}}
	}
	if f.Name == "verb" {
		// 2026-11-04：不能用 portalActionVerbs() —— 它返回的是给错误提示用的「键 (说明)」整串，
		// 拿来查 portalActions 查不到（按钮没有文字），提交上去又是 400「不支持的节点操作」。
		keys := make([]string, 0, len(portalActions))
		for v := range portalActions {
			keys = append(keys, v)
		}
		sort.Strings(keys)
		out := make([]telegramChoice, 0, len(keys))
		for _, v := range keys {
			out = append(out, telegramChoice{portalActions[v].Label, v})
		}
		return out
	}
	if f.Name == "currency" || f.Name == "revenue_currency" {
		return []telegramChoice{{"人民币 CNY", "CNY"}, {"美元 USD", "USD"}, {"欧元 EUR", "EUR"}, {"港元 HKD", "HKD"}, {"日元 JPY", "JPY"}}
	}
	if f.Name == "duration_days" || f.Name == "extend_days" {
		return []telegramChoice{{"7天", "7"}, {"30天", "30"}, {"90天", "90"}, {"365天", "365"}}
	}
	if f.Name == "operation_type" {
		return []telegramChoice{{"健康检查", "health_check"}, {"日志", "logs"}, {"重启服务", "restart"}, {"启动服务", "start"}, {"停止服务", "stop"}, {"运维预设", "system_preset"}}
	}
	q := ""
	switch {
	case f.Name == "server_id" || f.Name == "target_server_id":
		q = "SELECT id::text,name FROM servers WHERE deleted_at IS NULL ORDER BY name LIMIT 40"
	case f.Name == "category_id":
		q = "SELECT id::text,name FROM node_categories ORDER BY name LIMIT 40"
	case f.Location == "path" && f.Name == "id":
		switch {
		case strings.Contains(w.Pattern, "/api/nodes/"):
			q = "SELECT id::text,name FROM hysteria_nodes WHERE deleted_at IS NULL ORDER BY name LIMIT 40"
		case strings.Contains(w.Pattern, "/api/servers/"):
			q = "SELECT id::text,name FROM servers WHERE deleted_at IS NULL ORDER BY name LIMIT 40"
		case strings.Contains(w.Pattern, "/api/subscriptions/"):
			q = "SELECT id::text,label FROM subscription_tokens WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT 40"
		case strings.Contains(w.Pattern, "/api/xui/"):
			q = "SELECT id::text,name FROM xui_panels ORDER BY name LIMIT 40"
		case strings.Contains(w.Pattern, "/api/tasks/"):
			q = "SELECT id::text,operation_type||' · '||status FROM operation_tasks ORDER BY created_at DESC LIMIT 40"
		case strings.Contains(w.Pattern, "/api/orphans/"):
			q = "SELECT id::text,node_name||' · '||user_id FROM orphan_node_accounts WHERE resolved_at IS NULL ORDER BY created_at DESC LIMIT 40"
		case strings.Contains(w.Pattern, "/api/install-jobs/"):
			q = "SELECT id,payload->>'step' FROM host_operation_jobs ORDER BY updated_at DESC LIMIT 40"
		case strings.Contains(w.Pattern, "/api/portal-update-batches/"):
			q = "SELECT id::text,status FROM portal_update_batches ORDER BY created_at DESC LIMIT 40"
		}
	}
	if q != "" {
		rows, err := s.db.Query(ctx, q)
		if err != nil {
			s.logger.Warn("telegram wizard: load options failed", "field", f.Name, "error", err)
			return nil
		}
		defer rows.Close()
		out := []telegramChoice{}
		for rows.Next() {
			var id, label string
			if err := rows.Scan(&id, &label); err != nil {
				rows.Close()
				s.logger.Warn("telegram wizard: option scan failed", "field", f.Name, "error", err)
				return nil
			}
			out = append(out, telegramChoice{label, id})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			s.logger.Warn("telegram wizard: option iteration failed", "field", f.Name, "error", err)
			return nil
		}
		return out
	}
	if f.Location == "path" && f.Name == "uid" {
		id, _ := w.Values["path.id"].(string)
		if !uuidPattern.MatchString(id) {
			return nil
		}
		raw, err := s.telegramWizardRead(ctx, "/api/nodes/"+id+"/users")
		if err != nil {
			return nil
		}
		var list struct {
			Users []struct {
				ID string `json:"user_id"`
			} `json:"users"`
		}
		if json.Unmarshal(raw, &list) != nil {
			return nil
		}
		out := []telegramChoice{}
		for _, u := range list.Users {
			if u.ID != "admin_master" {
				out = append(out, telegramChoice{u.ID, u.ID})
			}
		}
		return out
	}
	return nil
}
func (s *Server) telegramWizardRead(ctx context.Context, path string) ([]byte, error) {
	id, email, version, err := s.telegramAdmin(ctx)
	if err != nil {
		return nil, err
	}
	token, err := s.signer.IssueVersion(id, email, "admin", version)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		return nil, fmt.Errorf("读取对象失败 %d", rec.Code)
	}
	return rec.Body.Bytes(), nil
}
func (s *Server) telegramWizardPrompt(ctx context.Context, chat string, w *telegramWizard) {
	if w.Index >= len(w.Fields) {
		s.telegramWizardFinish(ctx, chat, w)
		return
	}
	f := w.Fields[w.Index]
	options := s.telegramWizardOptions(ctx, w, f)
	st := &s.telegramControl
	st.Lock()
	if st.Wizards[chat] != w {
		st.Unlock()
		return
	}
	w.Choices = options
	nonce := make([]byte, 8)
	rand.Read(nonce)
	w.Nonce = hex.EncodeToString(nonce)
	st.Unlock()
	rows := [][]map[string]string{}
	for i, o := range options {
		if i >= 40 {
			break
		}
		rows = append(rows, []map[string]string{tgButton(o.Label, fmt.Sprintf("tg:w:%s:%d", w.Nonce, i))})
	}
	if f.Location != "path" {
		rows = append(rows, []map[string]string{tgButton("跳过（保留默认）", "tg:w:"+w.Nonce+":skip")})
	}
	rows = append(rows, []map[string]string{tgButton("取消", "tg:w:"+w.Nonce+":cancel")})
	text := fmt.Sprintf("%s\n第%d/%d项：%s\n", telegramRouteTitle(w.Pattern), w.Index+1, len(w.Fields), telegramWizardLabel(f))
	if len(options) > 0 {
		text += "请点选；也可直接发送对应值。"
	} else {
		text += "请直接发送内容，无需JSON。"
	}
	if f.Type == "map" {
		text += "\n多项参数每行：名称=值"
	}
	if f.Location != "path" {
		text += "\n不需要可点「跳过」。"
	}
	_ = s.telegramRich(ctx, chat, text, rows)
}
func telegramWizardValue(f telegramFormField, text string) (any, error) {
	switch f.Type {
	case "bool":
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "是", "开启", "启用":
			return true, nil
		case "false", "否", "关闭", "停用":
			return false, nil
		}
		return nil, fmt.Errorf("请选择是或否")
	case "number":
		v, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("请输入有效数字")
		}
		return v, nil
	case "array":
		parts := strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '，' || r == '\n' })
		out := []string{}
		for _, p := range parts {
			if v := strings.TrimSpace(p); v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	case "map":
		out := map[string]string{}
		for _, line := range strings.Split(text, "\n") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("每行格式：参数名=值")
			}
			out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
		return out, nil
	default:
		if len(text) > 8000 {
			return nil, fmt.Errorf("内容过长")
		}
		return text, nil
	}
}
func (s *Server) telegramWizardInput(ctx context.Context, chat, text string) bool {
	st := &s.telegramControl
	st.Lock()
	w := st.Wizards[chat]
	st.Unlock()
	if w == nil {
		return false
	}
	if time.Now().After(w.Until) {
		st.Lock()
		delete(st.Wizards, chat)
		st.Unlock()
		s.sendTelegram(ctx, chat, "表单已过期，请从全部功能重新选择。")
		return true
	}
	if w.Index >= len(w.Fields) {
		return true
	}
	f := w.Fields[w.Index]
	if text == "/skip" && f.Location == "path" {
		s.sendTelegram(ctx, chat, "对象必须选择，不能跳过。")
		return true
	}
	if text != "/skip" {
		v, err := telegramWizardValue(f, text)
		if err != nil {
			s.sendTelegram(ctx, chat, err.Error())
			return true
		}
		st.Lock()
		w.Values[f.Location+"."+f.Name] = v
		st.Unlock()
	}
	st.Lock()
	w.Index++
	w.Until = time.Now().Add(10 * time.Minute)
	st.Unlock()
	s.telegramWizardPrompt(ctx, chat, w)
	return true
}
func (s *Server) telegramWizardCallback(ctx context.Context, chat, data string) bool {
	if !strings.HasPrefix(data, "tg:w:") {
		return false
	}
	p := strings.Split(data, ":")
	if len(p) != 4 {
		return true
	}
	st := &s.telegramControl
	st.Lock()
	w := st.Wizards[chat]
	if w == nil || w.Nonce != p[2] {
		st.Unlock()
		s.sendTelegram(ctx, chat, "该选择已过期，请使用最新一条表单。")
		return true
	}
	if p[3] == "cancel" {
		delete(st.Wizards, chat)
		st.Unlock()
		s.sendTelegram(ctx, chat, "已取消。")
		return true
	}
	if p[3] == "skip" {
		st.Unlock()
		return s.telegramWizardInput(ctx, chat, "/skip")
	}
	i, err := strconv.Atoi(p[3])
	if err != nil || i < 0 || i >= len(w.Choices) {
		st.Unlock()
		return true
	}
	value := w.Choices[i].Value
	st.Unlock()
	return s.telegramWizardInput(ctx, chat, value)
}
func setTelegramField(dst map[string]any, name string, value any) {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) == 1 {
		dst[name] = value
		return
	}
	child, ok := dst[parts[0]].(map[string]any)
	if !ok {
		child = map[string]any{}
		dst[parts[0]] = child
	}
	setTelegramField(child, parts[1], value)
}
func (s *Server) telegramWizardFinish(ctx context.Context, chat string, w *telegramWizard) {
	st := &s.telegramControl
	st.Lock()
	if st.Wizards[chat] != w {
		st.Unlock()
		return
	}
	delete(st.Wizards, chat)
	st.Unlock()
	parts := strings.SplitN(w.Pattern, " ", 2)
	params := map[string]string{}
	body := map[string]any{}
	query := url.Values{}
	for k, v := range w.Values {
		p := strings.SplitN(k, ".", 2)
		switch p[0] {
		case "path":
			params[p[1]] = fmt.Sprint(v)
		case "query":
			query.Set(p[1], fmt.Sprint(v))
		case "body":
			setTelegramField(body, p[1], v)
		}
	}
	path, err := telegramFillPath(parts[1], params)
	if err != nil {
		s.sendTelegram(ctx, chat, err.Error())
		return
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	raw, err := json.Marshal(body)
	if err != nil {
		s.sendTelegram(ctx, chat, "参数格式无效，请重新开始操作。")
		return
	}
	s.telegramRun(ctx, chat, chat, telegramAction{parts[0], path, raw}, false)
}
func telegramWizardPreview(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return "参数：默认设置"
	}
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := []string{}
	for _, k := range keys {
		n := strings.ToLower(k)
		label := telegramWizardLabel(telegramFormField{Name: k})
		if strings.Contains(n, "password") || strings.Contains(n, "secret") || strings.Contains(n, "token") {
			lines = append(lines, label+"：已填写（不回显）")
			continue
		}
		switch v := m[k].(type) {
		case map[string]any:
			lines = append(lines, label+"：已填写子项")
		case []any:
			lines = append(lines, fmt.Sprintf("%s：%d项", label, len(v)))
		default:
			r := []rune(fmt.Sprint(v))
			if len(r) > 100 {
				r = append(r[:100], '…')
			}
			lines = append(lines, label+"："+string(r))
		}
	}
	return strings.Join(lines, "\n")
}
