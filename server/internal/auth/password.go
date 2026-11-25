package auth

import (
	"errors"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// 最小长度按 Unicode 字符计数；最大字节数受 bcrypt 限制。
const (
	MinPasswordRunes = 12
	// bcrypt 的硬上限是 72 字节；留 0 余量，正好等于它。
	maxPasswordBytes = 72
)

// ErrPasswordTooShort / ErrPasswordTooLong 供 handler 区分提示。
var (
	ErrPasswordTooShort = errors.New("密码至少 12 个字符")
	ErrPasswordTooLong  = errors.New("密码过长（最多 72 字节，约 24 个汉字）")
)

// ValidatePassword 按**字符**校验长度，并挡住 bcrypt 的 72 字节上限。
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordRunes {
		return ErrPasswordTooShort
	}
	if len(password) > maxPasswordBytes {
		return ErrPasswordTooLong
	}
	return nil
}

func HashPassword(password string) (string, error) {
	// 统一走 ValidatePassword：原来这里按字节卡 8 位，比 HTTP 层的"12 个字符"更松，
	// 两条路径对同一条口令给出不同结论。
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// dummyHash 是一个与真实口令同成本的 bcrypt 哈希，只用于 BurnPasswordCheck。
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("hy2-ops-console-timing-equalizer"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
}()

// BurnPasswordCheck 做一次与 VerifyPassword 同样耗时的比较，结果丢弃。
//
// 2026-11-03：登录时邮箱不存在原来直接 401，邮箱存在才跑 bcrypt（几十毫秒）——
// 只看响应时间就能枚举出哪些邮箱是账号。账号不存在时调它，让两条路径耗时一致。
func BurnPasswordCheck(password string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
}
