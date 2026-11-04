// Package webpush sends content-free web push notifications (ADR-0045
// decision 6, #1437).
//
// Each install makes one VAPID key pair at first boot and keeps the private
// key in the global vault's hidden bucket (KeyVault). No key is shared
// between installs. A person opts in per browser; the browser's subscription
// lands in centry.push_subscriptions (at most 10 per person).
//
// The Dispatcher reads new run_finished and agent_approval_required
// notification rows through a PostgreSQL outbox (a cursor over
// centry.notifications.id), adds one delivery per subscription, and sends
// each one encrypted (RFC 8291, aes128gcm) with a VAPID token (RFC 8292).
// One replica sends at a time. A 404 or 410 deletes the subscription; any
// other failure retries with backoff until the 24-hour TTL ends.
package webpush

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"
)

// VAPIDSecretName is the name of the private key in the global vault's
// hidden bucket. The hidden bucket is never merged into a project's
// {{secret.name}} lookups, so no agent can read it.
const VAPIDSecretName = "web_push_vapid_private_key"

// KeyVault keeps the private key (secrets.Handler).
type KeyVault interface {
	// EnsureAdminHiddenSecret answers the stored value, or stores the one
	// generate makes when there is none. Replicas that race agree on one
	// value.
	EnsureAdminHiddenSecret(ctx context.Context, name string, generate func() (string, error)) (string, error)
}

// keyRetry is how long the key source waits after a failed vault read
// before it tries again.
const keyRetry = time.Minute

// KeySource loads the install's key pair from the vault, once, and keeps it.
// A vault that is missing or fails leaves push disabled; it never stops the
// boot. A failed load is tried again after keyRetry.
type KeySource struct {
	vault  KeyVault
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	keys     Keys
	loaded   bool
	failedAt time.Time
}

// NewKeySource builds the key source. A nil vault answers no key.
func NewKeySource(vault KeyVault, logger *slog.Logger) *KeySource {
	if logger == nil {
		logger = slog.Default()
	}
	return &KeySource{vault: vault, logger: logger, now: time.Now}
}

// Keys answers the key pair, making and storing it the first time. false:
// no key (no vault, or the vault failed recently).
func (s *KeySource) Keys(ctx context.Context) (Keys, bool) {
	if s == nil || s.vault == nil {
		return Keys{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return s.keys, true
	}
	if !s.failedAt.IsZero() && s.now().Sub(s.failedAt) < keyRetry {
		return Keys{}, false
	}
	stored, err := s.vault.EnsureAdminHiddenSecret(ctx, VAPIDSecretName, func() (string, error) {
		generated, err := GenerateKeys()
		if err != nil {
			return "", err
		}
		return generated.EncodePrivateKey()
	})
	if err == nil {
		var keys Keys
		if keys, err = ParsePrivateKey(stored); err == nil {
			s.keys, s.loaded = keys, true
			return keys, true
		}
	}
	s.failedAt = s.now()
	s.logger.WarnContext(ctx, "web push: the VAPID key is not available; web push stays off", "err", err)
	return Keys{}, false
}

// Settings reads the admin switch notifications.web_push.
type Settings interface {
	WebPushEnabled(ctx context.Context) bool
}

// SettingsFunc adapts a function to Settings.
type SettingsFunc func(ctx context.Context) bool

// WebPushEnabled implements Settings.
func (f SettingsFunc) WebPushEnabled(ctx context.Context) bool { return f(ctx) }

// Service answers the push configuration and holds what the dispatcher
// needs to sign a send.
type Service struct {
	keys     *KeySource
	settings Settings
	subject  string
}

// NewService builds the service. subject is the VAPID `sub` claim
// (Subject).
func NewService(keys *KeySource, settings Settings, subject string) *Service {
	return &Service{keys: keys, settings: settings, subject: subject}
}

// ErrNoSubject is a configuration without a contact for the push services.
var ErrNoSubject = errors.New("web push: set ELITEA_WEB_PUSH_SUBJECT (mailto: or https:) or PUBLIC_BASE_URL (https)")

// Subject picks the VAPID subject: the explicit one (a mailto: or https:
// URL), else the install's public URL when it is https. Push services
// refuse a token without a contact, so neither means no push.
func Subject(explicit, publicBaseURL string) (string, error) {
	if value := strings.TrimSpace(explicit); value != "" {
		if strings.HasPrefix(value, "mailto:") && len(value) > len("mailto:") && strings.Contains(value, "@") {
			return value, nil
		}
		if parsed, err := url.Parse(value); err == nil && parsed.Scheme == "https" && parsed.Host != "" {
			return value, nil
		}
		return "", ErrNoSubject
	}
	if parsed, err := url.Parse(strings.TrimSpace(publicBaseURL)); err == nil && parsed.Scheme == "https" &&
		parsed.Host != "" {
		return "https://" + parsed.Host, nil
	}
	return "", ErrNoSubject
}

// Config is what push_config answers: whether a browser can opt in, and the
// key it subscribes with. Enabled is false when the admin switch is off,
// when there is no key, or when there is no subject. PublicKey is set
// whenever the key exists.
type Config struct {
	Enabled   bool
	PublicKey string
}

// Config answers the push configuration.
func (s *Service) Config(ctx context.Context) Config {
	if s == nil {
		return Config{}
	}
	keys, ok := s.keys.Keys(ctx)
	if !ok {
		return Config{}
	}
	enabled := s.subject != "" && (s.settings == nil || s.settings.WebPushEnabled(ctx))
	return Config{Enabled: enabled, PublicKey: keys.Public}
}

// sender answers the keys and subject of a pass, or false when push is off.
func (s *Service) sender(ctx context.Context) (Keys, string, bool) {
	if s == nil || s.subject == "" || (s.settings != nil && !s.settings.WebPushEnabled(ctx)) {
		return Keys{}, "", false
	}
	keys, ok := s.keys.Keys(ctx)
	return keys, s.subject, ok
}
