package admin_test

// The admin E-mail surface (gap G7).
//
// The properties asserted here are the ones this screen's hazards make worth
// asserting, and each one is invisible to a status-code test:
//
//  1. **The password is never echoed.** No route here returns it. A read says
//     `password_set` and nothing more, and the whole response body is searched
//     for the plaintext, not just the field it would obviously be in.
//  2. **The tri-state password on save.** Absent, `''` and a value mean leave
//     it, clear it, and re-seal it. A form cannot echo the stored credential,
//     so a save that always sent the field would erase it every time an
//     operator corrected a host — and every message after that would be
//     refused at AUTH.
//  3. **A refusal NAMES the field**, because the operator reading it is the one
//     who has to change it.
//  4. **`rejectCredentialField` still refuses a credential on the GENERIC
//     endpoints**, and the `email` SECTION is still unavailable there. This
//     surface exists because those endpoints cannot serve a password; a change
//     that quietly made them able to would move the SMTP password into a
//     plaintext row readable by every holder of `runtime.plugins`.
//
// No fixture value here is or resembles a real credential.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/emailsettings"
)

// The plaintext no response may ever contain.
const fixturePassword = "not-a-real-relay-password"

// fakeEmailStore records what a save asked for, without a database.
type fakeEmailStore struct {
	ready bool
	saves []emailSave
	err   error
}

type emailSave struct {
	settings emailsettings.Settings
	password *string
	author   string
}

func (f *fakeEmailStore) Ready() bool { return f.ready }

func (f *fakeEmailStore) Save(
	_ context.Context, settings emailsettings.Settings, password *string, author string,
) error {
	if f.err != nil {
		return f.err
	}
	f.saves = append(f.saves, emailSave{settings: settings, password: password, author: author})
	return nil
}

// fakeEmailResolver answers with a fixed resolution.
type fakeEmailResolver struct{ resolution emailsettings.Resolution }

func (f *fakeEmailResolver) Resolve(context.Context) emailsettings.Resolution { return f.resolution }

func configuredResolution() emailsettings.Resolution {
	stored := emailsettings.Settings{Host: "smtp.acme.example", Username: "relay-user"}
	effective := emailsettings.Settings{
		Host: "smtp.acme.example", Port: 587, TLS: emailsettings.TLSStartTLS,
		Username: "relay-user", From: "noreply@acme.example",
		PublicBaseURL: "https://ai.acme.example",
	}
	return emailsettings.Resolution{
		Stored: stored, Effective: effective, Configured: true,
		PasswordSet: true, PasswordSource: emailsettings.SourceDatabase,
	}
}

func emailRouter(handler *admin.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/admin/email/administration", handler.EmailSettingsRead)
	router.Put("/admin/email/administration", handler.EmailSettingsSave)
	router.Post("/admin/email/test/administration", handler.EmailTestSend)
	return router
}

func callEmail(handler *admin.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	emailRouter(handler).ServeHTTP(rec, req)
	return rec
}

func emailHandler(store *fakeEmailStore, resolution emailsettings.Resolution, options ...admin.Option) *admin.Handler {
	all := append([]admin.Option{
		admin.WithEmailSettings(store, &fakeEmailResolver{resolution: resolution}),
	}, options...)
	return admin.NewHandler(nil, all...)
}

