package admin_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
)

type fakeMailer struct {
	configured bool
	fail       error
	sent       []string
	invited    []appmailer.Invitation
}

func (f *fakeMailer) Configured(context.Context) bool { return f.configured }
func (f *fakeMailer) SendTest(_ context.Context, to string) error {
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, to)
	return nil
}
func (f *fakeMailer) SendInvitation(_ context.Context, invitation appmailer.Invitation) error {
	if f.fail != nil {
		return f.fail
	}
	f.invited = append(f.invited, invitation)
	return nil
}

func postTestEmail(handler *admin.Handler, body string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Post("/admin/branding/test_email/administration", handler.BrandingTestEmail)
	req := httptest.NewRequest(http.MethodPost, "/admin/branding/test_email/administration", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestBrandingTestEmail(t *testing.T) {
	t.Run("sends to a plain address", func(t *testing.T) {
		mailer := &fakeMailer{configured: true}
		rec := postTestEmail(admin.NewHandler(nil, admin.WithMailer(mailer)), `{"to":"ops@acme.example"}`)
		if rec.Code != http.StatusOK || len(mailer.sent) != 1 || mailer.sent[0] != "ops@acme.example" {
			t.Fatalf("status %d sent %v body %s", rec.Code, mailer.sent, rec.Body.String())
		}
	})
	t.Run("503 without a configured mailer", func(t *testing.T) {
		for _, handler := range []*admin.Handler{
			admin.NewHandler(nil),
			admin.NewHandler(nil, admin.WithMailer(&fakeMailer{configured: false})),
		} {
			if rec := postTestEmail(handler, `{"to":"ops@acme.example"}`); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
		}
	})
	t.Run("400 on a display-name or empty address", func(t *testing.T) {
		mailer := &fakeMailer{configured: true}
		handler := admin.NewHandler(nil, admin.WithMailer(mailer))
		for _, body := range []string{`{"to":"Ops <ops@acme.example>"}`, `{"to":""}`, `{}`, `not json`} {
			if rec := postTestEmail(handler, body); rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status %d", body, rec.Code)
			}
		}
		if len(mailer.sent) != 0 {
			t.Fatal("a refused request sent mail")
		}
	})
	t.Run("502 names the relay's refusal", func(t *testing.T) {
		mailer := &fakeMailer{configured: true, fail: errors.New("550 mailbox unavailable")}
		rec := postTestEmail(admin.NewHandler(nil, admin.WithMailer(mailer)), `{"to":"ops@acme.example"}`)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "550 mailbox unavailable") {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
}
