// Package account implements the bifrost/core schemas.Account interface for the
// elitea-llm-gateway.
//
// It is the sole credential coupling point between the gateway and bifrost/core
// (design §5.2, §6.1). Provider credentials are never minted or persisted by the
// gateway: the Fernet-encrypted vault (centry.secrets_*) remains the source of
// truth and per-project provider settings live in the p_{projectID}.configuration
// table (section 'ai_credentials'). Credentials are read per request, decrypted,
// and handed to core as schemas.Key values — raw key material is never logged,
// returned to callers, or otherwise surfaced.
//
// # Account interface shape (validated against core/schemas/account.go @ v1.7.3)
//
//	GetConfiguredProviders() ([]schemas.ModelProvider, error)
//	GetKeysForProvider(ctx context.Context, provider schemas.ModelProvider) ([]schemas.Key, error)
//	GetConfigForProvider(provider schemas.ModelProvider) (*schemas.ProviderConfig, error)
//
// Only GetKeysForProvider carries a context.Context. GetConfiguredProviders and
// GetConfigForProvider are invoked without one — at bifrost.Init to prewarm
// provider workers and lazily on first request for a provider (bifrost.go
// 317/356/4398). Per-project credential resolution therefore MUST happen in
// GetKeysForProvider: it reads the resolved projectID from the request context
// (BifrostContextKeyVirtualKey, set by the /llm handler from the signed
// X-Elitea-Project-Id header) and returns only that project's keys. The other
// two methods return a static supported-provider set and a tuned ProviderConfig;
// a project with no credential for a provider simply yields zero keys.
package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/hopmarker"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/overhead"
)

// SelfReferentialCredentialReason is the rejection reason emitted when a
// provider credential's api_base resolves to the platform's own /llm origin.
// This replicates the legacy tools/mappers/integration/open_ai.py guard
// (api_base == get_base_url()) and prevents a request routed through the
// gateway from looping straight back into it (spec §2.6 guard #1, design §4.3).
const SelfReferentialCredentialReason = "SELF_REFERENTIAL_CREDENTIAL"

// ErrSelfReferentialCredential is returned when a credential is rejected by the
// self-referential guard. It carries SelfReferentialCredentialReason so callers
// can map it to the HTTP 400 invalid_request_error response (spec §2.5).
var ErrSelfReferentialCredential = errors.New(SelfReferentialCredentialReason)

// IncompleteCredentialReason is the rejection reason emitted when a stored
// credential does not carry every field its provider needs.
//
// This exists because bifrost fails OPEN for one provider. core/utils.go
// validateKey substitutes an empty BedrockKeyConfig for a Bedrock key that has
// none, and the AWS SDK then authenticates with the ambient identity of the
// pod. A dropped tenant credential therefore becomes a request that AWS bills
// to, and authorises with, the platform's own role (issue #454). The gateway
// must refuse the credential before the key reaches core.
//
// The same rule is applied to every other provider whose key configuration is
// mandatory, so an incomplete credential always fails with one named reason
// instead of a provider-specific error much further downstream.
const IncompleteCredentialReason = "INCOMPLETE_PROVIDER_CREDENTIAL"

// ErrIncompleteCredential is returned when a credential is missing a field its
// provider key configuration needs. It carries IncompleteCredentialReason.
var ErrIncompleteCredential = errors.New(IncompleteCredentialReason)

// UnsupportedAPIBaseReason is the rejection reason emitted for an open_ai
// credential that names an api_base the gateway cannot honour.
//
// bifrost carries a per-key endpoint for Azure, Ollama and vLLM only
// (AzureKeyConfig.Endpoint, OllamaKeyConfig.URL, VLLMKeyConfig.URL). Its OpenAI
// provider takes its base URL from ProviderConfig.NetworkConfig.BaseURL, which
// GetConfigForProvider supplies once per process and without a context, so it
// cannot vary per project. An open_ai credential that names a third-party
// OpenAI-compatible endpoint therefore cannot be dispatched to that endpoint.
//
// Before this guard the api_base was simply dropped and the request went to
// api.openai.com WITH THE TENANT KEY (issue #452). That sends a secret to a
// host the tenant never named. The credential is now refused instead.
const UnsupportedAPIBaseReason = "UNSUPPORTED_CREDENTIAL_API_BASE"

// ErrUnsupportedAPIBase is returned when an open_ai credential names an api_base
// the OpenAI provider cannot be pointed at. It carries UnsupportedAPIBaseReason.
var ErrUnsupportedAPIBase = errors.New(UnsupportedAPIBaseReason)