func TestEmailSettingsRead(t *testing.T) {
	t.Run("reports the layers and never the password", func(t *testing.T) {
		rec := callEmail(
			emailHandler(&fakeEmailStore{ready: true}, configuredResolution()),
			http.MethodGet, "/admin/email/administration", "",
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Settings       emailsettings.Settings `json:"settings"`
			Effective      emailsettings.Settings `json:"effective"`
			Sources        map[string]string      `json:"sources"`
			PasswordSet    bool                   `json:"password_set"`
			PasswordSource string                 `json:"password_source"`
			Configured     bool                   `json:"configured"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !got.PasswordSet || got.PasswordSource != emailsettings.SourceDatabase {
			t.Errorf("password_set=%v source=%q", got.PasswordSet, got.PasswordSource)
		}
		if !got.Configured {
			t.Error("a resolvable relay was reported as not configured")
		}
		// The STORED layer is what the operator typed; the EFFECTIVE one is
		// what the next message uses. A page that showed only one of them
		// cannot tell an unset field from an inherited one.
		if got.Settings.Host != "smtp.acme.example" || got.Effective.From != "noreply@acme.example" {
			t.Errorf("layers = %+v / %+v", got.Settings, got.Effective)
		}
		if got.Sources["from"] != emailsettings.SourceEnvironment {
			t.Errorf("source[from] = %q; a field only the environment states must say so", got.Sources["from"])
		}
		if got.Sources["host"] != emailsettings.SourceDatabase {
			t.Errorf("source[host] = %q", got.Sources["host"])
		}
		// There is no password field of any name in this body.
		if strings.Contains(strings.ToLower(rec.Body.String()), "password\":\"") {
			t.Errorf("a password value appears in the response: %s", rec.Body.String())
		}
	})

	t.Run("a fresh install with no sealed password reports password_set false", func(t *testing.T) {
		// The FRESH-INSTALL shape: the operator has typed a relay that needs
		// no authentication, so the global vault holds nothing and was never
		// created. `emailsettings.Store` now reads that absence as "no
		// password" rather than as a read failure, so the resolution reaching
		// this handler is complete, and the page must report it plainly: a
		// relay that is configured, and a password that is not set.
		resolution := configuredResolution()
		resolution.Stored.Username = ""
		resolution.Effective.Username = ""
		resolution.PasswordSet = false
		resolution.PasswordSource = emailsettings.SourceUnset

		rec := callEmail(
			emailHandler(&fakeEmailStore{ready: true}, resolution),
			http.MethodGet, "/admin/email/administration", "",
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Settings       emailsettings.Settings `json:"settings"`
			PasswordSet    bool                   `json:"password_set"`
			PasswordSource string                 `json:"password_source"`
			Configured     bool                   `json:"configured"`
			Reason         string                 `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.PasswordSet {
			t.Error("password_set is true on a deployment that has sealed nothing")
		}
		if got.PasswordSource != emailsettings.SourceUnset {
			t.Errorf("password_source = %q, want %q", got.PasswordSource, emailsettings.SourceUnset)
		}
		// The absent credential must not read as an unusable deployment: the
		// stored host is still the host the next message uses.
		if !got.Configured || got.Reason != "" {
			t.Errorf("configured=%v reason=%q", got.Configured, got.Reason)
		}
		if got.Settings.Host != "smtp.acme.example" {
			t.Errorf("the stored layer was discarded: %+v", got.Settings)
		}
	})

	t.Run("503 without a store or a resolver", func(t *testing.T) {
		for name, handler := range map[string]*admin.Handler{
			"nothing wired": admin.NewHandler(nil),
			"store not ready": emailHandler(
				&fakeEmailStore{ready: false}, configuredResolution()),
			"nil halves": admin.NewHandler(nil, admin.WithEmailSettings(nil, nil)),
		} {
			rec := callEmail(handler, http.MethodGet, "/admin/email/administration", "")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s: status %d body %s", name, rec.Code, rec.Body.String())
			}
		}
	})
}

