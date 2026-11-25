package httpapi

import (
	"strings"
	"testing"
)

func assertSuccessValidationBeforeAudit(t *testing.T, file, function string, checks []string) {
	t.Helper()
	body := funcBlock(t, file, function)
	lastCheck := -1
	for _, check := range checks {
		idx := strings.Index(body, check)
		if idx < 0 {
			t.Errorf("%s must validate node success response; missing %q", function, check)
			continue
		}
		if idx > lastCheck {
			lastCheck = idx
		}
	}
	audit := strings.LastIndex(body, "s.audit(")
	if lastCheck >= 0 && (audit < 0 || lastCheck > audit) {
		t.Errorf("%s must verify the node's result before auditing success", function)
	}
}

func TestRenewUserValidatesPortalSuccessAndIdentity(t *testing.T) {
	assertSuccessValidationBeforeAudit(t, "portalusers.go", "func (s *Server) portalRenewUser(", []string{
		"!parsed.OK",
		"parsed.UserID != userID",
		"parsed.ExpiresAt <= 0",
		"if json.Unmarshal(envelope.Data, &result) != nil || !result.OK || result.User.ExpiresAt != *input.ExpiresAt",
	})
}

func TestCreateProxyServiceRequiresCompletePortalConfirmation(t *testing.T) {
	assertSuccessValidationBeforeAudit(t, "portalproxy.go", "func (s *Server) portalCreateProxyService(", []string{
		"if !parsed.OK || parsed.ID == \"\"",
		"parsed.Port < 1 || parsed.Port > 65535",
		"parsed.Username == \"\" || parsed.Password == \"\"",
	})
}
