package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	CurrentEmbeddingBindingSchema = "elitea.index.embedding-binding.v2"
	maxEmbeddingBindingIdentity   = 1024

	// currentEmbeddingBindingRoute is the only route the Bifrost gateway can be
	// bound to. The "project"/"public" routes carried a `{projectID}_` model
	// group prefix, which existed solely because LiteLLM held one flat global
	// registry and had to disambiguate identically named per-project models
	// inside it. The gateway has no such registry: the wire name sent upstream
	// is the plain model name and the project is carried by the edge's signed
	// identity (services/elitea-llm-gateway/internal/llmproxy/handler.go,
	// modelsProjectID), from which the gateway resolves that project's own
	// configuration row. A prefixed name would therefore not resolve at all.
	currentEmbeddingBindingRoute = "raw"
)

var (
	ErrCurrentEmbeddingBindingUnavailable = errors.New("current embedding binding is unavailable")
	ErrCurrentEmbeddingBindingAmbiguous   = errors.New("current embedding binding is ambiguous")
	ErrInvalidCurrentEmbeddingBinding     = errors.New("invalid current embedding binding")
)

// CurrentEmbeddingConfiguration is the non-secret identity of one current
// Configurations row. Data must remain the saved model configuration, not an
// unsecreted or gateway-expanded value.
type CurrentEmbeddingConfiguration struct {
	UUID      string
	ProjectID int32
	Type      string
	Section   string
	Data      json.RawMessage
	Shared    bool
}

// CurrentEmbeddingConfigurationReader performs one tenant-routed exact-name
// lookup. sharedOnly is true only for the public-project fallback.
type CurrentEmbeddingConfigurationReader interface {
	FindCurrentEmbeddingConfiguration(
		context.Context,
		int32,
		string,
		bool,
	) (CurrentEmbeddingConfiguration, bool, error)
}

// EmbeddingBinding is an immutable, non-secret admission record. It preserves
// the authoritative default (model_name, model_project_id) tuple and the
// identity of the Configurations row that backed the model at admission.
//
// It is not a deployment pin: the worker passes ModelName to the unchanged SDK,
// and the gateway resolves provider, endpoint and credentials at execution from
// the same p_{project}.configuration rows this binding was resolved against.
type EmbeddingBinding struct {
	SchemaVersion          string
	ModelName              string
	ResolvedModelGroup     string
	Route                  string
	ModelProjectID         int32
	ConfigurationProjectID int32
	ConfigurationUUID      string
	ConfigurationDigest    runtimedomain.Digest
}

func (b EmbeddingBinding) Validate() error {
	if b.SchemaVersion != CurrentEmbeddingBindingSchema ||
		!validEmbeddingBindingIdentity(b.ModelName) ||
		!validEmbeddingBindingIdentity(b.ResolvedModelGroup) {
		return ErrInvalidCurrentEmbeddingBinding
	}
	switch b.Route {
	// "project"/"public" are no longer produced: only LiteLLM needed the
	// `{projectID}_` group prefix. They stay accepted because the v2 schema is a
	// cross-process contract shared with the Python worker
	// (protocol/indexing.py) and pins already-admitted bindings that are still
	// replayable; rejecting them here would be a schema break, not a cleanup.
	case "project", "public":
		if !validPrefixedEmbeddingGroup(b.ResolvedModelGroup, b.ModelName) {
			return ErrInvalidCurrentEmbeddingBinding
		}
	case "raw":
		if b.ResolvedModelGroup != b.ModelName {
			return ErrInvalidCurrentEmbeddingBinding
		}
	default:
		return ErrInvalidCurrentEmbeddingBinding
	}
	if b.ModelProjectID < 0 || b.ConfigurationProjectID < 0 {
		return ErrInvalidCurrentEmbeddingBinding
	}
	hasConfigurationIdentity := b.ConfigurationProjectID != 0 ||
		b.ConfigurationUUID != "" ||
		!b.ConfigurationDigest.IsZero()
	if hasConfigurationIdentity &&
		(b.ConfigurationProjectID <= 0 ||
			!validCanonicalUUID(b.ConfigurationUUID) ||
			b.ConfigurationDigest.IsZero()) {
		return ErrInvalidCurrentEmbeddingBinding
	}
	if b.ModelProjectID != 0 && b.ConfigurationProjectID != 0 &&
		b.ModelProjectID != b.ConfigurationProjectID {
		return ErrInvalidCurrentEmbeddingBinding
	}
	return nil
}

