package account

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// credential is a single provider credential row read from the project's
// configuration table. apiKeyRef is the stored api_key value, which may be a
// literal or a {{secret.NAME}} reference resolved through the Fernet vault; it
// is never logged.
type credential struct {
	configID string // configuration row id
	// keyID is the bifrost Key ID. It equals configID for a credential the
	// caller's own project owns. A credential read from the public project is
	// prefixed (sharedKeyIDPrefix) so two rows that carry the same numeric id in
	// two different schemas cannot collide on one Key ID.
	keyID string
	// ownerProjectID is the project whose schema the row came from. It is NOT
	// always the caller: a shared credential is owned by the public project.
	// The Fernet vault MUST be read with this id, because a {{secret.NAME}}
	// reference in a shared row names a secret in the public project's vault.
	ownerProjectID string
	// shared is the row's `shared` flag as stored. It is read back so the
	// public-scope result can be re-verified in Go (defence in depth against a
	// query that loses its predicate).
	shared    bool
	name      string // human-readable label (elitea_title)
	apiBase   string // provider endpoint (subject to the self-referential guard)
	apiKeyRef string // stored api_key: literal or {{secret.NAME}} reference
	// apiVersion is the Azure OpenAI / DIAL api-version (issue #455). Empty
	// means the credential names no version and bifrost applies its own.
	apiVersion string
	// awsAccessKeyID, awsSecretAccessKeyRef and awsRegion are the amazon_bedrock
	// credential fields (issue #454). awsSecretAccessKeyRef is the stored value
	// and may be a {{secret.NAME}} reference; it is never logged.
	awsAccessKeyID        string
	awsSecretAccessKeyRef string
	awsRegion             string
	// vertexProject, vertexLocation and vertexCredentialsRef are the vertex_ai
	// credential fields (issue #453). vertexCredentialsRef holds the Google
	// service-account document and may be a {{secret.NAME}} reference; it is
	// never logged.
	vertexProject        string
	vertexLocation       string
	vertexCredentialsRef string
	// useAnthropicEndpoints routes vllm-class credentials through the
	// upstream's Anthropic-compatible /v1/messages surface (see credentialData).
	useAnthropicEndpoints bool
	// dialProtocol is the per-model AI DIAL protocol (dial_protocol.go). It is
	// not stored on the credential row: GetKeysForProvider copies it from the
	// linked-credential pin onto the one credential the pin selected.
	dialProtocol DialProtocol
}

// sharedKeyIDPrefix marks a bifrost Key ID that came from the public project
// rather than the caller's own project. Two schemas can each hold a
// configuration row with the same numeric id, so the raw id is not unique once
// two scopes are read; the prefix also tells an operator reading a log line
// that a platform-shared credential served the request.
const sharedKeyIDPrefix = "shared:"

// SelectedCredentialOwner classifies the credential that served the request on
// ctx: requestlog.CredentialOwnerPlatform for a shared credential of the public
// project, requestlog.CredentialOwnerProject for one the calling project owns,
// and "" when no credential served it (legacy issue 6709).
//
// It reads the key bifrost/core SELECTED, not the set GetKeysForProvider
// offered. Without a model link the offered set can mix both scopes, and only
// the selected key says which one paid. bifrost writes that id onto the
// request's BifrostContext after it picks a key, and clears it when the request
// fails, so a failed request classifies as "".
//
// The id is this package's own Key ID, so the shared prefix is a fact this
// package wrote and not a value a caller can supply.
func SelectedCredentialOwner(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	keyID, _ := ctx.Value(schemas.BifrostContextKeySelectedKeyID).(string)
	switch {
	case keyID == "":
		return ""
	case strings.HasPrefix(keyID, sharedKeyIDPrefix):
		return requestlog.CredentialOwnerPlatform
	default:
		return requestlog.CredentialOwnerProject
	}
}

// providerConfigTypes maps a bifrost provider to the p_{projectID}.configuration
// `type` values that represent a credential for it. Derived from the legacy
// runtime_interface_litellm configuration mappers (open_ai→OpenAI,
// azure_open_ai/open_ai_azure/ai_dial→Azure, ollama→Ollama,
// amazon_bedrock→Bedrock, vertex_ai→Vertex).
var providerConfigTypes = map[schemas.ModelProvider][]string{
	schemas.OpenAI:    {"open_ai"},
	schemas.Azure:     {"azure_open_ai", "open_ai_azure", "ai_dial"},
	schemas.Anthropic: {"anthropic"},
	schemas.Ollama:    {"ollama"},
	schemas.Bedrock:   {"amazon_bedrock"},
	schemas.Vertex:    {"vertex_ai"},
	schemas.VLLM:      {"vllm"},
}

