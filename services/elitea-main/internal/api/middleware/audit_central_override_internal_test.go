package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// The central_override mark (ADR-0059 R0.6) reaches the audit event only for
// a write that happened: a refused or failed request changed nothing.
func TestCentralOverrideIsKeptOnlyOnASuccessfulWrite(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusOK: true, http.StatusCreated: true,
		http.StatusBadRequest: false, http.StatusForbidden: false, http.StatusNotFound: false,
		http.StatusInternalServerError: false,
	} {
		ctx, slot := audit.ContextWithAnnotationSlot(context.Background())
		audit.Annotate(ctx, audit.Annotation{CentralOverride: true})
		request := httptest.NewRequest(http.MethodPut, "/api/v2/admin/users/administration/7", nil).WithContext(ctx)
		if got := buildEvent(request, status, 0, slot).CentralOverride; got != want {
			t.Errorf("status %d: central_override = %v, want %v", status, got, want)
		}
	}
}
