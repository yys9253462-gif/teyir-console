package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 面板登录人机验证（2026-10-29）。
//
// 背景：2026-10-29 凌晨 panel.example.com 被约 1250 个 IP（Tor 出口 + CF Workers）
// 分布式爆破，每个 IP 只试 2-3 次，按 IP 的限流/封禁全部失效。攻击脚本是
// python-requests 直接 POST /api/login，不加载页面 —— 加 Turnstile 后这类请求
// 拿不到 token，直接 400。
//
// 配置：TURNSTILE_SITE_KEY + TURNSTILE_SECRET 同时设置才启用；任一为空 = 关闭
// （与加这个功能之前一样，方便本地开发和测试）。
// 校验失败或 Cloudflare 不可达一律拒绝登录（fail closed）：宁可暂时登不上，
// 也不能让爆破绕过。应急关闭 = 清空 .env 里的 TURNSTILE_SECRET 后重启 api。

const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

var turnstileHTTPClient = &http.Client{Timeout: 8 * time.Second}

// turnstileSlots 限制同时在途的 siteverify 请求数。
//
// ⚠️ 2026-10-30：登录是**公开端点**，每个请求都要外呼 Cloudflare 一次，
// 而单次最长 8 秒。分布式爆破（1250 个 IP 各试两三次，正是这次被攻击的形态）
// 会让面板同时挂着上千条对外连接：自己的 goroutine/文件描述符先耗尽，
// 之后所有接口一起慢 —— 一个"防爆破"的增强反过来成了放大器。
//
// 取 16：正常登录页同时在线的人远小于这个数；饱和时**立即失败**（fail closed），
// 让攻击请求快速结束而不是继续排队等 Cloudflare。
const turnstileConcurrency = 16

var turnstileSlots = make(chan struct{}, turnstileConcurrency)

func turnstileKeys() (siteKey, secret string) {
	siteKey = strings.TrimSpace(os.Getenv("TURNSTILE_SITE_KEY"))
	secret = strings.TrimSpace(os.Getenv("TURNSTILE_SECRET"))
	if siteKey == "" || secret == "" {
		return "", ""
	}
	return siteKey, secret
}

// GET /api/login/config —— 登录页取 Turnstile site key（公开信息，无需登录）。
func (s *Server) loginConfig(w http.ResponseWriter, r *http.Request) {
	siteKey, _ := turnstileKeys()
	writeJSON(w, http.StatusOK, map[string]any{"turnstile_site_key": siteKey})
}

// verifyTurnstile 向 Cloudflare 校验 token；未启用时恒为 true。
func (s *Server) verifyTurnstile(ctx context.Context, token, ip string) bool {
	_, secret := turnstileKeys()
	if secret == "" {
		return true
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 2048 {
		if s.logger != nil {
			s.logger.Info("turnstile rejected", "ip", ip, "codes", "missing-or-oversized-token")
		}
		return false
	}
	// 取一个在途槽位；拿不到就直接失败，不排队。
	// 排队会让攻击流量在面板里越积越多，而合法用户只会更慢。
	select {
	case turnstileSlots <- struct{}{}:
		defer func() { <-turnstileSlots }()
	default:
		if s.logger != nil {
			s.logger.Warn("turnstile verify saturated, rejecting", "ip", ip, "in_flight", turnstileConcurrency)
		}
		return false
	}

	form := url.Values{"secret": {secret}, "response": {token}}
	if ip != "" {
		form.Set("remoteip", ip)
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileVerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := turnstileHTTPClient.Do(req)
	if err != nil {
		// ⚠️ 2026-10-30：这一条用 Error 级别，而且要把后果写清楚。
		//
		// 校验是 fail closed 的：面板到 Cloudflare 不通（机房出口被墙、DNS 故障、
		// CF 侧故障）时**所有登录都会被拒绝**。原来只记一条 Warn 加裸错误，
		// 从日志上看和"某人密码输错了"没有区别 —— 真出事时没人会想到去查
		// 面板出网，只会以为凭据坏了。这里明确写出「期间无人能登录」。
		if s.logger != nil {
			s.logger.Error("turnstile verify unreachable: 面板无法访问 Cloudflare，期间所有登录都会被拒绝（fail closed）；确认出网或清空 TURNSTILE_SECRET 应急", "ip", ip, "error", err)
		}
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		if s.logger != nil {
			s.logger.Warn("turnstile verify decode failed", "status", resp.StatusCode, "error", err)
		}
		return false
	}
	if !out.Success && s.logger != nil {
		s.logger.Info("turnstile rejected", "ip", ip, "codes", strings.Join(out.ErrorCodes, ","))
	}
	return out.Success
}

// validLoginService exempts only the configured machine account from the
// interactive challenge. Password verification and login throttling still apply.
// Disabled unless a strong key and an exact account allowlist are configured.
func validLoginService(email, key string) bool {
	expected := os.Getenv("LOGIN_SERVICE_KEY")
	allowed := strings.TrimSpace(os.Getenv("LOGIN_SERVICE_EMAIL"))
	if allowed == "" {
		allowed = strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL"))
	}
	if len(expected) < 32 || len(key) > 1024 || allowed == "" || email != allowed {
		return false
	}
	a, b := sha256.Sum256([]byte(expected)), sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
