// Package deepwiki is the facade in front of the DeepWiki provider service.
//
// ADR-0022 decision 5: elitea-main is the only door. The provider refuses
// non-mTLS traffic and strips any identity header a client supplies, so every
// caller reaches it through here — authenticated, permission-checked, and with
// an identity this service signs.
//
// The shape follows internal/llmproxy, which is the established pattern for an
// mTLS hop to an independently deployed Go service, and reuses its transport
// and signing helpers rather than re-deriving either. Reusing the signer
// matters more than it looks: the canonical string it produces is duplicated
// across three modules now (this one, the gateway, and the provider's Python
// verifier), and a fourth spelling of it would fail every request.
package deepwiki

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// Environment variables this facade reads. All are optional except the enable
// flag; a deployment that sets none simply does not serve DeepWiki.
const (
	EnabledEnv        = "ELITEA_DEEPWIKI_ENABLED"
	TargetEnv         = "ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET" // host:port (#1359); BASE_URL is refused
	ClientCertEnv     = "ELITEA_DEEPWIKI_CLIENT_CERT_FILE"
	ClientKeyEnv      = "ELITEA_DEEPWIKI_CLIENT_KEY_FILE"
	CAFileEnv         = "ELITEA_DEEPWIKI_CA_FILE"
	ServerNameEnv     = "ELITEA_DEEPWIKI_SERVER_NAME"
	IdentitySecretEnv = "ELITEA_DEEPWIKI_IDENTITY_SECRET"
	TimeoutEnv        = "ELITEA_DEEPWIKI_TIMEOUT_SECONDS"

	// RetiredCallbackBaseURLEnv was the platform origin the provider called
	// back to over HTTP. Retired (#1359 U5): the engine calls back over the
	// platform gRPC listener, and ConfigFromEnv refuses the variable.
	RetiredCallbackBaseURLEnv = "ELITEA_DEEPWIKI_CALLBACK_BASE_URL"

	// CallbackTokenTTLEnv is how long a minted callback bearer lives. It must
	// outlast the longest generation the deployment expects; a token that dies
	// mid-run makes the engine fail its artifact upload after doing all the
	// work.
	CallbackTokenTTLEnv = "ELITEA_DEEPWIKI_CALLBACK_TOKEN_TTL_MINUTES"
)

// ErrIncompleteConfig reports a deployment that enabled DeepWiki without
// giving the facade what it needs to reach the provider.
var ErrIncompleteConfig = errors.New("incomplete DeepWiki facade configuration")

// Config is what the facade needs to proxy.
type Config struct {
	Enabled bool

	// Target is the provider's gRPC listener, e.g. elitea-deepwiki:9443.
	Target string

	// mTLS material. The provider requires a client certificate, so these are
	// mandatory whenever the facade is enabled — see Validate.
	ClientCertFile string
	ClientKeyFile  string
	CAFile         string
	ServerName     string

	// IdentitySecret signs the identity headers the provider verifies. Empty
	// disables signing, which the provider also allows; the mTLS transport
	// still authenticates the hop. It is not a default worth relying on and
	// Validate says so.
	IdentitySecret string

	Timeout time.Duration

	// CallbackTokenTTL is the minted bearer's lifetime. Defaults to two hours
	// — long enough for a large repository's generation, short enough that a
	// leaked token is not a standing grant. The legacy flow's equivalent had
	// no expiry at all.
	CallbackTokenTTL time.Duration

	// GitEgress is the allowlist of hosts a repository may be cloned from,
	// checked BEFORE the vault is opened. Fail-closed: unset refuses
	// everything, which is the same choice the provider service makes.
	GitEgress GitEgressPolicy

	// Admission is the deployment's answer to "may this provider still be
	// invoked" (internal/providerhost/admission), asked on the invoke route.
	// Supplied by the composition root and never by ConfigFromEnv: the gate
	// needs a database pool and the public project id, and Validate has
	// nothing to say about it — nil is a deployment with no admission plane,
	// which forwards unchanged.
	Admission facade.AdmissionHook
}

// ConfigFromEnv reads the facade's configuration.
//
// Strict: a value that cannot be parsed is an error rather than a silent
// default, so a typo fails at startup instead of producing a facade that is
// quietly disabled or quietly unauthenticated.
func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}

	get := func(key string) string {
		value, _ := lookup(key)
		return strings.TrimSpace(value)
	}

	enabled, err := parseBool(get(EnabledEnv))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnabledEnv, err)
	}

	timeout := 30 * time.Second
	if raw := get(TimeoutEnv); raw != "" {
		seconds, convErr := strconv.Atoi(raw)
		if convErr != nil || seconds <= 0 {
			return Config{}, fmt.Errorf(
				"%s must be a positive integer, got %q", TimeoutEnv, raw)
		}
		timeout = time.Duration(seconds) * time.Second
	}

	callbackTTL := 2 * time.Hour
	if raw := get(CallbackTokenTTLEnv); raw != "" {
		minutes, convErr := strconv.Atoi(raw)
		if convErr != nil || minutes < 1 {
			return Config{}, fmt.Errorf(
				"%s must be a positive whole number of minutes, got %q",
				CallbackTokenTTLEnv, raw)
		}
		callbackTTL = time.Duration(minutes) * time.Minute
	}

	return Config{
			Enabled:        enabled,
			Target:         get(TargetEnv),
			ClientCertFile: get(ClientCertEnv),
			ClientKeyFile:  get(ClientKeyEnv),
			CAFile:         get(CAFileEnv),
			ServerName:     get(ServerNameEnv),
			IdentitySecret: get(IdentitySecretEnv),
			Timeout:        timeout,

			CallbackTokenTTL: callbackTTL,
			GitEgress:        ParseGitEgressPolicy(get(GitAllowlistEnv)),
		}, errors.Join(
			facade.RefuseRetiredBaseURL("ELITEA_DEEPWIKI_BASE_URL", TargetEnv, lookup),
			material.RetiredCallbackBaseURL(RetiredCallbackBaseURLEnv, lookup))
}

// Validate reports whether an ENABLED config can actually reach the provider.
//
// A disabled facade needs nothing. An enabled one that cannot present a client
// certificate would be refused by the provider on every request, so it is
// caught here, at composition, rather than as a 503 per call.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	var missing []string
	if c.Target == "" {
		missing = append(missing, TargetEnv)
	}
	// The provider terminates mTLS with CERT_REQUIRED. Without all three of
	// these the hop cannot be established at all.
	if c.ClientCertFile == "" {
		missing = append(missing, ClientCertEnv)
	}
	if c.ClientKeyFile == "" {
		missing = append(missing, ClientKeyEnv)
	}
	if c.CAFile == "" {
		missing = append(missing, CAFileEnv)
	}

	if len(missing) > 0 {
		return fmt.Errorf("%w: %s is set but %s %s not",
			ErrIncompleteConfig, EnabledEnv,
			strings.Join(missing, ", "),
			pluralIsAre(len(missing)))
	}
	return nil
}

func pluralIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// parseBool accepts the same spellings the rest of elitea-main's runtime
// configuration does, and refuses anything else rather than reading it as
// false — a misspelled "ture" must not silently disable a feature someone
// believes they turned on.
func parseBool(raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	default:
		return false, fmt.Errorf("must be a boolean, got %q", raw)
	}
}
