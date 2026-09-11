package secrets

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultSecretPolicySection = "default_secrets"

type defaultSecretPolicy struct {
	names    map[string]struct{}
	suppress bool
}

// WithPlatformDefaultSecretPolicy shares the admin policy across REST and MCP.
func WithPlatformDefaultSecretPolicy(pool *pgxpool.Pool) Option {
	return func(h *Handler) {
		h.defaultSecretPolicy = func(ctx context.Context) (defaultSecretPolicy, error) {
			if pool == nil {
				return defaultSecretPolicy{}, errors.New("secret policy storage is unavailable")
			}
			values, err := platformconfig.Load(ctx, pool, DefaultSecretPolicySection)
			if err != nil {
				return defaultSecretPolicy{}, err
			}
			return parseDefaultSecretPolicy(values)
		}
	}
}

func parseDefaultSecretPolicy(values platformconfig.Values) (defaultSecretPolicy, error) {
	policy := defaultSecretPolicy{names: map[string]struct{}{}}
	if value, present := values["ignore_default_secret_api"]; present {
		flag, ok := value.(bool)
		if !ok {
			return policy, errors.New("invalid default secret policy")
		}
		policy.suppress = flag
	}
	if value, present := values["default_secret_keys"]; present {
		names, ok := value.([]any)
		if !ok || len(names) > 100 {
			return policy, errors.New("invalid default secret names")
		}
		for _, value := range names {
			name, ok := value.(string)
			if !ok || !acceptableSecretName(name) {
				return policy, errors.New("invalid default secret name")
			}
			policy.names[name] = struct{}{}
		}
	}
	return policy, nil
}

func (h *Handler) readDefaultSecretPolicy(w http.ResponseWriter, r *http.Request) (defaultSecretPolicy, bool) {
	if h.defaultSecretPolicy == nil {
		return defaultSecretPolicy{}, true
	}
	policy, err := h.defaultSecretPolicy(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "secret policy is unavailable"})
		return defaultSecretPolicy{}, false
	}
	return policy, true
}

func (p defaultSecretPolicy) isDefault(name string) bool {
	_, found := p.names[name]
	return found
}

func (p defaultSecretPolicy) suppressed(vault vaultData, r *http.Request) bool {
	if !p.suppress {
		return false
	}
	expected, found := vault.Secrets[SecretsHeaderValueName]
	if !found {
		expected, found = vault.HiddenSecrets[SecretsHeaderValueName]
	}
	supplied := r.Header.Get("X-SECRET")
	return !found || expected == "" || supplied == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) != 1
}

func (p defaultSecretPolicy) refuse(w http.ResponseWriter, r *http.Request, vault vaultData, names ...string) bool {
	if !p.suppressed(vault, r) {
		return false
	}
	for _, name := range names {
		if p.isDefault(name) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Default secrets API disabled"})
			return true
		}
	}
	return false
}

// ValidateDefaultSecretPolicy checks the same fields consumed by vault handlers.
func ValidateDefaultSecretPolicy(values map[string]any) error {
	_, err := parseDefaultSecretPolicy(platformconfig.Values(values))
	return err
}
