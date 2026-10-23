// Package facade carries the parts of a provider facade that are the SAME for
// every provider, extracted with TWO REAL CALLERS in hand.
//
// WHY NOW AND NOT IN P1.2. ADR-0012 deferred exactly these — `guard()`,
// `validProjectID`, the env-prefix-driven config — to the phase that mounts a
// second facade, on the ground that shaping a generic from one example is how
// "internal convenience" gets renamed. That phase is this one: DeepWiki and
// Inventory both call everything below.
//
// WHAT IS STILL NOT HERE, and stays per provider:
//
//   - the ENV PREFIX and the permission strings. A provider's variables and
//     its grants are what a deployment configures it with, and sharing them
//     would make two providers impossible to configure differently.
//   - anything about what a provider DOES. DeepWiki resolves repository
//     credentials, checks a git-host allowlist and mints a callback token
//     before it forwards an invoke; Inventory does none of that. Pulling that
//     in would produce a "generic" facade that carries one provider's
//     features and refuses the other's.
package facade

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// ErrIncompleteConfig reports a facade that cannot be composed from its
// environment.
var ErrIncompleteConfig = errors.New("incomplete provider facade configuration")

// Config is the transport half of a provider facade: how to reach the peer and
// how to prove who is calling. Everything a provider does with a request is
// outside it.
type Config struct {
	Enabled bool
	// Target is the provider service's gRPC listener, host:port (#1359): the
	// sub-application host serves the provider SPI and the platform
	// operations on it, behind one TLS configuration.
	Target         string
	ClientCertFile string
	ClientKeyFile  string
	CAFile         string
	ServerName     string
	IdentitySecret string
	Timeout        time.Duration

	// Admission is the deployment's answer to "may this provider still be
	// invoked" (internal/providerhost/admission), asked on the invoke route.
	//
	// SUPPLIED BY THE COMPOSITION ROOT, never by ConfigFromEnv — which is why
	// it sits below the transport fields and why the env reader leaves it nil.
	// The gate needs a database pool and the public project id, neither of
	// which a facade has or should have. Nil is a deployment with no admission
	// plane, and forwards unchanged.
	Admission AdmissionHook
}

// EnvNames is one provider's variable names.
//
// Passed in rather than built from a prefix. A prefix-driven scheme reads more
// cleanly and hides the one thing a reader needs: which literal string an
// operator has to set. Every name here appears in a chart and in an
// env-drift allowlist, and both are searched by the literal.
type EnvNames struct {
	Enabled string
	Target  string
	// RetiredBaseURL is the HTTPS origin the facade proxied to before #1359.
	// Set, it is refused: the provider no longer serves HTTP, and a facade
	// that read the old value as nothing would leave an operator guessing.
	RetiredBaseURL string
	ClientCertFile string
	ClientKeyFile  string
	CAFile         string
	ServerName     string
	IdentitySecret string
	Timeout        string
}