// MarshalCanonical returns the exact non-secret bytes stored in the input data
// plane. Redis receives only their immutable entry reference and digest.
func (b EmbeddingBinding) MarshalCanonical() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	document := struct {
		SchemaVersion          string `json:"schema_version"`
		ModelName              string `json:"model_name"`
		ResolvedModelGroup     string `json:"resolved_model_group"`
		Route                  string `json:"route"`
		ModelProjectID         int32  `json:"model_project_id,omitempty"`
		ConfigurationProjectID int32  `json:"configuration_project_id,omitempty"`
		ConfigurationUUID      string `json:"configuration_uuid,omitempty"`
		ConfigurationDigest    string `json:"configuration_digest,omitempty"`
	}{
		SchemaVersion:          b.SchemaVersion,
		ModelName:              b.ModelName,
		ResolvedModelGroup:     b.ResolvedModelGroup,
		Route:                  b.Route,
		ModelProjectID:         b.ModelProjectID,
		ConfigurationProjectID: b.ConfigurationProjectID,
		ConfigurationUUID:      b.ConfigurationUUID,
	}
	if !b.ConfigurationDigest.IsZero() {
		document.ConfigurationDigest = b.ConfigurationDigest.String()
	}
	return json.Marshal(document)
}

func (b EmbeddingBinding) Clone() EmbeddingBinding {
	return b
}

type CurrentEmbeddingBindingResolver struct {
	configurations CurrentEmbeddingConfigurationReader
	publicProject  int32
}

func NewCurrentEmbeddingBindingResolver(
	configurations CurrentEmbeddingConfigurationReader,
	publicProjectID int32,
) (*CurrentEmbeddingBindingResolver, error) {
	if configurations == nil || publicProjectID <= 0 {
		return nil, errors.New("current embedding binding dependencies are required")
	}
	return &CurrentEmbeddingBindingResolver{
		configurations: configurations,
		publicProject:  publicProjectID,
	}, nil
}

// Resolve performs exactly one caller-project -> shared-public-project search,
// against the Configurations rows themselves. It used to perform that search
// twice: once against the LiteLLM administration API purely to learn which
// model group name existed, and once against the database for the configuration
// identity. The registry search is gone with LiteLLM — the gateway pulls the
// same p_{project}.configuration rows this search reads, so a second registry
// that has to be told about models separately no longer exists to disagree.
//
// preferredProjectID is present only when the current model catalog supplied an
// authoritative default tuple; it scopes the single search to that owner.
func (r *CurrentEmbeddingBindingResolver) Resolve(
	ctx context.Context,
	projectID int32,
	modelName string,
	preferredProjectID *int32,
) (EmbeddingBinding, error) {
	if ctx == nil || projectID <= 0 || !validEmbeddingBindingIdentity(modelName) {
		return EmbeddingBinding{}, ErrInvalidCurrentEmbeddingBinding
	}
	if preferredProjectID != nil &&
		(*preferredProjectID != projectID && *preferredProjectID != r.publicProject) {
		return EmbeddingBinding{}, ErrInvalidCurrentEmbeddingBinding
	}
	if err := ctx.Err(); err != nil {
		return EmbeddingBinding{}, err
	}

	binding := EmbeddingBinding{
		SchemaVersion:      CurrentEmbeddingBindingSchema,
		ModelName:          modelName,
		ResolvedModelGroup: modelName,
		Route:              currentEmbeddingBindingRoute,
	}
	if preferredProjectID != nil {
		binding.ModelProjectID = *preferredProjectID
	}

	configuration, found, err := r.resolveCurrentEmbeddingConfiguration(
		ctx,
		projectID,
		modelName,
		preferredProjectID,
	)
	if err != nil {
		return EmbeddingBinding{}, err
	}
	// A name that no row holds is refused HERE (issue #470).
	//
	// This branch used to admit the run. It returned a valid binding with no
	// configuration identity, because Validate checks the identity triple only
	// when one of its three fields is already set. The platform then wrote a
	// durable admission record, dispatched the command, and the run died inside
	// the worker: the gateway reads the same rows and answers 404
	// model_not_found (internal/llmproxy/modelmap.go). The user saw a run that
	// started and then failed, and no message named the model.
	//
	// The preferredProjectID branch above already fails closed for the same
	// state (resolveCurrentEmbeddingConfiguration), so this makes one rule of
	// two. The caller maps the error to 503 "Embedding model is unavailable"
	// (api/v2/indexing/start_handler.go), which names the fault at the moment
	// the caller can still act on it.
	//
	// It must also never fall back to another model. There is no substitute
	// path: the binding is built from the requested name alone.
	if !found {
		return EmbeddingBinding{}, ErrCurrentEmbeddingBindingUnavailable
	}
	configurationDigest, digestErr := currentEmbeddingConfigurationDigest(
		configuration,
		modelName,
	)
	if digestErr != nil {
		return EmbeddingBinding{}, digestErr
	}
	binding.ConfigurationProjectID = configuration.ProjectID
	binding.ConfigurationUUID = configuration.UUID
	binding.ConfigurationDigest = configurationDigest

	if err := binding.Validate(); err != nil {
		return EmbeddingBinding{}, err
	}
	return binding, nil
}

