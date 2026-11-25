package auth

import (
	"testing"
	"time"
)

// 2026-11-03：账号不存在时也要花与校验真实哈希相当的时间。
func TestBurnPasswordCheckCostsLikeVerify(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	VerifyPassword(hash, "wrong-password-value")
	real := time.Since(start)
	start = time.Now()
	BurnPasswordCheck("wrong-password-value")
	burn := time.Since(start)
	if burn < real/3 {
		t.Fatalf("dummy check too cheap: burn=%v real=%v", burn, real)
	}
}
