package portaltls

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestDefaultTLSRejectsUntrustedCertificate(t *testing.T) {
	t.Setenv("PORTAL_TLS_PINS", "")
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer s.Close()
	base := &http.Client{Transport: &http.Transport{}}
	c, err := Client(base, s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get(s.URL); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}
func TestOriginPinAcceptsOnlyExactCertificate(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer s.Close()
	digest := sha256.Sum256(s.Certificate().Raw)
	u, _ := url.Parse(s.URL)
	for _, correct := range []bool{true, false} {
		pin := hex.EncodeToString(digest[:])
		if !correct {
			pin = "0000000000000000000000000000000000000000000000000000000000000000"
		}
		raw, _ := json.Marshal(map[string]string{u.Host: pin})
		t.Setenv("PORTAL_TLS_PINS", string(raw))
		c, err := Client(&http.Client{Transport: &http.Transport{}}, s.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Get(s.URL)
		if correct && err != nil {
			t.Fatal(err)
		}
		if !correct && err == nil {
			t.Fatal("wrong pin accepted")
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
}
func TestMalformedPinsFailClosed(t *testing.T) {
	for _, raw := range []string{"invalid", `{"example.com":"bad-pin"}`} {
		t.Setenv("PORTAL_TLS_PINS", raw)
		if _, err := Client(&http.Client{Transport: &http.Transport{}}, "https://example.com"); err == nil {
			t.Fatal("bad configuration accepted")
		}
	}
}
