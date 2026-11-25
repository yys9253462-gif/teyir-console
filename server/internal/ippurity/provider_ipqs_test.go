package ippurity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeyStoreWebOverridesEnv(t *testing.T) {
	k := NewKeyStore(map[string]string{KeyIPQS: "env-key"})
	if k.Get(KeyIPQS) != "env-key" || k.Source(KeyIPQS) != "env" {
		t.Fatal("env key should be used when no web key")
	}
	k.SetWeb(KeyIPQS, "web-key")
	if k.Get(KeyIPQS) != "web-key" || k.Source(KeyIPQS) != "web" {
		t.Fatal("web key must override env key")
	}
	k.SetWeb(KeyIPQS, "")
	if k.Get(KeyIPQS) != "env-key" {
		t.Fatal("clearing web key must fall back to env")
	}
}

func TestNormalizeIPQS(t *testing.T) {
	var r ipqsResponse
	_ = json.Unmarshal([]byte(`{"success":true,"fraud_score":88,"proxy":false,"vpn":true,"tor":true,"recent_abuse":false}`), &r)
	v := normalizeIPQS("1.1.1.1", r)
	if v.FraudScore == nil || *v.FraudScore != 88 || v.ScoreSource != "ipqs" {
		t.Fatalf("score wrong: %+v", v)
	}
	if v.Proxy == nil || !*v.Proxy {
		t.Fatal("tor must count as proxy")
	}
	if v.VPN == nil || !*v.VPN || v.Abuser == nil || *v.Abuser {
		t.Fatal("vpn / abuse flags wrong")
	}
}

func TestIPQSLookupNoKeyAndErrorsHideKey(t *testing.T) {
	if _, err := (IPQS{Keys: NewKeyStore(nil)}).Lookup(context.Background(), http.DefaultClient, "1.1.1.1"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("want ErrNoKey, got %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"message":"Invalid API key."}`))
	}))
	defer srv.Close()
	secret := "SECRETKEY123456"
	p := IPQS{Keys: NewKeyStore(map[string]string{KeyIPQS: secret}), BaseURL: srv.URL}
	_, err := p.Lookup(context.Background(), srv.Client(), "1.1.1.1")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error must exist and never contain the key: %v", err)
	}
}
