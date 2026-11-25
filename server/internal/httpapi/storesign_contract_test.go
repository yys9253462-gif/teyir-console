package httpapi

import (
	"strings"
	"testing"
)

func TestStoreSignatureReadFailureFailsClosed(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) requireStoreKey(")
	checks := []string{
		"signatureRequired, err := s.storeSignatureRequired(r)",
		"s.databaseError(w, r, err)",
		"if !protocolOK",
		"if signatureRequired {",
		"if !signed && signatureRequired",
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("store auth must fail closed when signature policy cannot be read; missing %q", want)
		}
	}
}

func TestStoreSignaturePolicyReadReturnsDatabaseError(t *testing.T) {
	body := funcBlock(t, "storesign.go", "func (s *Server) storeSignatureRequired(")
	if !strings.Contains(body, "(bool, error)") || !strings.Contains(body, "return false, err") {
		t.Fatal("signature policy read errors must reach the enforcing middleware")
	}
}
