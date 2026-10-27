// Package vault is the HashiCorp Vault / OpenBao KV v2 SecretStore adapter.
//
// It is a small net/http client with no vendor SDK. Vault is a third-party
// API, so HTTPS is its transport; TLS is mandatory and verified against the
// operator's CA bundle only.
//
// Authorization arrangement C (ADR-0039 decision 9): the Elitea PDP decides,
// and Vault is workload-scoped storage. Each StoreContext.WorkloadScope logs in
// with its own role, RolePrefix+scope, because Vault policies are a union and
// isolation needs one role per scope.
package vault

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Environment variables. They are read only when the governed profile flag is
// on, and only by ConfigFromEnv.
const (
	EnvAddr           = "ELITEA_SECRETSTORE_VAULT_ADDR"
	EnvCAFile         = "ELITEA_SECRETSTORE_VAULT_CA_FILE"
	EnvAuthMethod     = "ELITEA_SECRETSTORE_VAULT_AUTH_METHOD"
	EnvAuthMount      = "ELITEA_SECRETSTORE_VAULT_AUTH_MOUNT"
	EnvRolePrefix     = "ELITEA_SECRETSTORE_VAULT_ROLE_PREFIX"
	EnvKVMount        = "ELITEA_SECRETSTORE_VAULT_KV_MOUNT"
	EnvNamespace      = "ELITEA_SECRETSTORE_VAULT_NAMESPACE"
	EnvTokenFile      = "ELITEA_SECRETSTORE_VAULT_TOKEN_FILE"
	EnvReadinessScope = "ELITEA_SECRETSTORE_VAULT_READINESS_SCOPE"
)

// EnvNames lists every variable, for "is anything configured" checks.
var EnvNames = []string{EnvAddr, EnvCAFile, EnvAuthMethod, EnvAuthMount, EnvRolePrefix, EnvKVMount, EnvNamespace, EnvTokenFile, EnvReadinessScope}

// AuthMethod is how elitea-main logs in to Vault.
type AuthMethod string

const (
	// AuthJWT is the JWT/OIDC auth method with a projected workload token.
	AuthJWT AuthMethod = "jwt"
	// AuthKubernetes is the Kubernetes auth method with the service account
	// token.
	AuthKubernetes AuthMethod = "kubernetes"
)

// ErrInvalidConfig refuses boot. Its text names the variable, never a value
// read from a file.
var ErrInvalidConfig = errors.New("secretstore vault configuration is invalid")

var (
	mountPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}(/[A-Za-z0-9][A-Za-z0-9_-]{0,63}){0,3}$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// Config is the validated operator configuration. There are no defaults for
// the address, the role prefix or anything else that selects authority.
type Config struct {
	Addr       *url.URL
	CAPEM      []byte
	AuthMethod AuthMethod
	AuthMount  string
	RolePrefix string
	KVMount    string
	// Namespace is the Vault Enterprise namespace header; empty for OpenBao
	// and Vault CE.
	Namespace string
	// TokenFile is the projected workload token, re-read on every login so a
	// rotated token is picked up.
	TokenFile string
	// ReadinessScope is the workload scope the boot check logs in as.
	ReadinessScope string
}

// Configured reports whether any variable is set. When none is, Vault is
// simply not configured; that is valid until a policy revision requires it.
func Configured(lookup func(string) (string, bool)) bool {
	for _, name := range EnvNames {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// ConfigFromEnv reads and validates the configuration. A partially set
// configuration is invalid: an operator who set one variable meant to
// configure Vault, and a silent half-configuration is never accepted.
func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("secretstore vault environment lookup is required")
	}
	get := func(name string) string {
		value, _ := lookup(name)
		return strings.TrimSpace(value)
	}
	var cfg Config

	rawAddr := get(EnvAddr)
	if rawAddr == "" {
		return Config{}, fmt.Errorf("%w: %s is required", ErrInvalidConfig, EnvAddr)
	}
	addr, err := url.Parse(rawAddr)
	if err != nil || addr.Host == "" || addr.User != nil || addr.RawQuery != "" || addr.Fragment != "" ||
		(addr.Path != "" && addr.Path != "/") {
		return Config{}, fmt.Errorf("%w: %s must be an https://host[:port] URL", ErrInvalidConfig, EnvAddr)
	}
	if addr.Scheme != "https" {
		return Config{}, fmt.Errorf("%w: %s must use https; TLS is mandatory", ErrInvalidConfig, EnvAddr)
	}
	addr.Path = ""
	cfg.Addr = addr

	caFile := get(EnvCAFile)
	if caFile == "" {
		return Config{}, fmt.Errorf("%w: %s is required", ErrInvalidConfig, EnvCAFile)
	}
	cfg.CAPEM, err = os.ReadFile(caFile) // #nosec G304 -- operator-configured path.
	if err != nil || len(cfg.CAPEM) == 0 {
		return Config{}, fmt.Errorf("%w: %s is not a readable file", ErrInvalidConfig, EnvCAFile)
	}

	switch AuthMethod(get(EnvAuthMethod)) {
	case AuthJWT:
		cfg.AuthMethod = AuthJWT
	case AuthKubernetes:
		cfg.AuthMethod = AuthKubernetes
	default:
		return Config{}, fmt.Errorf("%w: %s must be %q or %q", ErrInvalidConfig, EnvAuthMethod, AuthJWT, AuthKubernetes)
	}

	cfg.AuthMount = get(EnvAuthMount)
	if !mountPattern.MatchString(cfg.AuthMount) {
		return Config{}, fmt.Errorf("%w: %s is required and must be a mount path", ErrInvalidConfig, EnvAuthMount)
	}
	cfg.RolePrefix = get(EnvRolePrefix)
	if !prefixPattern.MatchString(cfg.RolePrefix) {
		return Config{}, fmt.Errorf("%w: %s is required (letters, digits, _ and -)", ErrInvalidConfig, EnvRolePrefix)
	}
	cfg.KVMount = get(EnvKVMount)
	if !mountPattern.MatchString(cfg.KVMount) {
		return Config{}, fmt.Errorf("%w: %s is required and must be a mount path", ErrInvalidConfig, EnvKVMount)
	}
	cfg.Namespace = get(EnvNamespace)
	if cfg.Namespace != "" && !mountPattern.MatchString(cfg.Namespace) {
		return Config{}, fmt.Errorf("%w: %s must be a namespace path", ErrInvalidConfig, EnvNamespace)
	}
	cfg.TokenFile = get(EnvTokenFile)
	if cfg.TokenFile == "" {
		return Config{}, fmt.Errorf("%w: %s is required", ErrInvalidConfig, EnvTokenFile)
	}
	if info, statErr := os.Stat(cfg.TokenFile); statErr != nil || info.IsDir() {
		return Config{}, fmt.Errorf("%w: %s is not a readable file", ErrInvalidConfig, EnvTokenFile)
	}
	cfg.ReadinessScope = get(EnvReadinessScope)
	if !validScope(cfg.ReadinessScope) {
		return Config{}, fmt.Errorf("%w: %s is required and must be a workload scope", ErrInvalidConfig, EnvReadinessScope)
	}
	return cfg, nil
}

// Role is the Vault role a workload scope logs in with.
func (c Config) Role(scope string) string { return c.RolePrefix + "-" + scope }