func TestEmailSettingsSave(t *testing.T) {
	t.Run("an omitted password leaves the sealed one alone", func(t *testing.T) {
		store := &fakeEmailStore{ready: true}
		rec := callEmail(emailHandler(store, configuredResolution()), http.MethodPut,
			"/admin/email/administration",
			`{"host":"smtp.acme.example","port":587,"tls":"starttls","from":"noreply@acme.example",
			  "public_base_url":"https://ai.acme.example"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		if len(store.saves) != 1 {
			t.Fatalf("saves = %d", len(store.saves))
		}
		if store.saves[0].password != nil {
			t.Fatalf("an omitted password reached the store as %q; the sealed value would have been "+
				"overwritten on every unrelated edit", *store.saves[0].password)
		}
		if store.saves[0].settings.Host != "smtp.acme.example" || store.saves[0].settings.Port != 587 {
			t.Errorf("settings = %+v", store.saves[0].settings)
		}
	})

	t.Run("an empty password clears it and a value re-seals it", func(t *testing.T) {
		for body, want := range map[string]string{
			`{"host":"h","password":""}`:                        "",
			`{"host":"h","password":"` + fixturePassword + `"}`: fixturePassword,
		} {
			store := &fakeEmailStore{ready: true}
			rec := callEmail(emailHandler(store, configuredResolution()), http.MethodPut,
				"/admin/email/administration", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			if len(store.saves) != 1 || store.saves[0].password == nil {
				t.Fatalf("%s: the password state was lost on the way to the store", body)
			}
			if *store.saves[0].password != want {
				t.Errorf("%s: password = %q, want %q", body, *store.saves[0].password, want)
			}
			// Even the request that CARRIED a password must not get one back.
			if strings.Contains(rec.Body.String(), fixturePassword) {
				t.Errorf("the response echoed the password: %s", rec.Body.String())
			}
		}
	})

	t.Run("a refused value answers 400 and names the field", func(t *testing.T) {
		store := &fakeEmailStore{ready: true, err: emailsettings.FieldError{
			Field: "port", Reason: "the port must be between 1 and 65535",
		}}
		rec := callEmail(emailHandler(store, configuredResolution()), http.MethodPut,
			"/admin/email/administration", `{"host":"h","port":70000}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		var got struct{ Error, Field string }
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got.Field != "port" || got.Error == "" {
			t.Errorf("body = %s; a refusal with no field sends the operator back to a form of eight controls",
				rec.Body.String())
		}
	})

	t.Run("a store failure is 503, not 400", func(t *testing.T) {
		// The distinction matters: 400 tells the operator to change what they
		// typed, and there is nothing wrong with what they typed.
		store := &fakeEmailStore{ready: true, err: errors.New("connection refused")}
		rec := callEmail(emailHandler(store, configuredResolution()), http.MethodPut,
			"/admin/email/administration", `{"host":"h"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "connection refused") {
			t.Error("the internal cause crossed the trust boundary")
		}
	})

	t.Run("an invalid body writes nothing", func(t *testing.T) {
		store := &fakeEmailStore{ready: true}
		rec := callEmail(emailHandler(store, configuredResolution()), http.MethodPut,
			"/admin/email/administration", "not json")
		if rec.Code != http.StatusBadRequest || len(store.saves) != 0 {
			t.Fatalf("status %d saves %d", rec.Code, len(store.saves))
		}
	})
}

func TestEmailTestSend(t *testing.T) {
	t.Run("sends through the same composer as every other message", func(t *testing.T) {
		mail := &fakeMailer{configured: true}
		handler := emailHandler(&fakeEmailStore{ready: true}, configuredResolution(), admin.WithMailer(mail))
		rec := callEmail(handler, http.MethodPost, "/admin/email/test/administration", `{"to":"ops@acme.example"}`)
		if rec.Code != http.StatusOK || len(mail.sent) != 1 || mail.sent[0] != "ops@acme.example" {
			t.Fatalf("status %d sent %v body %s", rec.Code, mail.sent, rec.Body.String())
		}
	})

	t.Run("an unconfigured relay answers 503 with the RESOLVER's reason", func(t *testing.T) {
		// The generic sentence is true of every incomplete document and
		// actionable for none of them. When the resolver knows which field is
		// missing, that is what the operator must be shown.
		resolution := emailsettings.Resolution{
			Configured: false,
			Reason:     "a sender address is required before e-mail can be sent: set From on Admin › E-mail, or EMAIL_FROM",
		}
		handler := emailHandler(&fakeEmailStore{ready: true}, resolution, admin.WithMailer(&fakeMailer{configured: false}))
		rec := callEmail(handler, http.MethodPost, "/admin/email/test/administration", `{"to":"ops@acme.example"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "sender address is required") {
			t.Errorf("body = %s; the specific reason was replaced by a generic one", rec.Body.String())
		}
	})
}
