package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"unicode"
)

type Config struct {
	// Explicit opt-in; legacy viewer access remains unchanged by default.
	ViewerRestrictSubscriptions bool
	TrustedProxyCIDRs           []string
	Port                        int
	DatabaseURL                 string
	AllowedOrigins              []string
	EnableCollector             bool
	CollectorSeconds            int
	JWTSecret                   string
	JWTTTLMinutes               int
	CredentialKey               string
	ExecutorSeconds             int
	SeedAdminEmail              string
	SeedAdminPassword           string
	// GeoOfflineDB 是离线归属库文件路径（可选）。为空表示不加载库，
	// 识别只走公网 API；API 也被关掉时识别功能整体静默关闭。
	//
	// 之所以做成"路径"而不是内置数据：归属数据是持续变化的，
	// 内置进二进制就等于把数据更新变成代码发版（详见 geo 包注释）。
	GeoOfflineDB string
	// GeoEnableAPI 控制是否允许在离线库查不到时调用公网 API。
	// 默认 true（开箱可用）；置 false 则完全不联网 —— 适合严格内网部署。
	GeoEnableAPI bool
	// GeoAPIBaseURL 覆盖兜底 API 的地址（默认 ip-api.com）。留空用默认。
	// 存在的意义：换了供应商/自建了服务时不用改代码。
	GeoAPIBaseURL string
	// PurityOfflineDB 是 IP 纯净度离线判定库文件路径（可选）。为空表示不加载，
	// 判定只走第三方 API；API 密钥也没配时纯净度功能整体静默关闭。
	PurityOfflineDB string
	// PurityAPIKey 是第三方纯净度 API（ipapi.is）的密钥。**留空即不启用 API**，
	// 此时只有离线库能给出连接类型；部署方注册后填这里即可。
	PurityAPIKey string
	// PurityIPQSKey 是 IPQualityScore 的密钥（可选），用于单 IP 欺诈分。也可在系统设置里填。
	PurityIPQSKey string
	// PurityAPIBaseURL 覆盖第三方纯净度 API 的地址。留空用默认（ipapi.is）。
	PurityAPIBaseURL string
	// SpeedtestIntervalMinutes 是三网测速的周期（分钟）。
	//
	// 默认 15 分钟：三网线路质量是**慢变量**（除非运营商调整路由，
	// 一天内基本不变），跑更勤只增加节点负担而不带来新信息。
	// 想立刻看结果用界面上的「立即测速」。
	//
	// 设为 0 表示**禁用**周期测速（手动触发仍可用）——
	// 节点很多、或不想让面板定期 SSH 到节点的用户会需要它。
	SpeedtestIntervalMinutes int
}