// ConfigFromEnv reads the transport configuration.
//
// A DISABLED facade returns cleanly with Enabled false and nothing else
// validated. That is the shipped default, and demanding certificates from a
// deployment that has not turned the provider on would make every install fail
// on a component it does not run.
func ConfigFromEnv(names EnvNames, lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(key string) string {
		value, _ := lookup(key)
		return strings.TrimSpace(value)
	}

	enabled, err := parseBool(get(names.Enabled))
	if err != nil {
		return Config{}, fmt.Errorf("%w: %s must be true or false", ErrIncompleteConfig, names.Enabled)
	}
	if err := RefuseRetiredBaseURL(names.RetiredBaseURL, names.Target, lookup); err != nil {
		return Config{}, err
	}
	cfg := Config{Enabled: enabled}
	if !enabled {
		return cfg, nil
	}

	cfg.Target = get(names.Target)
	cfg.ClientCertFile = get(names.ClientCertFile)
	cfg.ClientKeyFile = get(names.ClientKeyFile)
	cfg.CAFile = get(names.CAFile)
	cfg.ServerName = get(names.ServerName)
	cfg.IdentitySecret = get(names.IdentitySecret)

	// Every required name is reported together. An operator configuring a new
	// provider otherwise learns about one missing variable per restart.
	missing := make([]string, 0, 5)
	for _, required := range []struct{ name, value string }{
		{names.Target, cfg.Target},
		{names.ClientCertFile, cfg.ClientCertFile},
		{names.ClientKeyFile, cfg.ClientKeyFile},
		{names.CAFile, cfg.CAFile},
		{names.IdentitySecret, cfg.IdentitySecret},
	} {
		if required.value == "" {
			missing = append(missing, required.name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("%w: %s must be set", ErrIncompleteConfig, strings.Join(missing, ", "))
	}

	timeout, err := parseSeconds(get(names.Timeout))
	if err != nil {
		return Config{}, fmt.Errorf("%w: %s must be a positive number of seconds", ErrIncompleteConfig, names.Timeout)
	}
	cfg.Timeout = timeout
	return cfg, nil
}

// RefuseRetiredBaseURL fails when the retired HTTPS origin variable is set,
// naming the gRPC target variable that replaced it (#1359). It refuses
// whether the facade is on or off: an operator who set it meant it.
func RefuseRetiredBaseURL(retired, target string, lookup func(string) (string, bool)) error {
	if retired == "" {
		return nil
	}
	if value, _ := lookup(retired); strings.TrimSpace(value) != "" {
		return fmt.Errorf("%w: %s is retired (#1359): the provider is reached over gRPC with mutual TLS; set %s to its listener's host:port and unset %s",
			ErrIncompleteConfig, retired, target, retired)
	}
	return nil
}

func parseBool(raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}

func parseSeconds(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0, errors.New("not a positive integer")
	}
	return time.Duration(seconds) * time.Second, nil
}

// ValidProjectID accepts only a positive decimal id.
//
// THE LEADING-ZERO RULE IS THE POINT, and it is why this is not a bare
// ParseInt. "007" parses to 7, so a caller could address one project by
// several spellings — and the value reaches a permission resolver, which would
// then be asked about a string the audit trail records differently from the
// project it resolved. CodeQL flagged the same class on the numeric narrowing
// in the invoke path (go/incorrect-integer-conversion).
func ValidProjectID(raw string) bool {
	if raw == "" || (strings.HasPrefix(raw, "0") && raw != "0") {
		return false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && value > 0
}

// ProjectFromPath reads the {project_id} segment and validates it.
func ProjectFromPath(r *http.Request) (string, bool) {
	projectID := chi.URLParam(r, "project_id")
	return projectID, ValidProjectID(projectID)
}

// Guard builds the middleware every provider route carries: authenticate, then
// resolve the caller's permissions FOR THE PROJECT IN THE PATH.
//
// THE ORDER IS LOAD-BEARING. Auth runs outermost, so an unauthenticated caller
// is refused before any permission is resolved — a permission check on an
// absent principal is a database round trip whose answer is never useful, and
// on some resolvers it is a lookup for the empty user.
func Guard(
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
	mode string,
	permission string,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		endpoint := apimw.RequireResolvedPermissionsForProject(
			permissions, mode, ProjectFromPath, permission,
		)(next)
		return apimw.Auth(authConfig)(endpoint)
	}
}

// Composable reports whether a facade has everything it needs to authenticate.
//
// The same nil check every route in this service makes, in one place: a route
// serves perfectly well without a validator, it simply does not authenticate,
// and that is invisible at runtime.
func Composable(authConfig apimw.AuthConfig, permissions auth.PermissionResolver) bool {
	// A Form session reader (production Form authentication) OR a token
	// validator (OIDC-only, where APPLICATION_SECRET_KEY-signed tokens are read
	// back by a LocalValidator): either authenticates a caller. The principal
	// validator is required in both — it is the check that turns a deactivated
	// user away, and a nil one serves without saying so.
	return authConfig.PrincipalValidator != nil &&
		(authConfig.FormSessions != nil || authConfig.Validator != nil) &&
		permissions != nil
}

// UserID is the caller's id for the signed identity headers.
//
// Empty rather than a placeholder when there is no principal: the signature
// covers this value, and inventing one would produce a signature the provider
// computes differently and rejects — a 401 whose cause is nowhere near the
// facade.
func UserID(r *http.Request) string {
	if user, ok := auth.UserFromContext(r.Context()); ok {
		return user.UserID
	}
	return ""
}