// credentialData is the subset of a configuration row's JSONB `data` the account
// reads. api_key holds the credential secret (literal or {{secret.NAME}}); the
// legacy integration schema used api_token, so that name is accepted as a
// fallback for rows authored via the integration path.
type credentialData struct {
	APIBase  string `json:"api_base"`
	APIKey   string `json:"api_key"`
	APIToken string `json:"api_token"`
	// APIVersion is the azure_open_ai / ai_dial api-version (issue #455). The
	// legacy mapper copied it into the credential values; the gateway threads
	// it into the per-request Azure alias so bifrost does not substitute its
	// own default.
	APIVersion string `json:"api_version"`
	// AWSAccessKeyID, AWSSecretAccessKey and AWSRegionName are the
	// amazon_bedrock credential fields (issue #454). Without them bifrost
	// builds an empty bedrock_key_config and AWS falls back to the ambient
	// credentials of the pod, which is a tenant-isolation fault.
	AWSAccessKeyID     string `json:"aws_access_key_id"`
	AWSSecretAccessKey string `json:"aws_secret_access_key"`
	AWSRegionName      string `json:"aws_region_name"`
	// VertexProject, VertexLocation and VertexCredentials are the vertex_ai
	// credential fields (issue #453). bifrost rejects a Vertex key that has no
	// vertex_key_config.
	VertexProject     string   `json:"vertex_project"`
	VertexLocation    string   `json:"vertex_location"`
	VertexCredentials jsonText `json:"vertex_credentials"`
	// UseAnthropicEndpoints routes vllm-class credentials through the
	// upstream's Anthropic-compatible endpoints (/v1/messages) instead of the
	// OpenAI-compatible ones. Threaded into schemas.Key.UseAnthropicEndpoints;
	// bifrost's vllm provider then builds Anthropic-dialect requests against
	// api_base + /v1/messages with Bearer auth.
	UseAnthropicEndpoints bool `json:"use_anthropic_endpoints"`
}

// noValueSentinel is the platform's "this field has no value" marker. The
// credential screens write a single dash rather than an empty string, so a
// keyless credential arrives as "-" and not as "" (issue #456). The deleted
// LiteLLM mapper dropped both forms (copyOptionalNonDashString); the gateway
// must do the same, or the dash reaches the provider as a Bearer token.
const noValueSentinel = "-"

// storedValue returns the usable text of a stored credential field. It maps the
// no-value sentinel onto the empty string so every caller can test one thing.
func storedValue(raw string) string {
	if raw == noValueSentinel {
		return ""
	}
	return raw
}

// jsonText decodes a JSONB field the platform stores either as a JSON string or
// as a nested JSON object. vertex_credentials is the field that needs it: the
// Google service-account document is written as an escaped string by one screen
// and as an object by another. A plain `string` field makes the object form fail
// to decode, and a failed decode drops the whole credential row.
type jsonText string

func (t *jsonText) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		*t = ""
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		*t = jsonText(s)
		return nil
	}
	*t = jsonText(raw)
	return nil
}

// credentialKeyRef returns the credential secret reference, preferring api_key
// and falling back to the legacy api_token field. The no-value sentinel counts
// as absent for both, so a keyless credential yields "" (issue #456).
func (d credentialData) credentialKeyRef() string {
	if k := storedValue(d.APIKey); k != "" {
		return k
	}
	return storedValue(d.APIToken)
}

// credentialsSQL reads the ai_credentials rows for one project scope. %q is the
// schema name and %s is the scope predicate: empty for the caller's own project
// (a project sees all of its own credentials) and sharedPredicate for the public
// project (a caller may see ONLY the rows the platform published). `shared` is
// selected so the result can be re-verified in Go.
const credentialsSQL = `
		SELECT COALESCE(uuid::text, id::text), COALESCE(elitea_title, ''), data, shared
		FROM %q.configuration
		WHERE section = 'ai_credentials' AND type = ANY($1) AND status_ok = true%s
		ORDER BY id`

// sharedPredicate restricts a cross-project read to published rows. It is the
// ONLY thing that makes reading a second schema safe, so it is a constant and
// never built from a caller-supplied value.
const sharedPredicate = " AND shared = true"