// ContextKeyRequestModel carries the model name the /llm handler dispatches,
// written after the caller's model id is mapped onto the provider's own name
// and read here by GetKeysForProvider.
//
// It exists for the Azure api-version (issue #455). bifrost accepts a per-key
// api-version override in exactly one place: AzureAliasCfg.APIVersion inside
// Key.Aliases, which core resolves by the requested model name
// (bifrost.go: k.Aliases.ResolveConfig(originalModelRequested)). Building that
// alias needs the model, and the schemas.Account interface hands the account a
// context and a provider only — so the model has to arrive on the context.
//
// An absent value is not an error: the key is then built with no alias and
// bifrost applies its own default api-version, which is the behaviour before
// this change.
const ContextKeyRequestModel schemas.BifrostContextKey = "elitea-request-model"

// supportedProviders is the static set of providers the gateway can serve. It is
// intentionally not per-project: per-project availability is expressed by
// GetKeysForProvider returning zero keys for a provider a project has not
// configured. Keeping this static lets bifrost.Init prewarm a bounded, tuned
// worker pool per provider without a database round-trip at startup (§6.1).
//
// Each entry maps to a provider bifrost/core can construct at Init without a key
// (createBaseProvider, bifrost.go:4234 — keys are supplied per request).
var supportedProviders = []schemas.ModelProvider{
	schemas.OpenAI,
	schemas.Azure,
	schemas.Anthropic,
	schemas.Bedrock,
	schemas.Vertex,
	schemas.Ollama,
	schemas.VLLM,
}

// rowQuerier is the minimal pgx surface the account needs. It is satisfied by
// *pgxpool.Pool and by test fakes, keeping the package unit-testable without a
// live database (mirrors the elitea-main handler DB-abstraction idiom).
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgxRows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgxRow
}

// pgxRow mirrors pgx.Row (Scan-only).
type pgxRow interface {
	Scan(dest ...any) error
}

// pgxRows mirrors the subset of pgx.Rows the account consumes.
type pgxRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// vaultDecryptor resolves a project secret reference ({{secret.NAME}}) to its
// plaintext value by reading and decrypting the project's Fernet vault. It is an
// interface so tests can supply an in-memory implementation; the production
// implementation is *fernetVault (vault.go), which reads centry.secrets_*.
type vaultDecryptor interface {
	// Resolve returns the plaintext for secretRef within the given project. When
	// secretRef is not a {{secret.NAME}} reference it is returned verbatim.
	Resolve(ctx context.Context, projectID, secretRef string) (string, error)
}

// EliteaAccount implements schemas.Account backed by Elitea's Postgres store.
type EliteaAccount struct {
	db    rowQuerier
	vault vaultDecryptor

	// providerConcurrency caps the worker goroutines bifrost spawns per provider,
	// tuning ConcurrencyAndBufferSize.Concurrency down from the 1000-worker
	// default (design §9.5, §6.1).
	providerConcurrency int

	// selfOrigins holds the normalised origins of the platform's own /llm
	// surface. Any credential api_base whose origin matches one of these is
	// rejected by the self-referential guard.
	selfOrigins map[string]struct{}

	// egress is the merged allowlist for tenant-authored api_base hosts: the
	// GATEWAY_EGRESS_ALLOWLIST floor UNION the authored `egress_allowlist`
	// governance rows (issues #13, G6). Never nil after New.
	egress *egressGate

	// publicProjectID is the platform's shared project (issue #316). Empty
	// disables the shared scope. It is operator configuration and is never
	// taken from a request.
	publicProjectID string

	// hopMarker is the outbound half of hop-marker detection (issue #164).
	// nil = unarmed: no marker header is added to any provider request and the
	// gateway detects no loop. See GetConfigForProvider.
	hopMarker *hopmarker.Marker

	logger *slog.Logger
}

var _ schemas.Account = (*EliteaAccount)(nil)

