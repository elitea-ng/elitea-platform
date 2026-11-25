package adminaccess_test

// The notice webhook's sender without a database (ADR-0059 R2.5b, issue
// 2598): the URL is a credential, so no log line and no error names it,
// whatever the delivery's outcome; the configuration is refused at start
// rather than run half set.

import (
	"bytes"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

const webhookToken = "T0KEN-sEcReT-pAtH"

func TestTheWebhookURLIsInNoLogLine(t *testing.T) {
	secret := uuid.NewString()
	failing := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/"+webhookToken, http.StatusFound)
	}))
	defer redirecting.Close()
	ok := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ok.Close()

	roots := x509.NewCertPool()
	for _, server := range []*httptest.Server{failing, redirecting, ok} {
		roots.AddCert(server.Certificate())
	}
	for name, c := range map[string]struct {
		url, allowlist string
		delivered      bool
	}{
		"refused destination": {url: failing.URL, allowlist: ""},
		"receiver error":      {url: failing.URL, allowlist: "127.0.0.1/32"},
		"redirect":            {url: redirecting.URL, allowlist: "127.0.0.1/32"},
		"unreachable":         {url: "https://127.0.0.1:1", allowlist: "127.0.0.1/32"},
		"delivered":           {url: ok.URL, allowlist: "127.0.0.1/32", delivered: true},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			url := c.url + "/hooks/" + webhookToken + "?token=" + webhookToken
			webhook, err := adminaccess.NewNoticeWebhook(adminaccess.NoticeWebhookConfig{
				URL: url, Secret: secret, Allowlist: c.allowlist, RootCAs: roots,
			}, logger)
			if err != nil {
				t.Fatal(err)
			}
			noticeID := uuid.NewString()
			err = webhook.DeliverNotice(t.Context(), adminaccess.WebhookNotice{
				Type: string(adminaccess.NoticeActivation), ElevationID: uuid.NewString(), NoticeID: noticeID,
				Time: "2026-11-25T10:00:00Z", Level: adminaccess.ElevationLevelAdmin, Path: adminaccess.ElevationPathApproved,
			})
			if (err == nil) != c.delivered {
				t.Fatalf("delivered = %v, want %v (err %v)", err == nil, c.delivered, err)
			}
			text := logs.String()
			if err != nil {
				text += "\n" + err.Error()
			}
			if !strings.Contains(logs.String(), noticeID) {
				t.Fatalf("the log does not name the notice: %s", logs.String())
			}
			for _, part := range []string{webhookToken, strings.TrimPrefix(c.url, "https://"), url} {
				if strings.Contains(text, part) {
					t.Fatalf("the log or the error names %q:\n%s", part, text)
				}
			}
		})
	}
}

func TestTheWebhookConfigurationIsRefusedHalfSet(t *testing.T) {
	strong := uuid.NewString()
	for name, config := range map[string]adminaccess.NoticeWebhookConfig{
		"plain http":       {URL: "http://hooks.example/" + webhookToken, Secret: strong},
		"no host":          {URL: "https:///" + webhookToken, Secret: strong},
		"no secret":        {URL: "https://hooks.example/" + webhookToken},
		"weak secret":      {URL: "https://hooks.example/" + webhookToken, Secret: "changeme"},
		"bad allowlist":    {URL: "https://hooks.example/" + webhookToken, Secret: strong, Allowlist: "not a/host/"},
		"not a URL at all": {URL: "https://hooks.example/%zz" + webhookToken, Secret: strong},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := adminaccess.NewNoticeWebhook(config, nil)
			if err == nil {
				t.Fatal("the configuration was accepted")
			}
			if strings.Contains(err.Error(), webhookToken) || strings.Contains(err.Error(), "hooks.example") {
				t.Fatalf("the refusal names the URL: %v", err)
			}
		})
	}

	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	if _, set, err := adminaccess.NoticeWebhookConfigFromEnv(env(nil)); set || err != nil {
		t.Fatalf("unset environment: set %v, err %v", set, err)
	}
	if _, _, err := adminaccess.NoticeWebhookConfigFromEnv(env(map[string]string{
		adminaccess.NoticeWebhookSecretEnv: strong,
	})); err == nil {
		t.Fatal("a secret without a URL was accepted")
	}
	config, set, err := adminaccess.NoticeWebhookConfigFromEnv(env(map[string]string{
		adminaccess.NoticeWebhookURLEnv: " https://hooks.example/x ", adminaccess.NoticeWebhookSecretEnv: strong,
	}))
	if err != nil || !set || config.URL != "https://hooks.example/x" {
		t.Fatalf("set environment: %+v %v %v", config, set, err)
	}
}
