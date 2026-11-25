package httpapi

import (
	"strings"
	"testing"
)

func TestInstallNodeImportRejectsMalformedCreateResponse(t *testing.T) {
	body := funcBlock(t, "installhy2.go", "func (s *Server) importInstalledNode(")
	if !strings.Contains(body, "json.Unmarshal(rec, &created); err != nil") {
		t.Fatal("install flow must reject malformed create-node responses")
	}
	if !strings.Contains(body, "创建节点响应无法解析") || !strings.Contains(body, "if created.ID == \"\" {") {
		t.Fatal("install flow must surface malformed responses and still require a returned node id")
	}
}
