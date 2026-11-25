package httpapi

import (
	"strings"
	"testing"
)

func TestCategoryUpdateDoesNotReturnSuccessfulEmptyListWhenRefreshFails(t *testing.T) {
	body := funcBlock(t, "categories.go", "func (s *Server) categoryUpdate(")
	refresh := strings.Index(body, "cats, err := s.listCategories(r.Context())")
	if refresh < 0 {
		t.Fatal("category update must refresh its response from the category source")
	}
	errCheck := strings.Index(body[refresh:], "if err != nil")
	databaseError := strings.Index(body[refresh:], "s.databaseError(w, r, err)")
	response := strings.Index(body[refresh:], "writeJSON(w, http.StatusOK")
	if errCheck < 0 || databaseError < 0 || response < 0 || databaseError > response {
		t.Fatal("category refresh failure must not return HTTP 200 with a misleading partial or empty list")
	}
}