// resolveCurrentEmbeddingConfiguration is the single project -> public search.
// A caller-project row always wins over an identically named shared row in the
// public project, and the public project is consulted only when the caller's
// project has no match — and then only with sharedOnly, so a private
// public-project row can never be bound by another tenant.
func (r *CurrentEmbeddingBindingResolver) resolveCurrentEmbeddingConfiguration(
	ctx context.Context,
	projectID int32,
	modelName string,
	preferredProjectID *int32,
) (CurrentEmbeddingConfiguration, bool, error) {
	if preferredProjectID != nil {
		configuration, found, err := r.configurations.FindCurrentEmbeddingConfiguration(
			ctx,
			*preferredProjectID,
			modelName,
			*preferredProjectID == r.publicProject && projectID != r.publicProject,
		)
		if err != nil {
			return CurrentEmbeddingConfiguration{}, false, currentEmbeddingDependencyError(ctx, err)
		}
		if !found {
			return CurrentEmbeddingConfiguration{}, false, ErrCurrentEmbeddingBindingUnavailable
		}
		// A pinned PUBLIC row is still subject to the grant. The pin says which
		// project the model was resolved in, not that this project may use it.
		if *preferredProjectID == r.publicProject && projectID != r.publicProject &&
			!platformModelGrantsProject(configuration.Data, projectID) {
			return CurrentEmbeddingConfiguration{}, false, ErrCurrentEmbeddingBindingUnavailable
		}
		return configuration, true, nil
	}

	configuration, found, err := r.configurations.FindCurrentEmbeddingConfiguration(
		ctx,
		projectID,
		modelName,
		false,
	)
	if err != nil {
		return CurrentEmbeddingConfiguration{}, false, currentEmbeddingDependencyError(ctx, err)
	}
	if found || projectID == r.publicProject {
		return configuration, found, nil
	}
	configuration, found, err = r.configurations.FindCurrentEmbeddingConfiguration(
		ctx,
		r.publicProject,
		modelName,
		true,
	)
	if err != nil {
		return CurrentEmbeddingConfiguration{}, false, currentEmbeddingDependencyError(ctx, err)
	}
	if found && !platformModelGrantsProject(configuration.Data, projectID) {
		// The row is published and this project holds no grant for it, so for
		// this project the model does not exist. NOT FOUND rather than an
		// error: an ungranted model is the same answer as an absent one, and
		// every caller already handles it.
		return CurrentEmbeddingConfiguration{}, false, nil
	}
	return configuration, found, nil
}

// platformModelGrantsProject reports whether a shared public-project row is
// offered to projectID.
//
// The rule and its leniency are stated once, in
// application/configurations/model_grant.go: an absent or unreadable grant
// means every project, because that is what every row written before the field
// existed meant. This is the THIRD reader of it inside elitea-main — the model
// catalogue and the admin surface are the others — and it is here because the
// index plane resolves a platform embedding model on its own path. Without it a
// project could not SEE a model it could still build an index against.
func platformModelGrantsProject(data json.RawMessage, projectID int32) bool {
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return true
	}
	return configurationapp.ReadModelGrant(decoded).Allows(projectID)
}

