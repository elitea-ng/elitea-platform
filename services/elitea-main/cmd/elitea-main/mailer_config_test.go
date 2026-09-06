package main

// The ENVIRONMENT layer of outbound e-mail.
//
// What this file asserts changed with gap G7, and the change is the point. It
// used to assert that a HALF-configured environment refuses to boot — SMTP_HOST
// without EMAIL_FROM, EMAIL_FROM without SMTP_HOST. Both are legitimate now:
// the environment is a bootstrap default that the admin E-mail page completes
// field by field, and completeness is decided on the merged document at send
// time (`emailsettings.transportConfig`), where it can name the missing field.
//
// So the cases below split into two groups, and the split is the contract:
//
//   - a value that is WRONG in any layer (a port that is not a number, an
//     address that is not an address, a TLS mode that does not exist, a user
//     name with no password) still refuses to boot. A chart carrying one of
//     those should fail at start-up, not at the first invitation;
//   - a value that is merely MISSING is carried as absent, because the layer
//     above may supply it.

import (
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/emailsettings"
)

func lookupOf(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

func TestMailerConfigFromEnv(t *testing.T) {
	t.Run("unset is incomplete and keeps the public URL", func(t *testing.T) {
		got, err := mailerConfigFromEnv(lookupOf(map[string]string{"DEPLOYMENT_URL": "https://ai.acme.example"}))
		if err != nil || got.Complete() || got.Settings.PublicBaseURL != "https://ai.acme.example" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("complete", func(t *testing.T) {
		got, err := mailerConfigFromEnv(lookupOf(map[string]string{
			"SMTP_HOST": "smtp.acme.example", "SMTP_PORT": "465", "SMTP_TLS": "implicit",
			"SMTP_USERNAME": "u", "SMTP_PASSWORD": "p",
			"EMAIL_FROM": "Acme <noreply@acme.example>", "EMAIL_REPLY_TO": "help@acme.example",
			"PUBLIC_BASE_URL": "https://ai.acme.example/",
		}))
		if err != nil || !got.Complete() {
			t.Fatalf("got %+v, %v", got, err)
		}
		// `implicit` is the transport's word and `tls` is this surface's; the
		// environment keeps accepting both and the layer stores the canonical
		// one, so the admin page and the chart cannot disagree about port 465.
		if got.Settings.Port != 465 || got.Settings.TLS != emailsettings.TLSImplicit ||
			got.Settings.From != "Acme <noreply@acme.example>" || got.Settings.ReplyTo != "help@acme.example" ||
			got.Settings.Username != "u" || got.Password != "p" {
			t.Fatalf("settings = %+v password=%q", got.Settings, got.Password)
		}
		// The trailing slash is trimmed here, so a link built from this origin
		// cannot come out with a doubled separator.
		if got.Settings.PublicBaseURL != "https://ai.acme.example" {
			t.Fatalf("base URL = %q", got.Settings.PublicBaseURL)
		}
	})
	t.Run("suppressed", func(t *testing.T) {
		got, err := mailerConfigFromEnv(lookupOf(map[string]string{
			"SMTP_HOST": "h", "EMAIL_FROM": "n@e.example", "DEPLOYMENT_URL": "https://a.example",
			"ELITEA_EMAIL_SUPPRESS": "true",
		}))
		if err != nil || !got.Suppressed || !got.Complete() {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("an unstated port and TLS mode stay unstated", func(t *testing.T) {
		// The DEFAULTS are not applied here any more. They belong to the
		// resolver, which applies them to the MERGED document: baking 587 in
		// at this layer would make the environment state a port the admin page
		// could never override, since an override is "the database states it"
		// and this would have made the environment state it too.
		got, err := mailerConfigFromEnv(lookupOf(map[string]string{
			"SMTP_HOST": "smtp.acme.example", "EMAIL_FROM": "noreply@acme.example",
			"DEPLOYMENT_URL": "https://a.example",
		}))
		if err != nil || got.Settings.Port != 0 || got.Settings.TLS != "" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	// A MISSING value is carried, never refused: the database layer above may
	// supply it, and these are exactly the shapes an operator configuring the
	// relay from the admin page leaves behind in the chart.
	for name, env := range map[string]map[string]string{
		"host without from":     {"SMTP_HOST": "h", "DEPLOYMENT_URL": "https://a.example"},
		"host without base url": {"SMTP_HOST": "h", "EMAIL_FROM": "n@e.example"},
		"from without host":     {"EMAIL_FROM": "n@e.example", "DEPLOYMENT_URL": "https://a.example"},
		"reply-to alone":        {"EMAIL_REPLY_TO": "help@e.example"},
		"port alone":            {"SMTP_PORT": "2525"},
	} {
		t.Run("accepted: "+name, func(t *testing.T) {
			got, err := mailerConfigFromEnv(lookupOf(env))
			if err != nil {
				t.Fatalf("refused a partial environment layer: %v", err)
			}
			if got.Complete() {
				t.Fatalf("a partial layer reported itself complete: %+v", got.Settings)
			}
		})
	}

	// A WRONG value still refuses to boot, in any layer.
	for name, env := range map[string]map[string]string{
		"port not a number":    {"SMTP_HOST": "h", "SMTP_PORT": "smtp"},
		"port out of range":    {"SMTP_HOST": "h", "SMTP_PORT": "70000"},
		"unknown tls":          {"SMTP_HOST": "h", "SMTP_TLS": "ssl3"},
		"username alone":       {"SMTP_HOST": "h", "SMTP_USERNAME": "u"},
		"password alone":       {"SMTP_PASSWORD": "p"},
		"bad from":             {"SMTP_HOST": "h", "EMAIL_FROM": "not an address"},
		"bad reply to":         {"SMTP_HOST": "h", "EMAIL_REPLY_TO": "not an address"},
		"relative base url":    {"PUBLIC_BASE_URL": "/elitea"},
		"suppress not boolean": {"ELITEA_EMAIL_SUPPRESS": "yes"},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			if _, err := mailerConfigFromEnv(lookupOf(env)); err == nil {
				t.Fatal("accepted")
			} else if !strings.Contains(err.Error(), "SMTP") && !strings.Contains(err.Error(), "EMAIL") &&
				!strings.Contains(err.Error(), "PUBLIC_BASE_URL") && !strings.Contains(err.Error(), "ELITEA_EMAIL") {
				t.Fatalf("error does not name the variable: %v", err)
			}
		})
	}
}
