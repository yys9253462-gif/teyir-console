package httpapi

import (
	"strings"
	"testing"
)

func TestOutboundProbeRejectsMalformedPortalResults(t *testing.T) {
	body := funcBlock(t, "outbounds.go", "func (s *Server) nodeOutboundProbe(")
	for _, want := range []string{
		"json.Unmarshal(env.Data, &res); err != nil",
		"res.IP == \"\" || res.Kind == \"\"",
		"http.StatusBadGateway",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("outbound probe must not report incomplete portal data as success; missing %q", want)
		}
	}
}