// Config configures a new EliteaAccount.
type Config struct {
	// DB is the Postgres handle (*pgxpool.Pool in production). Required.
	DB rowQuerier
	// Vault resolves {{secret.NAME}} references from the Fernet vault. Required.
	Vault vaultDecryptor
	// ProviderConcurrency caps per-provider worker goroutines (§9.5). When <= 0
	// bifrost's CheckAndSetDefaults applies its own default.
	ProviderConcurrency int
	// SelfOrigins are the platform's own /llm origins (e.g.
	// "https://dev.elitea.ai/llm/v1", "http://pylon_main:8080/llm/v1"). Any
	// credential api_base matching one of these origins is rejected with
	// SELF_REFERENTIAL_CREDENTIAL. Values are normalised (scheme+host+path,
	// trailing slash stripped) before comparison.
	SelfOrigins []string
	// EgressAllowlist is the operator's BOOTSTRAP FLOOR for the hosts a
	// tenant-authored credential api_base may name: `host`, `host:port`,
	// `*.domain`, `*.domain:port` or a CIDR block (issue #13,
	// GATEWAY_EGRESS_ALLOWLIST). A malformed entry fails construction.
	//
	// It is a floor, not the whole policy. EgressSource supplies the rows an
	// admin authors at runtime, and the gate enforces the UNION. See egress.go.
	EgressAllowlist []string
	// EgressSource is the runtime half of the allowlist: the compiled
	// `egress_allowlist` governance rows. nil leaves the gate on the
	// environment floor alone, which is the posture of a gateway with no
	// governance store. It can also be bound after construction with
	// SetEgressSource, because the policy store is built later than the
	// account.
	EgressSource EgressSource
	// PublicProjectID is the platform's shared ("public") project id as a
	// decimal string (ELITEA_AI_PROJECT_ID). When set, GetKeysForProvider also
	// returns that project's `shared = true` credentials, so a platform-published
	// model is usable by every project (issue #316). Empty disables the shared
	// scope and restores the project-local-only behaviour.
	//
	// This MUST be operator configuration. A request-supplied value would let a
	// caller name any project as "public" and read its credentials.
	PublicProjectID string
	// HopMarker is the deployment's hop marker (issue #164). When armed, its
	// value is added to EVERY outbound provider request so a request that
	// routes back into this platform's /llm surface can be recognised as a
	// re-entry and refused. nil leaves the outbound half unarmed.
	//
	// Setting it on every provider, and not only on the tenant-authored
	// classes, is deliberate: the loop closes through whatever endpoint a
	// credential names, and a provider class the marker skipped would be a
	// class the guard cannot see.
	HopMarker *hopmarker.Marker
	// Logger is used for structured logging; never logs secret material. When
	// nil, slog.Default is used.
	Logger *slog.Logger
}