// loadCredentials reads the ai_credentials configuration rows for the given
// provider and decodes each row's `data` JSONB into a credential.
//
// It reads two scopes (issue #316):
//
//  1. p_{projectID} — every credential the caller's own project owns.
//  2. p_{publicProjectID} — normally only rows flagged `shared = true`.
//     A credential pinned by a model published from that project may read its
//     exact owner row even when the credential is not separately published.
//     Running the model does not make the credential generally selectable.
//
// The caller's own rows come first, so the legacy precedence is preserved: where
// both scopes describe the same thing, the project's own row wins (legacy
// _map_model_name probed {project}_{model} before {public}_{model}).
//
// The second scope is skipped when it is unset or when the caller IS the public
// project (that project already reads its own rows in scope 1, and reading it
// twice would duplicate every shared row).
//
// TENANT ISOLATION: publicProjectID is operator configuration, never request
// data, and the public-scope read always carries sharedPredicate. No other
// project's schema is reachable from this path.
//
// Rows whose type does not map to a supported provider yield no results. No
// secret material is resolved here — that happens in GetKeysForProvider after
// the self-referential guard runs.
func (a *EliteaAccount) loadCredentials(ctx context.Context, projectID string, provider schemas.ModelProvider) ([]credential, error) {
	types, ok := providerConfigTypes[provider]
	if !ok || len(types) == 0 {
		return nil, nil
	}
	// A linked open_ai credential can name a tenant-specific OpenAI-compatible
	// endpoint. The model resolver routes that exact link through Bifrost's vLLM
	// provider so the per-key api_base is preserved. Widen the type query only
	// when a credential is pinned; selectLinkedCredential below then admits that
	// one row and cannot substitute another OpenAI credential.
	if provider == schemas.VLLM {
		if _, linked := linkedCredentialFromContext(ctx); linked {
			types = append(append([]string(nil), types...), "open_ai")
		}
	}

	// projectID and the derived schema name come from a signed, server-resolved
	// header (never raw client input) and are validated numeric before use, so
	// the fmt-built schema identifier is not an injection vector.
	if err := validateProjectID(projectID); err != nil {
		return nil, err
	}

	creds, err := a.loadCredentialScope(ctx, projectID, provider, types, false)
	if err != nil {
		return nil, err
	}

	if a.publicProjectID == "" || a.publicProjectID == projectID {
		return creds, nil
	}
	if err := validateProjectID(a.publicProjectID); err != nil {
		return nil, fmt.Errorf("public project id: %w", err)
	}
	sharedOnly := true
	if link, linked := linkedCredentialFromContext(ctx); linked &&
		link.ModelOwnerAccess && link.ProjectID == a.publicProjectID {
		sharedOnly = false
	}
	shared, err := a.loadCredentialScope(ctx, a.publicProjectID, provider, types, sharedOnly)
	if err != nil {
		return nil, err
	}
	return append(creds, shared...), nil
}

// loadCredentialScope reads one project's ai_credentials rows. sharedOnly adds
// the `shared = true` predicate and makes the caller's read a cross-project one;
// every row returned under it is re-checked against its own `shared` column
// before it is used.
func (a *EliteaAccount) loadCredentialScope(
	ctx context.Context,
	scopeProjectID string,
	provider schemas.ModelProvider,
	types []string,
	sharedOnly bool,
) ([]credential, error) {
	predicate := ""
	if sharedOnly {
		predicate = sharedPredicate
	}
	q := fmt.Sprintf(credentialsSQL, fmt.Sprintf("p_%s", scopeProjectID), predicate)

	rows, err := a.db.Query(ctx, q, types)
	if err != nil {
		return nil, fmt.Errorf("query configuration for project %s: %w", scopeProjectID, err)
	}
	defer rows.Close()

	var creds []credential
	for rows.Next() {
		var (
			id, title string
			dataBytes []byte
			shared    bool
		)
		if err := rows.Scan(&id, &title, &dataBytes, &shared); err != nil {
			return nil, fmt.Errorf("scan configuration row: %w", err)
		}
		// Defence in depth: a cross-project read must never yield an unpublished
		// row. The SQL predicate above already excludes them, so reaching here
		// means the query lost its predicate — fail the whole read rather than
		// hand another project's private credential to the caller. This mirrors
		// elitea-main's "escaped its authorized scope" check on the same table.
		if sharedOnly && !shared {
			return nil, fmt.Errorf(
				"account: configuration row %s from project %s escaped the shared scope",
				id, scopeProjectID)
		}
		var d credentialData
		if len(dataBytes) > 0 {
			if err := json.Unmarshal(dataBytes, &d); err != nil {
				// A malformed row must not poison the whole provider; skip it.
				a.logger.WarnContext(ctx, "skipping unparsable ai_credentials row",
					"project_id", scopeProjectID, "provider", string(provider), "config_id", id)
				continue
			}
		}
		keyID := id
		if sharedOnly {
			keyID = sharedKeyIDPrefix + id
		}
		creds = append(creds, credential{
			configID:              id,
			keyID:                 keyID,
			ownerProjectID:        scopeProjectID,
			shared:                shared,
			name:                  title,
			apiBase:               d.APIBase,
			apiKeyRef:             d.credentialKeyRef(),
			apiVersion:            storedValue(d.APIVersion),
			awsAccessKeyID:        storedValue(d.AWSAccessKeyID),
			awsSecretAccessKeyRef: storedValue(d.AWSSecretAccessKey),
			awsRegion:             storedValue(d.AWSRegionName),
			vertexProject:         storedValue(d.VertexProject),
			vertexLocation:        storedValue(d.VertexLocation),
			vertexCredentialsRef:  storedValue(string(d.VertexCredentials)),
			useAnthropicEndpoints: d.UseAnthropicEndpoints,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate configuration rows: %w", err)
	}
	return creds, nil
}

// validateProjectID rejects a projectID that is not a positive integer. The
// schema name is interpolated into SQL, so this guards against a malformed or
// hostile value reaching the query even though the id is server-resolved.
func validateProjectID(projectID string) error {
	if projectID == "" {
		return fmt.Errorf("empty project id")
	}
	for _, r := range projectID {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid project id %q: must be numeric", projectID)
		}
	}
	return nil
}
