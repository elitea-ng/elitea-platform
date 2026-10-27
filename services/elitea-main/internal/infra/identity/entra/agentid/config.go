package agentid

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

const (
	// DefaultAuthority is the Microsoft identity platform host.
	DefaultAuthority = "https://login.microsoftonline.com"
	// DefaultGraphBaseURL is Microsoft Graph.
	DefaultGraphBaseURL = "https://graph.microsoft.com"
	// DefaultIdentityLimit is Entra's app-only creation cap per blueprint.
	DefaultIdentityLimit = 250
	// DefaultDisplayNamePrefix names a created agent identity:
	// "<prefix><application id>".
	DefaultDisplayNamePrefix = "Elitea agent "
	defaultAgentCacheSize    = 1024
	maxAllowedScopes         = 64
	maxSponsorMappings       = 4096
	maxTextLen               = 512
)

var guidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Config is the `entra.agentid` operator block: one blueprint per trust
// boundary. It lives in operator configuration, never in the database.
type Config struct {
	// TenantID is the blueprint's tenant, a lower-case GUID. The
	// multi-tenant aliases common, organizations and consumers are refused.
	TenantID string
	// BlueprintAppID is the agent identity blueprint's application id.
	BlueprintAppID string
	// FederatedTokenFile is the projected service account token the
	// blueprint authenticates with (audience api://AzureADTokenExchange).
	// It is re-read on every token request, so rotation needs no restart.
	FederatedTokenFile string
	// Authority is the token endpoint host (DefaultAuthority when empty).
	Authority string
	// GraphBaseURL is the Microsoft Graph host (DefaultGraphBaseURL).
	GraphBaseURL string
	// AllowedResourceScopes are the only scopes AssumeApp and
	// AssumeOnBehalfOf may ask for, for example
	// "https://graph.microsoft.com/.default".
	AllowedResourceScopes []string
	// SponsorGroups maps a tenant project id to the Entra group that
	// sponsors its agent identities; DefaultSponsorGroupID covers the rest.
	SponsorGroups         map[int64]string
	DefaultSponsorGroupID string
	// IdentityLimit refuses Provision once the blueprint holds this many
	// agent identities (DefaultIdentityLimit when zero).
	IdentityLimit     int
	DisplayNamePrefix string
	// AssertionAudiences are the accepted user assertion audiences for
	// Agent OBO. Empty means api://<BlueprintAppID>/access_agent and
	// BlueprintAppID.
	AssertionAudiences []string
	// AgentCacheSize bounds the per-agent client cache (1024 when zero).
	AgentCacheSize int

	// ClientSecret and CertificateFile are never accepted. They exist so a
	// configured secret or certificate refuses boot instead of being
	// silently ignored: the governed profile authenticates the blueprint
	// with a federated credential only.
	ClientSecret    string
	CertificateFile string

	// Sessions reads the agent session a credential call belongs to.
	Sessions SessionReader
	// Delegations is the delegation custody (#1117). Nil denies every OBO.
	Delegations DelegationCustody
	HTTPClient  *http.Client
	Logger      *slog.Logger
	Now         func() time.Time
}

// ErrInvalidConfiguration refuses a configuration. Boot fails on it.
var ErrInvalidConfiguration = errors.New("entra agent id: invalid configuration")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfiguration, fmt.Sprintf(format, args...))
}

// Validate checks the operator block. It reads no file and dials nothing.
func (c Config) Validate() error {
	switch strings.ToLower(strings.TrimSpace(c.TenantID)) {
	case "common", "organizations", "consumers":
		return invalid("tenant_id %q is a multi-tenant alias; name the blueprint's tenant GUID", c.TenantID)
	}
	switch {
	case !obo.ValidTenantID(c.TenantID):
		return invalid("tenant_id must be a lower-case tenant GUID")
	case !guidPattern.MatchString(c.BlueprintAppID):
		return invalid("blueprint_app_id must be a lower-case GUID")
	case c.ClientSecret != "":
		return invalid("a client secret is refused in the governed profile; use federated_token_file")
	case c.CertificateFile != "":
		return invalid("a certificate is a development-profile credential; the governed profile takes federated_token_file only")
	case !validText(c.FederatedTokenFile):
		return invalid("federated_token_file is required")
	case c.IdentityLimit < 0:
		return invalid("identity_limit must not be negative")
	case len(c.DisplayNamePrefix) > 64:
		return invalid("display_name_prefix must be at most 64 bytes")
	case c.AgentCacheSize < 0:
		return invalid("agent cache size must not be negative")
	case c.Sessions == nil:
		return invalid("the agent session reader is required")
	}
	for _, raw := range []string{c.authority(), c.graphBaseURL()} {
		if err := checkHostURL(raw); err != nil {
			return err
		}
	}
	if len(c.AllowedResourceScopes) == 0 || len(c.AllowedResourceScopes) > maxAllowedScopes {
		return invalid("allowed_resource_scopes needs 1..%d scopes", maxAllowedScopes)
	}
	for _, scope := range c.AllowedResourceScopes {
		if _, ok := resourceOf(scope); !ok {
			return invalid("allowed resource scope %q must be <resource>/<scope>", scope)
		}
	}
	if len(c.SponsorGroups) > maxSponsorMappings {
		return invalid("at most %d sponsor group mappings", maxSponsorMappings)
	}
	for project, group := range c.SponsorGroups {
		if project <= 0 || !guidPattern.MatchString(group) {
			return invalid("sponsor group mapping %d=%q needs a positive project id and a group GUID", project, group)
		}
	}
	if c.DefaultSponsorGroupID != "" && !guidPattern.MatchString(c.DefaultSponsorGroupID) {
		return invalid("default sponsor group must be a group GUID")
	}
	for _, audience := range c.AssertionAudiences {
		if !validText(audience) {
			return invalid("assertion audience %q is not valid", audience)
		}
	}
	return nil
}

func checkHostURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return invalid("%q must be an https host URL without a path", raw)
	}
	return nil
}

func (c Config) authority() string {
	if c.Authority == "" {
		return DefaultAuthority
	}
	return strings.TrimSuffix(c.Authority, "/")
}

func (c Config) graphBaseURL() string {
	if c.GraphBaseURL == "" {
		return DefaultGraphBaseURL
	}
	return strings.TrimSuffix(c.GraphBaseURL, "/")
}

func (c Config) assertionAudiences() []string {
	if len(c.AssertionAudiences) > 0 {
		return c.AssertionAudiences
	}
	return []string{"api://" + c.BlueprintAppID + "/access_agent", c.BlueprintAppID}
}

// resourceOf splits "<resource>/<scope>" at the last slash.
func resourceOf(scope string) (string, bool) {
	if !validText(scope) {
		return "", false
	}
	i := strings.LastIndex(scope, "/")
	if i <= 0 || i == len(scope)-1 {
		return "", false
	}
	resource := scope[:i]
	if strings.HasSuffix(resource, ":/") || strings.HasSuffix(resource, ":") {
		return "", false
	}
	return resource, true
}

func validText(value string) bool {
	return value != "" && len(value) <= maxTextLen && !strings.ContainsFunc(value, unicode.IsSpace) &&
		!strings.ContainsFunc(value, unicode.IsControl)
}
