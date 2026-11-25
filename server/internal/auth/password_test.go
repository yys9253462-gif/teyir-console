package auth

import (
	"strings"
	"testing"
)

// 字符下限和 bcrypt 字节上限必须同时满足。
func TestValidatePasswordCountsRunes(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		ok   bool
	}{
		{"4 个汉字（12 字节）", strings.Repeat("密", 4), false},
		{"11 个汉字", strings.Repeat("密", 11), false},
		{"12 个汉字", strings.Repeat("密", 12), true},
		{"12 个 ASCII", "abcdefghijkl", true},
		{"11 个 ASCII", "abcdefghijk", false},
		{"超长（超过 bcrypt 72 字节）", strings.Repeat("a", 80), false},
		{"恰好 72 字节", strings.Repeat("a", 72), true},
	}
	for _, c := range cases {
		err := ValidatePassword(c.pw)
		if c.ok && err != nil {
			t.Errorf("%s：应当通过，却报错 %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s：应当拒绝，却通过了", c.name)
		}
	}
}

// HashPassword 必须与 ValidatePassword 同口径：否则同一口令在 HTTP 层被拒、
// 在内部调用被接受（或反过来），两边行为漂移。
func TestHashPasswordMatchesValidation(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("密", 4)); err == nil {
		t.Fatal("HashPassword 接受了 4 个汉字（12 字节）")
	}
	if _, err := HashPassword(strings.Repeat("a", 80)); err == nil {
		t.Fatal("HashPassword 接受了超过 72 字节的口令")
	}
	if _, err := HashPassword("abcdefghijkl"); err != nil {
		t.Fatalf("合法口令被拒: %v", err)
	}
}
