package geo

import (
	"strings"
	"sync"
	"time"
)

// Cache 是识别结果的进程内缓存。
//
// 为什么必须有它，而不是「反正离线库很快」：
// 公网兜底 API 有 45 次/分钟的限速，而调用点（导入节点、刷新面板、
// 批量重扫）天然是连发的 —— 一次导入 5 个节点就是 5 次查询，
// 手抖点两下刷新就可能撞限速。缓存把「同一台机器」压成一次出网。
//
// 为什么不落库：识别结果的价值只在于「填一次字段」。填完之后
// region/provider 就是用户自己的数据了（还可能被手工改过），
// 再落一张 geo_cache 表意味着要给「用户改了名字后缓存算不算失效」
// 定义规则 —— 那是自找的复杂度。进程内缓存足够吸收突发流量。
//
// TTL 选 24 小时：VPS 的归属极少变（换商家＝换机器），
// 而进程重启（部署/升级）本来就会清空。
type Cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration
	// maxEntries 防止长期运行时无界增长（面板管理的机器数有上限，
	// 但 IP 输入是外部来的，理论上可以被灌）。
	maxEntries int
}

type cacheEntry struct {
	info      Info
	expiresAt time.Time
}

// NewCache 构造缓存。ttl<=0 时用默认 24h；maxEntries<=0 时用默认 4096。
func NewCache(ttl time.Duration, maxEntries int) *Cache {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &Cache{
		entries:    make(map[string]cacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

// Get 取缓存。命中且未过期返回 (info, true)。
func (c *Cache) Get(ip string) (Info, bool) {
	return c.GetAt(ip, time.Now())
}

// GetAt 按指定时刻取缓存，供解析器与测试保持时钟口径一致。
func (c *Cache) GetAt(ip string, now time.Time) (Info, bool) {
	if c == nil {
		return Info{}, false
	}
	key := strings.TrimSpace(ip)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return Info{}, false
	}
	if now.After(entry.expiresAt) {
		delete(c.entries, key)
		return Info{}, false
	}
	return entry.info, true
}

// Put 写缓存。
func (c *Cache) Put(ip string, info Info, now time.Time) {
	if c == nil {
		return
	}
	key := strings.TrimSpace(ip)
	if key == "" {
		return
	}
	// 空结果不缓存：识别不出来的原因通常是「暂时抖动/限速」，
	// 缓存下来会让这台机器在 TTL 内永远识别不出。
	if info.Empty() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxEntries {
		c.evictLocked(now)
	}
	c.entries[key] = cacheEntry{info: info, expiresAt: now.Add(c.ttl)}
}

// evictLocked 先清过期项；仍然满就整表清空。
//
// 整表清空看着粗暴，但它是对的：这是纯加速用的缓存，清空的代价只是
// 重新查一次；而实现一套 LRU 要为这个「一天最多用几次」的场景引入
// 额外的数据结构与 bug 面。清空也只会在真的灌到 4096 条时才发生。
func (c *Cache) evictLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	if len(c.entries) >= c.maxEntries {
		c.entries = make(map[string]cacheEntry)
	}
}

// Len 返回当前条目数（诊断用）。
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
