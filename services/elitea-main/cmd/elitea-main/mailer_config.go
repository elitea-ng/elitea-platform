package main

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/emailsettings"
)

// mailerConfig is the ENVIRONMENT layer of outbound e-mail.
//
// # It used to be the whole configuration, and it is now the default
//
// ADR-0024 WP7 read these variables at boot and built the transport from them,
// which made the relay un-configurable without a redeploy and made e-mail
// unreachable for an operator with no access to the environment. Gap G7 moved
// the authority to `centry.platform_config` (`internal/emailsettings`), read
// per send. What survives here is the BOOTSTRAP DEFAULT: the layer a
// deployment ships in its chart, which the admin E-mail page overrides field
// by field.
//
// # Why the completeness refusals are gone
//
// This function used to refuse to boot when SMTP_HOST was set and EMAIL_FROM
// was not, and when any sibling was set while SMTP_HOST was not. Both refusals
// existed to stop a half-configured mailer from reporting deliveries it never
// made — the right goal, enforced in the wrong place once a second layer
// exists:
//
//   - "EMAIL_FROM without SMTP_HOST" is now the NORMAL shape of a deployment
//     that ships a sender address in its chart and lets an administrator name
//     the relay. Refusing it would have made the new page impossible to use on
//     exactly the deployments that need it;
//   - completeness is a property of the MERGED document, which this function
//     cannot see. `emailsettings.transportConfig` decides it, on every send,
//     and a merge that is not complete reports `Configured: false` with the
//     field to fix. Nothing is reported as delivered.
//
// Per-VALUE validation stays: a port that is not a number and an address that
// is not an address are wrong in any layer, and a chart that carries one
// should fail at boot rather than at the first invitation.
type mailerConfig struct {
	// Settings is the environment layer, under the database rows.
	Settings emailsettings.Settings
	// Password is SMTP_PASSWORD. The vault entry the E-mail page seals wins
	// over it.
	Password string
	// Suppressed (ELITEA_EMAIL_SUPPRESS=true) renders but never sends: the
	// setting for a shadow or staging deployment that must produce no
	// user-visible side effect (spec-security-verification §shadow).
	Suppressed bool
}

// Complete reports whether the environment ALONE names a usable relay. It
// drives one log line at boot; it does not gate anything, because the database
// layer can complete a partial environment and does so at run time.
func (c mailerConfig) Complete() bool {
	return c.Settings.Host != "" && c.Settings.From != "" && c.Settings.PublicBaseURL != ""
}

// mailerConfigFromEnv reads SMTP_HOST, SMTP_PORT, SMTP_USERNAME,
// SMTP_PASSWORD, SMTP_TLS, EMAIL_FROM, EMAIL_REPLY_TO and PUBLIC_BASE_URL
// (falling back to DEPLOYMENT_URL, which the chart already declares).
func mailerConfigFromEnv(lookup func(string) (string, bool)) (mailerConfig, error) {
	if lookup == nil {
		return mailerConfig{}, errors.New("mailer environment lookup is required")
	}
	get := func(name string) string {
		value, _ := lookup(name)
		return strings.TrimSpace(value)
	}

	config := mailerConfig{}
	switch get("ELITEA_EMAIL_SUPPRESS") {
	case "", "false":
	case "true":
		config.Suppressed = true
	default:
		return mailerConfig{}, errors.New("ELITEA_EMAIL_SUPPRESS must be true or false")
	}

	baseURL := get("PUBLIC_BASE_URL")
	if baseURL == "" {
		baseURL = get("DEPLOYMENT_URL")
	}
	if baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return mailerConfig{}, fmt.Errorf(
				"PUBLIC_BASE_URL must be an absolute http(s) origin, got %q", baseURL)
		}
	}
	config.Settings.PublicBaseURL = strings.TrimRight(baseURL, "/")
	config.Settings.Host = get("SMTP_HOST")

	if raw := get("SMTP_PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port <= 0 || port > 65535 {
			return mailerConfig{}, fmt.Errorf("SMTP_PORT must be a port number, got %q", raw)
		}
		config.Settings.Port = port
	}
	if raw := get("SMTP_TLS"); raw != "" {
		mode, ok := emailsettings.ParseTLS(raw)
		if !ok {
			return mailerConfig{}, fmt.Errorf("SMTP_TLS must be starttls, tls (implicit) or none, got %q", raw)
		}
		config.Settings.TLS = emailsettings.FormatTLS(mode)
	}
	config.Settings.Username = get("SMTP_USERNAME")
	config.Password = get("SMTP_PASSWORD")
	// The pair rule stays a BOOT refusal for the environment layer, because
	// both halves are in this layer and neither can be completed from the
	// other: a chart that ships one without the other is a chart that would
	// have its session refused at AUTH.
	if (config.Settings.Username == "") != (config.Password == "") {
		return mailerConfig{}, errors.New("SMTP_USERNAME and SMTP_PASSWORD must be set together")
	}

	if from := get("EMAIL_FROM"); from != "" {
		if _, err := mail.ParseAddress(from); err != nil {
			return mailerConfig{}, fmt.Errorf(
				"EMAIL_FROM must be an address such as noreply@example.com, got %q", from)
		}
		config.Settings.From = from
	}
	if replyTo := get("EMAIL_REPLY_TO"); replyTo != "" {
		if _, err := mail.ParseAddress(replyTo); err != nil {
			return mailerConfig{}, fmt.Errorf("EMAIL_REPLY_TO must be an address, got %q", replyTo)
		}
		config.Settings.ReplyTo = replyTo
	}
	return config, nil
}