// New constructs an EliteaAccount from cfg.
func New(cfg Config) (*EliteaAccount, error) {
	if cfg.DB == nil {
		return nil, errors.New("account: DB is required")
	}
	if cfg.Vault == nil {
		return nil, errors.New("account: Vault is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	origins := make(map[string]struct{}, len(cfg.SelfOrigins))
	for _, o := range cfg.SelfOrigins {
		if n := normaliseOrigin(o); n != "" {
			origins[n] = struct{}{}
		}
	}
	egress, err := newEgressGate(cfg.EgressAllowlist, cfg.EgressSource, logger)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}
	// Reject a malformed public project id at construction. The value is
	// interpolated into a schema name, so a bad one must fail at startup rather
	// than on the first request.
	if cfg.PublicProjectID != "" {
		if err := validateProjectID(cfg.PublicProjectID); err != nil {
			return nil, fmt.Errorf("account: public project id: %w", err)
		}
	}
	return &EliteaAccount{
		db:                  cfg.DB,
		vault:               cfg.Vault,
		providerConcurrency: cfg.ProviderConcurrency,
		selfOrigins:         origins,
		egress:              egress,
		publicProjectID:     cfg.PublicProjectID,
		hopMarker:           cfg.HopMarker,
		logger:              logger,
	}, nil
}

// SetEgressSource binds the runtime allowlist plane after construction.
//
// The account is a hard dependency of the bifrost client and is built first;
// the policy store is optional and is built later. Rather than reorder two
// independent lifecycles, main() calls this once the store exists, before the
// listener opens.
func (a *EliteaAccount) SetEgressSource(source EgressSource) {
	if a == nil {
		return
	}
	a.egress.SetSource(source)
}

// EgressAllowlistConfigured reports whether an egress allowlist is in force,
// from either source. main() logs this at startup: the two policy modes differ
// in whether a tenant-authored api_base host is restricted at all, and an
// operator must be able to see which one is armed without reading the code
// (issue #13).
func (a *EliteaAccount) EgressAllowlistConfigured() bool { return a.egress.configured() }

// EgressPrivateNetworkAllowed reports whether the merged allowlist EXPLICITLY
// names a private destination, which is what relaxes bifrost's SSRF-safe dialer
// for the self-hosted provider classes.
//
// It is deliberately not the same question as EgressAllowlistConfigured. An
// allowlist naming only public SaaS hosts restricts api_base and grants no
// private reachability at all; conflating the two is what made a private
// on-premise endpoint impossible to reach without also widening the dialer for
// destinations nobody named.
func (a *EliteaAccount) EgressPrivateNetworkAllowed() bool {
	if a == nil {
		return false
	}
	return a.egress.allowsPrivateNetwork()
}

// EgressAllows reports whether apiBase may be dialled under the operator's
// configured egress allowlist. It exposes the identical decision
// GetKeysForProvider already applies to every persisted credential, for the
// connection-check endpoint (#319): that endpoint tests a credential BEFORE
// it is saved, so there is no p_{projectID}.configuration row to run through
// GetKeysForProvider — but the tenant-authored api_base it dials is exactly
// as untrusted, and must be refused on the same terms (issue #13).
func (a *EliteaAccount) EgressAllows(apiBase string) bool {
	if a == nil {
		return false
	}
	return a.egress.allows(apiBase)
}

// GetConfiguredProviders returns the static supported-provider set. Per-project
// availability is enforced in GetKeysForProvider (which returns zero keys for a
// provider a project has not configured), so this list does not depend on any
// project and needs no context (matching the interface, which supplies none).
func (a *EliteaAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	out := make([]schemas.ModelProvider, len(supportedProviders))
	copy(out, supportedProviders)
	return out, nil
}

// GetConfigForProvider returns the per-provider ProviderConfig. The interface
// supplies no context, so this cannot vary by project; it carries only the
// tuned-down concurrency (§9.5). bifrost/core fills the remaining defaults via
// ProviderConfig.CheckAndSetDefaults. Per-project endpoints (api_base) are
// delivered per request through GetKeysForProvider's key configs, not here.
func (a *EliteaAccount) GetConfigForProvider(provider schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	cfg := &schemas.ProviderConfig{
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{
			Concurrency: a.providerConcurrency,
		},
	}
	// Self-hosted provider classes (vLLM, Ollama) exist to serve from private
	// networks — their per-key URLs routinely point at RFC-1918 hosts, so
	// bifrost's SSRF-safe dialer must not reject them. Cloud providers keep
	// the guard: an api_base for openai/anthropic/etc. must never resolve to
	// a private address (that shape is exactly the SSRF the dialer exists to
	// stop, and the SELF_REFERENTIAL guard's runtime backstop).
	//
	// ISSUE #13: that carve-out used to be unconditional, and the URL those
	// classes dial is TENANT-AUTHORED — any user who could author a credential
	// row could make the gateway open a connection to any address the pod can
	// reach. The exemption is gated on the operator having enumerated the
	// legitimate destinations; GetKeysForProvider refuses every credential
	// whose api_base is not on that list, so the only private addresses
	// reachable are ones an operator named.
	//
	// GAP G6: the gate used to be "is ANY allowlist configured", and that read
	// the wrong signal in both directions at once. An allowlist naming only
	// public SaaS hosts relaxed the dialer for the whole private network, while
	// an operator who wanted one private vLLM had to edit the chart and restart
	// the pod. The gate is now "does an entry EXPLICITLY name a private host or
	// CIDR" — see egresslib.AllowsPrivateNetwork — and the entries may come
	// from an authored governance row rather than only from the chart.
	//
	// bifrost LATCHES this value: ConfigureDialer runs when the provider worker
	// is created, so a later change does not reach a worker that already exists.
	// main() closes that gap by calling bifrost.UpdateProvider for the two
	// self-hosted classes when the merged decision flips, which re-reads this
	// method and rebuilds the worker. Without that call an authored row would
	// appear on /governance/status and change nothing until a restart, which is
	// the exact shape of defect this work exists to remove.
	//
	// This method takes no context and no key, so it cannot decide per
	// credential — which is exactly why the per-credential half of the policy
	// has to live in GetKeysForProvider.
	switch provider {
	case schemas.VLLM, schemas.Ollama:
		cfg.NetworkConfig.AllowPrivateNetwork = a.egress.allowsPrivateNetwork()
	}

	// ISSUE #164: the OUTBOUND half of hop-marker detection. bifrost applies
	// NetworkConfig.ExtraHeaders to every request it sends for this provider
	// (providers/utils.SetExtraHeaders / SetExtraHeadersHTTP), and it applies
	// them BEFORE it signs a request, so an AWS SigV4 signature covers the
	// marker instead of being invalidated by it.
	//
	// The marker rides EVERY provider, not only the classes whose api_base is
	// tenant-authored. A loop closes through whichever endpoint a credential
	// names, so a class the marker skipped would be a class the guard cannot
	// see. The cost of that breadth is that the marker reaches api.openai.com
	// and every other upstream, which is why it carries dedicated key material
	// and why recognising a harvested copy refuses only the request that
	// carries it — see internal/hopmarker.
	//
	// This method is called once per provider, without a context and without a
	// key, so the value it returns cannot vary per project. That fits: the
	// marker is a constant of the deployment.
	if v := a.hopMarker.Value(); v != "" {
		cfg.NetworkConfig.ExtraHeaders = map[string]string{hopmarker.Header: v}
	}
	return cfg, nil
}

// GetKeysForProvider resolves the calling project's credentials for provider,
// reading them from the Fernet vault per request. It never returns raw key
// material to anywhere but bifrost/core's in-memory schemas.Key.
//
// The projectID is taken from BifrostContextKeyVirtualKey, which the /llm
// handler sets to the resolved project id from the signed X-Elitea-Project-Id
// header (design §5.3). With no project in context there is nothing to resolve
// and zero keys are returned (core treats this as "no key for provider").
func (a *EliteaAccount) GetKeysForProvider(ctx context.Context, provider schemas.ModelProvider) ([]schemas.Key, error) {
	// Issue #17: the BFF.9d overhead metric must count this work. bifrost/core
	// calls this method from its provider worker, inside the router call, and
	// it passes the caller's own BifrostContext. The /llm handler put an
	// overhead.Meter on that context; the mark tells the handler how much of
	// the request had elapsed when core held the key. Everything after the mark
	// is the provider round-trip, which the metric excludes.
	//
	// The mark records on every return path. A refusal (a self-referential
	// credential, a blocked egress host, a vault failure) costs the caller the
	// same gateway time as a success. The Meter keeps the earliest mark, so a
	// retry that arrives here after a provider call cannot inflate the value.
	// A context with no Meter yields a nil *Meter, and the call is a no-op.
	defer overhead.FromContext(ctx).MarkCredentialsResolved()

	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return []schemas.Key{}, nil
	}

	creds, err := a.loadCredentials(ctx, projectID, provider)
	if err != nil {
		return nil, fmt.Errorf("account: load credentials for project %s provider %s: %w", projectID, provider, err)
	}

	// Issue #451: a model row names ONE credential. Use that credential alone.
	// Without this the whole provider set goes to bifrost/core, and core picks
	// one by its own rotation — so a project with two credentials of one
	// provider can call the endpoint the model did not name. See
	// credential_selector.go.
	creds, err = a.selectLinkedCredential(ctx, projectID, provider, creds)
	if err != nil {
		return nil, err
	}

	keys := make([]schemas.Key, 0, len(creds))
	for _, c := range creds {
		// Self-referential guard: reject any credential pointing back at the
		// platform's own /llm origin before its secret is ever resolved.
		if a.isSelfReferential(c.apiBase) {
			a.logger.WarnContext(ctx, "rejected self-referential provider credential",
				"reason", SelfReferentialCredentialReason,
				"project_id", projectID,
				// The credential may belong to the public project rather than
				// the caller (issue #316); name its owner so an operator looks
				// the row up in the right schema.
				"credential_project_id", c.ownerProjectID,
				"provider", string(provider),
				"config_id", c.configID,
			)
			return nil, fmt.Errorf("account: credential %s: %w", c.configID, ErrSelfReferentialCredential)
		}

		// Egress allowlist (issue #13). This MUST stay above the vault resolve:
		// api_key may be a {{secret.NAME}} reference, and the resolved plaintext
		// is shipped to whatever host api_base names as a Bearer token. Checking
		// after the resolve would still stop the connection, but only after the
		// tenant's own vault secret had been decrypted for a destination the
		// operator never sanctioned. Rejecting first means a non-allowlisted
		// destination never causes a decrypt at all.
		if !a.egress.allows(c.apiBase) {
			// The host is deliberately absent from the log line's structured
			// fields for a caller to read back; operators get the config_id and
			// can look the row up. The error carries no host either.
			a.logger.WarnContext(ctx, "rejected provider credential: api_base host is not on the egress allowlist",
				"reason", EgressNotAllowedReason,
				"project_id", projectID,
				"credential_project_id", c.ownerProjectID,
				"provider", string(provider),
				"config_id", c.configID,
			)
			return nil, fmt.Errorf("account: credential %s: %w", c.configID, ErrEgressNotAllowed)
		}

		// Resolve the secret against the project that OWNS the credential, not
		// the caller. A shared credential's {{secret.NAME}} reference names a
		// secret in the public project's Fernet vault; resolving it against the
		// caller's vault would either fail or silently pick up an unrelated
		// same-named secret of the caller's. The plaintext still never leaves
		// this process — it goes only into bifrost's in-memory schemas.Key.
		apiKey, err := a.vault.Resolve(ctx, c.ownerProjectID, c.apiKeyRef)
		if err != nil {
			return nil, fmt.Errorf("account: resolve secret for credential %s: %w", c.configID, err)
		}

		// The secret of a Bedrock or a Vertex credential is NOT api_key: it is
		// aws_secret_access_key or vertex_credentials. Those fields hold a
		// {{secret.NAME}} reference just as often as api_key does, so they get
		// the same vault resolve, against the same owning project. Without it
		// the provider receives the literal reference text and every Bedrock
		// and Vertex call fails to authenticate (issues #453, #454).
		c, err = a.resolveProviderSecrets(ctx, c)
		if err != nil {
			return nil, err
		}

		key, err := buildKey(provider, c, apiKey, requestModelFromContext(ctx))
		if err != nil {
			a.logger.WarnContext(ctx, "rejected provider credential",
				"reason", credentialRejectionReason(err),
				"project_id", projectID,
				"credential_project_id", c.ownerProjectID,
				"provider", string(provider),
				"config_id", c.configID,
			)
			return nil, fmt.Errorf("account: credential %s: %w", c.configID, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// resolveProviderSecrets decrypts the secret-bearing fields that are not
// api_key. Every value is resolved against the project that OWNS the row, for
// the same reason api_key is (a shared credential's reference names a secret in
// the public project's vault). A field that holds no reference is returned
// verbatim by the vault, so this is safe to run on every credential.
func (a *EliteaAccount) resolveProviderSecrets(ctx context.Context, c credential) (credential, error) {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"aws_secret_access_key", &c.awsSecretAccessKeyRef},
		{"vertex_credentials", &c.vertexCredentialsRef},
	} {
		if *field.value == "" {
			continue
		}
		plain, err := a.vault.Resolve(ctx, c.ownerProjectID, *field.value)
		if err != nil {
			// The field name is safe to name; the value is not, and the vault
			// error carries only the reference.
			return credential{}, fmt.Errorf("account: resolve %s for credential %s: %w", field.name, c.configID, err)
		}
		*field.value = plain
	}
	return c, nil
}

// credentialRejectionReason maps a buildKey error onto the reason string an
// operator reads in the log line. It never returns the error text, which can
// name fields but must not become a free-form log field.
func credentialRejectionReason(err error) string {
	switch {
	case errors.Is(err, ErrIncompleteCredential):
		return IncompleteCredentialReason
	case errors.Is(err, ErrUnsupportedAPIBase):
		return UnsupportedAPIBaseReason
	default:
		return "INVALID_PROVIDER_CREDENTIAL"
	}
}

// buildKey assembles a schemas.Key for a resolved credential and fills in the
// provider key configuration that provider needs.
//
// It returns an error when the stored credential cannot produce a usable key.
// Returning a key anyway is what made issues #452 to #456: bifrost either
// substitutes a default (Azure api-version), refuses the key deep in core with
// a message the tenant cannot act on (Vertex, vLLM, Ollama), or — for Bedrock —
// fills in an EMPTY key configuration and lets AWS authenticate the request with
// the pod's own identity. Every one of those is worse than a refusal here.
//
// requestModel is the model the /llm handler dispatches, or "" when it is not
// known. It is used only to build the Azure api-version alias.
//
// Every SecretVar is built as plain text rather than through
// schemas.NewSecretVar. NewSecretVar treats a leading "env." or "vault." as a
// reference and reads the process environment. Every value here is
// TENANT-AUTHORED, so passing it through that constructor would let a tenant
// name an environment variable of the gateway pod and have its content sent
// upstream as a Bearer token or an endpoint.
func buildKey(provider schemas.ModelProvider, c credential, apiKey, requestModel string) (schemas.Key, error) {
	key := schemas.Key{
		ID:     c.keyID,
		Name:   c.name,
		Value:  plainSecret(apiKey),
		Models: schemas.WhiteList{"*"},
	}
	switch provider {
	case schemas.OpenAI:
		// ISSUE #452. bifrost has no per-key base URL for the OpenAI provider,
		// so an api_base that is not OpenAI's own cannot be honoured. Refuse
		// the credential rather than send the tenant's key to api.openai.com.
		if c.apiBase != "" && !isOpenAIOrigin(c.apiBase) {
			return schemas.Key{}, fmt.Errorf(
				"%w: the openai provider cannot use a custom api_base", ErrUnsupportedAPIBase)
		}
	case schemas.Ollama:
		if c.apiBase == "" {
			return schemas.Key{}, missingCredentialFields("api_base")
		}
		key.OllamaKeyConfig = &schemas.OllamaKeyConfig{URL: plainSecret(c.apiBase)}
	case schemas.Azure:
		if c.apiBase == "" {
			return schemas.Key{}, missingCredentialFields("api_base")
		}
		key.AzureKeyConfig = &schemas.AzureKeyConfig{Endpoint: plainSecret(c.apiBase)}
		// ISSUE #455. Attach the credential's api-version to the model this
		// request dispatches. bifrost reads AzureAliasCfg.APIVersion from the
		// alias it resolves for that model name; with no alias it substitutes
		// its own default.
		if c.apiVersion != "" && requestModel != "" {
			key.Aliases = schemas.KeyAliases{
				requestModel: schemas.AliasConfig{
					ModelID:       requestModel,
					AzureAliasCfg: &schemas.AzureAliasCfg{APIVersion: schemas.Ptr(c.apiVersion)},
				},
			}
		}
	case schemas.Bedrock:
		// ISSUE #454, and the reason this function returns an error at all.
		// An incomplete Bedrock credential MUST NOT reach core: core accepts a
		// Bedrock key with no configuration and AWS then uses the ambient
		// credentials of the pod.
		var missing []string
		if c.awsAccessKeyID == "" {
			missing = append(missing, "aws_access_key_id")
		}
		if c.awsSecretAccessKeyRef == "" {
			missing = append(missing, "aws_secret_access_key")
		}
		if c.awsRegion == "" {
			missing = append(missing, "aws_region_name")
		}
		if len(missing) > 0 {
			return schemas.Key{}, missingCredentialFields(missing...)
		}
		key.BedrockKeyConfig = &schemas.BedrockKeyConfig{
			AccessKey: plainSecret(c.awsAccessKeyID),
			SecretKey: plainSecret(c.awsSecretAccessKeyRef),
			Region:    schemas.Ptr(plainSecret(c.awsRegion)),
		}
	case schemas.Vertex:
		// ISSUE #453. All three fields were required by the deleted mapper and
		// bifrost refuses a Vertex key that carries no vertex_key_config.
		var missing []string
		if c.vertexProject == "" {
			missing = append(missing, "vertex_project")
		}
		if c.vertexLocation == "" {
			missing = append(missing, "vertex_location")
		}
		if c.vertexCredentialsRef == "" {
			missing = append(missing, "vertex_credentials")
		}
		if len(missing) > 0 {
			return schemas.Key{}, missingCredentialFields(missing...)
		}
		key.VertexKeyConfig = &schemas.VertexKeyConfig{
			ProjectID:       plainSecret(c.vertexProject),
			Region:          plainSecret(c.vertexLocation),
			AuthCredentials: plainSecret(c.vertexCredentialsRef),
		}
	case schemas.VLLM:
		// Bifrost appends /v1 to this URL. The stored api_base follows the
		// OpenAI client contract and can already end in /v1, so remove that
		// one suffix before handing the URL to Bifrost.
		if c.apiBase == "" {
			return schemas.Key{}, missingCredentialFields("api_base")
		}
		key.VLLMKeyConfig = &schemas.VLLMKeyConfig{URL: plainSecret(bifrostVLLMBaseURL(c.apiBase))}
		// An OpenAI-compatible upstream that also serves the Anthropic
		// dialect (/v1/messages) is selected per credential. bifrost's vllm
		// provider reads this to build Anthropic-shaped requests instead of
		// /v1/responses.
		if c.useAnthropicEndpoints {
			key.UseAnthropicEndpoints = schemas.Ptr(true)
		}
	}
	return key, nil
}

// plainSecret wraps a tenant-authored value as a plain-text bifrost SecretVar.
// See buildKey for why schemas.NewSecretVar must not be used on these values.
func plainSecret(value string) schemas.SecretVar {
	return schemas.SecretVar{Val: value, SecretType: schemas.SecretTypePlainText}
}

// bifrostVLLMBaseURL converts an OpenAI client base URL to the root URL that
// Bifrost's vLLM provider expects. Bifrost adds /v1 for every operation.
func bifrostVLLMBaseURL(apiBase string) string {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		return base[:len(base)-len("/v1")]
	}
	return base
}

// missingCredentialFields builds the ErrIncompleteCredential error. It names the
// fields, which are field NAMES and never field values.
func missingCredentialFields(fields ...string) error {
	return fmt.Errorf("%w: missing %s", ErrIncompleteCredential, strings.Join(fields, ", "))
}

// openAIAPIHost is the only api_base host the bifrost OpenAI provider can be
// pointed at, because that provider's base URL is process-wide.
const openAIAPIHost = "api.openai.com"

// isOpenAIOrigin reports whether apiBase names OpenAI's own API host. An
// open_ai credential normally stores "https://api.openai.com/v1"; that value is
// honoured, and any other host is refused (issue #452).
func isOpenAIOrigin(apiBase string) bool {
	u, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == openAIAPIHost
}

// requestModelFromContext reads the dispatched model name the /llm handler
// stashed under ContextKeyRequestModel. Returns "" when absent or not a string.
func requestModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ContextKeyRequestModel).(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// isSelfReferential reports whether apiBase points at the platform's own /llm
// origin. Comparison uses segment-aware, case-insensitive matching on the
// normalised (scheme+host+path) origin so that partial-segment prefixes (e.g.
// credential "/llm/v" vs self "/llm/v1") do not produce false positives, and a
// credential with an uppercase path cannot evade the guard.
func (a *EliteaAccount) isSelfReferential(apiBase string) bool {
	n := normaliseOrigin(apiBase)
	if n == "" {
		return false
	}
	nLower := strings.ToLower(n)
	for self := range a.selfOrigins {
		selfLower := strings.ToLower(self)
		if nLower == selfLower {
			return true
		}
		// Reject when one is a path-segment prefix of the other so that
		// ".../llm" matches self ".../llm/v1" and ".../llm/v1/extra" also
		// matches — but ".../llm/v" does NOT match ".../llm/v1" (partial segment).
		if isSegmentPrefixOf(nLower, selfLower) || isSegmentPrefixOf(selfLower, nLower) {
			return true
		}
	}
	return false
}

// isSegmentPrefixOf reports whether prefix is a path-segment prefix of s.
// The prefix must be followed by "/" or be equal to s; this prevents a path
// component "/llm/v" from matching "/llm/v1".
func isSegmentPrefixOf(prefix, s string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	rest := s[len(prefix):]
	return rest == "" || rest[0] == '/'
}

// normaliseOrigin canonicalises a URL to scheme://host[:port]/path with any
// trailing slash removed, lowercasing scheme and host. Path case is preserved
// for the normalised form; isSelfReferential applies case-insensitive comparison
// at match time so that uppercase paths cannot evade the guard. Non-URL or empty
// input yields "".
//
// Fix #4: normalisation also strips:
//   - A trailing dot from FQDN hostnames (e.g. "host." → "host") so that
//     "https://host./path" and "https://host/path" compare equal.
//   - Explicit default ports that are semantically a no-op: :443 for HTTPS and
//     :80 for HTTP, so "https://host:443/path" and "https://host/path" match.
func normaliseOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)

	// Separate host and port so we can normalise each independently.
	hostname := u.Hostname()
	portStr := u.Port()

	// Strip trailing dot from FQDN (RFC 1034 § 3.1: "host." == "host").
	hostname = strings.TrimSuffix(hostname, ".")
	hostname = strings.ToLower(hostname)

	// Drop explicit default ports so "https://host:443" == "https://host".
	isDefaultPort := (scheme == "https" && portStr == "443") ||
		(scheme == "http" && portStr == "80")
	var host string
	if portStr == "" || isDefaultPort {
		host = hostname
	} else {
		host = net.JoinHostPort(hostname, portStr)
	}

	path := strings.TrimRight(u.Path, "/")
	return scheme + "://" + host + path
}

// projectIDFromContext extracts the resolved projectID the /llm handler stashed
// under BifrostContextKeyVirtualKey. Returns "" when absent or not a string.
func projectIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(schemas.BifrostContextKeyVirtualKey).(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
