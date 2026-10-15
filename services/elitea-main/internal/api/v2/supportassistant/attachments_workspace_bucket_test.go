package supportassistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

type panickingDownloader struct{}

func (panickingDownloader) DownloadObject(http.ResponseWriter, *http.Request) {
	panic("the hidden workspace bucket reached the object read")
}

// TestGetAttachmentRefusesTheWorkspaceBucket: the hidden workspace bucket
// (ADR-0034 decision 3) is a 404 before the caller, the conversation or the
// object store is consulted.
func TestGetAttachmentRefusesTheWorkspaceBucket(t *testing.T) {
	handler := NewHandler(nil, WithAttachmentDownloader(panickingDownloader{}))
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("bucket", storage.WorkspaceBucketName)
	routeCtx.URLParams.Add("*", "t/0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b/seg/x.seg")
	request := httptest.NewRequest(http.MethodGet, "/attachments/x", nil)
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeCtx))
	recorder := httptest.NewRecorder()
	handler.GetAttachment(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}
