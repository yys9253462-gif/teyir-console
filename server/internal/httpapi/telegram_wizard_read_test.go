package httpapi

import (
	"strings"
	"testing"
)

func TestTelegramWizardOptionsRejectsPartialChoiceLists(t *testing.T) {
	body := funcBlock(t, "telegram_wizard.go", "func (s *Server) telegramWizardOptions(")
	for _, want := range []string{
		"rows.Scan(&id, &label); err != nil",
		"rows.Err()",
		"return nil",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Telegram choices must not expose partial database results; missing %q", want)
		}
	}
}
