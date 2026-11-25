package secrets

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func testKey32() string {
	raw := bytes.Repeat([]byte{0x42}, 32)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestNewBoxValidatesKey(t *testing.T) {
	if _, err := NewBox("not-valid-base64!!!"); err == nil {
		t.Fatal("非法编码密钥必须被拒绝")
	}
	shortKey := base64.StdEncoding.EncodeToString([]byte("too-short-key"))
	if _, err := NewBox(shortKey); err == nil {
		t.Fatal("非 32 字节密钥必须被拒绝")
	}
	box, err := NewBox(testKey32())
	if err != nil {
		t.Fatalf("合法 32 字节密钥初始化失败: %v", err)
	}
	if box.Version != "aesgcm-v1" {
		t.Fatalf("版本标识异常: %s", box.Version)
	}
}

func TestBoxSealOpenAndTamperRejection(t *testing.T) {
	box, err := NewBox(testKey32())
	if err != nil {
		t.Fatalf("初始化加密盒失败: %v", err)
	}
	plaintext := []byte("fake-credential-payload-for-unit-test-only")
	ct1, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("首次加密失败: %v", err)
	}
	ct2, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("二次加密失败: %v", err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("相同明文两次加密不得产生相同密文（随机数必须独立）")
	}
	got, err := box.Open(ct1)
	if err != nil {
		t.Fatalf("解密合法密文失败: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("解密明文不匹配: got=%q want=%q", got, plaintext)
	}

	// 篡改密文最后一个字节，必须验签失败。
	tampered := append([]byte(nil), ct1...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := box.Open(tampered); err == nil {
		t.Fatal("被篡改的密文必须解密失败")
	}

	// 截断为短于随机数长度的密文，必须报错且不得崩溃。
	if _, err := box.Open([]byte{1, 2, 3}); err == nil {
		t.Fatal("过短密文必须被拒绝")
	}
}