func currentEmbeddingConfigurationDigest(
	configuration CurrentEmbeddingConfiguration,
	modelName string,
) (runtimedomain.Digest, error) {
	if configuration.ProjectID <= 0 ||
		configuration.Type != "embedding_model" ||
		configuration.Section != "embedding" ||
		!validCanonicalUUID(configuration.UUID) {
		return runtimedomain.Digest{}, ErrInvalidCurrentEmbeddingBinding
	}
	data, err := decodeCurrentEmbeddingConfigurationData(configuration.Data)
	if err != nil || data.Name != modelName {
		return runtimedomain.Digest{}, ErrInvalidCurrentEmbeddingBinding
	}
	canonical := struct {
		UUID          string                               `json:"uuid"`
		ProjectID     int32                                `json:"project_id"`
		Type          string                               `json:"type"`
		Section       string                               `json:"section"`
		Shared        bool                                 `json:"shared"`
		Name          string                               `json:"name"`
		CredentialRef *currentEmbeddingCredentialReference `json:"ai_credentials,omitempty"`
	}{
		UUID:          configuration.UUID,
		ProjectID:     configuration.ProjectID,
		Type:          configuration.Type,
		Section:       configuration.Section,
		Shared:        configuration.Shared,
		Name:          data.Name,
		CredentialRef: data.CredentialRef,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return runtimedomain.Digest{}, ErrInvalidCurrentEmbeddingBinding
	}
	return runtimedomain.SHA256(encoded), nil
}

type currentEmbeddingCredentialReference struct {
	EliteaTitle string `json:"elitea_title"`
	Private     bool   `json:"private"`
}

type currentEmbeddingConfigurationData struct {
	Name          string
	CredentialRef *currentEmbeddingCredentialReference
}

func decodeCurrentEmbeddingConfigurationData(raw []byte) (currentEmbeddingConfigurationData, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document struct {
		Name          string                               `json:"name"`
		AICredentials *currentEmbeddingCredentialReference `json:"ai_credentials"`
		// The platform GRANT. It is declared here so the strict decode ACCEPTS
		// it, and it is deliberately absent from the canonical document below.
		//
		// A platform embedding model can be granted to every project, to none,
		// or to a chosen set (application/configurations/model_grant.go), and
		// the two fields live on the same `data` object this decoder reads.
		// Without these two lines a granted embedding model failed the decode
		// entirely, which this package reports as an invalid binding: the model
		// would stop resolving for every project, including the ones it had
		// just been granted to.
		//
		// It stays out of the DIGEST because the digest identifies the binding
		// an index was built against. Re-granting a model does not change what
		// it embeds, and a digest that moved with the grant would invalidate
		// every index the moment an operator added one project to the list.
		ShareScope string          `json:"share_scope"`
		SharedWith json.RawMessage `json:"shared_with"`
	}
	if err := decoder.Decode(&document); err != nil {
		return currentEmbeddingConfigurationData{}, ErrInvalidCurrentEmbeddingBinding
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return currentEmbeddingConfigurationData{}, ErrInvalidCurrentEmbeddingBinding
	}
	if !validEmbeddingBindingIdentity(document.Name) ||
		(document.AICredentials != nil &&
			!validEmbeddingBindingIdentity(document.AICredentials.EliteaTitle)) {
		return currentEmbeddingConfigurationData{}, ErrInvalidCurrentEmbeddingBinding
	}
	return currentEmbeddingConfigurationData{
		Name:          document.Name,
		CredentialRef: document.AICredentials,
	}, nil
}

func currentEmbeddingDependencyError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// The reader's duplicate sentinel (two mutable rows share one model name)
	// is a distinct admission outcome the caller switches on
	// (resolver.resolveCurrentEmbeddingBinding), not an upstream failure. It is
	// returned bare rather than wrapped so the reader's message — which may
	// quote row content — never reaches the caller.
	if errors.Is(err, ErrCurrentEmbeddingBindingAmbiguous) {
		return ErrCurrentEmbeddingBindingAmbiguous
	}
	if errors.Is(err, ErrInvalidCurrentEmbeddingBinding) {
		return ErrInvalidCurrentEmbeddingBinding
	}
	return fmt.Errorf("%w", ErrCurrentEmbeddingBindingUnavailable)
}

func validEmbeddingBindingIdentity(value string) bool {
	return value != "" && len(value) <= maxEmbeddingBindingIdentity &&
		utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validCanonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return false
			}
		}
	}
	return true
}

func validPrefixedEmbeddingGroup(group, model string) bool {
	suffix := "_" + model
	if !strings.HasSuffix(group, suffix) || len(group) <= len(suffix) {
		return false
	}
	prefix := group[:len(group)-len(suffix)]
	for _, character := range prefix {
		if character < '0' || character > '9' {
			return false
		}
	}
	projectID, err := strconv.ParseInt(prefix, 10, 32)
	return err == nil && projectID > 0
}
