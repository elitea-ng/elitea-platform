package browsersession

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The sealed cookie is distinct from the ordinary one and carries the four
// attributes the `__Host-` prefix and ADR-0059 R2.7a require: Secure,
// HttpOnly, SameSite=Strict and Path=/ with no Domain.
func TestSealedCookieAttributes(t *testing.T) {
	if SealedCookieName == CookieName || !strings.HasPrefix(SealedCookieName, "__Host-") {
		t.Fatalf("the sealed cookie %q is not a distinct __Host- cookie", SealedCookieName)
	}
	recorder := httptest.NewRecorder()
	SetSealedCookie(recorder, "s1.value", 90*time.Minute)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies %v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != SealedCookieName || cookie.Value != "s1.value" || !cookie.Secure || !cookie.HttpOnly ||
		cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" ||
		cookie.MaxAge != int((90*time.Minute)/time.Second) {
		t.Fatalf("sealed cookie %+v", cookie)
	}
	header := recorder.Header().Get("Set-Cookie")
	for _, attribute := range []string{"Secure", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(header, attribute) {
			t.Errorf("Set-Cookie %q lacks %s", header, attribute)
		}
	}
	if strings.Contains(header, "Domain=") {
		t.Errorf("Set-Cookie %q names a Domain", header)
	}

	recorder = httptest.NewRecorder()
	ClearSealedCookie(recorder)
	cleared := recorder.Result().Cookies()[0]
	if cleared.Name != SealedCookieName || cleared.MaxAge >= 0 || cleared.Value != "" || !cleared.Secure ||
		cleared.SameSite != http.SameSiteStrictMode || cleared.Path != "/" {
		t.Fatalf("the clearing cookie %+v does not replace the sealed one", cleared)
	}
}

// Manager.Create never writes the sealed kind: only StartSealedSession does.
func TestManagerCreateRefusesTheSealedProvider(t *testing.T) {
	manager, err := NewManager(newMemoryStore(), Policy{AbsoluteLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), NewSession{UserID: 1, Provider: ProviderSealed}); err == nil {
		t.Fatal("Create wrote a sealed session")
	}
}

// A store that cannot read the elevation ledger authenticates no sealed
// session.
func TestValidateSealedFailsClosedWithoutTheLedger(t *testing.T) {
	manager, err := NewManager(newMemoryStore(), Policy{AbsoluteLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ValidateSealed(context.Background(), "s1.anything"); !errors.Is(err, ErrSealedUnavailable) {
		t.Fatalf("ValidateSealed on a store without the ledger = %v, want ErrSealedUnavailable", err)
	}
}
