package ippurity

import "sync"

// KeyStore 保存第三方 API 密钥，可在运行时更新（2026-10-15）。
//
// 密钥原本只从环境变量读、启动时固定。现在允许在 web 面板「系统设置」里填，
// 保存后立即生效、无需重启 —— provider 每次查询时从这里取当前值。
//
// 优先级：面板里设置的值 > 环境变量。面板里清空 = 回落到环境变量。
type KeyStore struct {
	mu  sync.RWMutex
	env map[string]string
	web map[string]string
}

// 已知的密钥名。
const (
	KeyIPAPIIs = "ipapi_is"
	KeyIPQS    = "ipqs"
	// KeyCloudflare 不属于 IP 检测，但同样是「面板里填、加密存库、热更新」的第三方密钥，共用这个存储。
	KeyCloudflare = "cloudflare"
	// KeyTelegram 是 Telegram Bot Token（告警推送与只读机器人）。
	KeyTelegram = "telegram"
)

func NewKeyStore(env map[string]string) *KeyStore {
	k := &KeyStore{env: map[string]string{}, web: map[string]string{}}
	for name, v := range env {
		k.env[name] = v
	}
	return k
}

// Get 返回生效的密钥；nil KeyStore 返回空串。
func (k *KeyStore) Get(name string) string {
	if k == nil {
		return ""
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if v := k.web[name]; v != "" {
		return v
	}
	return k.env[name]
}

// Source 返回生效密钥的来源："web" / "env" / ""（未配置）。
func (k *KeyStore) Source(name string) string {
	if k == nil {
		return ""
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	switch {
	case k.web[name] != "":
		return "web"
	case k.env[name] != "":
		return "env"
	}
	return ""
}

// SetWeb 设置面板侧的值；空串表示清除（回落到环境变量）。
func (k *KeyStore) SetWeb(name, value string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if value == "" {
		delete(k.web, name)
		return
	}
	k.web[name] = value
}