func Load() (Config, error) {
	port := 8080
	if raw := strings.TrimSpace(os.Getenv("PORT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 65535 {
			return Config{}, fmt.Errorf("PORT must be an integer between 1 and 65535")
		}
		port = parsed
	}

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	rawOrigins := envOr("ALLOWED_ORIGIN", "http://localhost:5173")
	origins := make([]string, 0)
	for _, value := range strings.Split(rawOrigins, ",") {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}

	collectorSeconds := 30
	if raw := strings.TrimSpace(os.Getenv("COLLECTOR_INTERVAL_SECONDS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed > 0 && parsed <= 3600 {
			collectorSeconds = parsed
		}
	}

	jwtTTL := 720
	if raw := strings.TrimSpace(os.Getenv("JWT_TTL_MINUTES")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed > 0 && parsed <= 24*60 {
			jwtTTL = parsed
		}
	}

	secret := os.Getenv("JWT_SECRET")
	if len(secret) < 32 || strings.TrimSpace(secret) == "" {
		return Config{}, fmt.Errorf("JWT_SECRET must be at least 32 non-whitespace characters")
	}
	adminEmail := strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL"))
	adminPassword := os.Getenv("SEED_ADMIN_PASSWORD")
	if adminEmail == "" || len(adminPassword) < 12 || strings.TrimSpace(adminPassword) == "" {
		return Config{}, fmt.Errorf("SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD (at least 12 characters) are required")
	}

	credentialKey := strings.TrimSpace(os.Getenv("CREDENTIAL_KEY"))
	if credentialKey == "" {
		return Config{}, fmt.Errorf("CREDENTIAL_KEY is required (base64 of 32 random bytes)")
	}

	executorSeconds := 20
	if raw := strings.TrimSpace(os.Getenv("EXECUTOR_INTERVAL_SECONDS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed > 0 && parsed <= 3600 {
			executorSeconds = parsed
		}
	}

	// 2026-11-04：布尔开关用 strconv.ParseBool，写错直接启动失败。原来只认字面量 "true"：
	// VIEWER_RESTRICT_SUBSCRIPTIONS=1 这种安全开关会被静默当成关闭，日志里也没有任何提示。
	viewerRestrict, err := envBool("VIEWER_RESTRICT_SUBSCRIPTIONS", false)
	if err != nil {
		return Config{}, err
	}
	enableCollector, err := envBool("ENABLE_COLLECTOR", true)
	if err != nil {
		return Config{}, err
	}
	geoEnableAPI, err := envBool("GEO_ENABLE_API", true)
	if err != nil {
		return Config{}, err
	}

	// 启动配置阶段即校验可信代理网段，兼容逗号与任意空白分隔，避免非法网段穿过配置加载后在路由初始化时崩溃。
	trustedProxyCIDRs := strings.FieldsFunc(os.Getenv("TRUSTED_PROXY_CIDRS"), func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	for _, entry := range trustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			return Config{}, fmt.Errorf("TRUSTED_PROXY_CIDRS contains invalid CIDR %q: %w", entry, err)
		}
	}

	return Config{
		ViewerRestrictSubscriptions: viewerRestrict,
		TrustedProxyCIDRs:           trustedProxyCIDRs,
		Port:                        port,
		DatabaseURL:                 databaseURL,
		AllowedOrigins:              origins,
		EnableCollector:             enableCollector,
		CollectorSeconds:            collectorSeconds,
		JWTSecret:                   secret,
		JWTTTLMinutes:               jwtTTL,
		CredentialKey:               credentialKey,
		ExecutorSeconds:             executorSeconds,
		SeedAdminEmail:              adminEmail,
		SeedAdminPassword:           adminPassword,
		GeoOfflineDB:                strings.TrimSpace(os.Getenv("GEO_OFFLINE_DB")),
		// 默认开启：不配任何环境变量时，加节点/面板也能自动识别。
		GeoEnableAPI:  geoEnableAPI,
		GeoAPIBaseURL: strings.TrimSpace(os.Getenv("GEO_API_BASE_URL")),
		// IP 纯净度：密钥留空即不启用（只有离线库能用）。空串是合法状态。
		PurityOfflineDB:  strings.TrimSpace(os.Getenv("IP_PURITY_OFFLINE_DB")),
		PurityAPIKey:     strings.TrimSpace(os.Getenv("IP_PURITY_API_KEY")),
		PurityIPQSKey:    strings.TrimSpace(os.Getenv("IP_PURITY_IPQS_KEY")),
		PurityAPIBaseURL: strings.TrimSpace(os.Getenv("IP_PURITY_API_BASE_URL")),
		// 0 是合法值（= 禁用周期测速），所以不能用 envOr 的"空则取默认值"。
		// 默认 15 分钟；显式写 0 才禁用。
		SpeedtestIntervalMinutes: speedtestInterval(),
	}, nil
}

// speedtestInterval 解析测速周期（分钟）。
//
// 单独一个函数是因为它的"默认值 vs 显式 0"语义与别处不同：
// 其他配置项用 envOr(name, default)，空值即取默认；
// 而这里 **0 是有意义的值**（禁用），必须与"没设置"区分开。
// 用 envOr 会把显式的 0 也当成"没设置"从而变回 15 —— 用户明明想禁用，
// 却发现面板还在定期 SSH 到他的机器上。
func speedtestInterval() int {
	raw := strings.TrimSpace(os.Getenv("SPEEDTEST_INTERVAL_MINUTES"))
	if raw == "" {
		return 15
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 || parsed > 24*60 {
		// 非法值退回默认，不让一个手滑的环境变量把测速关掉或调成每小时。
		return 15
	}
	return parsed
}

// envBool 读布尔环境变量：未设置时用 fallback；1/true/TRUE/0/false 等都接受；其它值报错。
func envBool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (true/false/1/0), got %q", name, raw)
	}
	return v, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
