package indexmeta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

// MaxCurrentIndexConfigurationBytes bounds one saved index configuration.
//
// It is deliberately the SAME ceiling the run path already applies to the tool
// parameters it stores in the very same document field
// (indexingapp.MaxToolParametersBytes): a configuration this route saves is
// replayed verbatim as the tool parameters of the next scheduled or manual
// reindex (internal/application/indexschedule/executor.go's
// scheduledIndexParameters). A configuration that could be SAVED but never
// RUN would be a silent trap, so the two bounds are one bound.
const MaxCurrentIndexConfigurationBytes = indexingapp.MaxToolParametersBytes

var (
	// ErrCurrentIndexConfigurationInvalid reports a configuration this
	// platform refuses to store: not a JSON object, empty, or over the bound
	// the reindex path can replay.
	ErrCurrentIndexConfigurationInvalid = errors.New("current index configuration is invalid")
	// ErrCurrentIndexConfigurationUnavailable reports a storage-side failure
	// of the save. It never carries the underlying driver error, which names
	// the PgVector user, database and host.
	ErrCurrentIndexConfigurationUnavailable = errors.New("current index configuration save failed")
)

// ConfigurationRequest is the already-authenticated, already-authorized save.
// Neither a PgVector DSN nor a PostgreSQL schema is accepted from HTTP input:
// both are resolved from the saved toolkit, exactly as the list and delete
// services resolve them.
type ConfigurationRequest struct {
	ProjectID     int64
	ActorUserID   int64
	ToolkitID     int64
	IndexName     string
	Configuration json.RawMessage
}

func (r ConfigurationRequest) validate() error {
	if !validCurrentIndexMetaDeleteText(r.IndexName, MaxCurrentIndexMetaCollectionBytes) {
		return ErrInvalidCurrentIndexMetaRequest
	}
	if _, ok := currentIndexMetaDatabaseID(r.ProjectID); !ok {
		return ErrInvalidCurrentIndexMetaRequest
	}
	if _, ok := currentIndexMetaDatabaseID(r.ActorUserID); !ok {
		return ErrInvalidCurrentIndexMetaRequest
	}
	if _, ok := currentIndexMetaDatabaseID(r.ToolkitID); !ok {
		return ErrInvalidCurrentIndexMetaRequest
	}
	return validCurrentIndexConfiguration(r.Configuration)
}

// validCurrentIndexConfiguration accepts exactly one bounded JSON OBJECT.
//
// An array, a number or a bare string would round-trip through storage and
// then fail `scheduledIndexParameters`'s own `{`/`}` check at the moment the
// schedule fires — hours later, in a background worker, with no user present.
// It is refused here, where there is someone to tell.
func validCurrentIndexConfiguration(configuration json.RawMessage) error {
	if len(configuration) == 0 || len(configuration) > MaxCurrentIndexConfigurationBytes ||
		!json.Valid(configuration) {
		return ErrCurrentIndexConfigurationInvalid
	}
	trimmed := bytes.TrimSpace(configuration)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return ErrCurrentIndexConfigurationInvalid
	}
	return nil
}

// ExternalConfigurationWriter performs the one PgVector transaction that
// replaces `index_configuration` on the addressed index metadata document.
//
// Implementations MUST leave every other field of that document alone — the
// state, the task id, the run history and the document counters are owned by
// the run path, and this save deliberately runs while no run is in flight.
type ExternalConfigurationWriter interface {
	SaveConfiguration(context.Context, ResolvedTarget, string, json.RawMessage) error
}

// ConfigurationService persists an index's configuration WITHOUT starting a
// run.
//
// Why it exists: until this service the only writer of
// `index_configuration` was the indexing run itself, so the UI's
// "Save"/"Save & Reindex" split had no Save half to call — a configuration
// change could only be persisted by re-indexing the whole collection. The
// scheduled reindex reads this exact field
// (runtimecomposition/index_schedule_inspector.go), so a save here is also
// what makes "the schedule uses the last SAVED configuration, not the
// unsaved edits in the form" true.
type ConfigurationService struct {
	toolkits indexingapp.CurrentToolkitReader
	settings indexingapp.CurrentToolkitSettingsValidator
	external ExternalConfigurationWriter
}

func NewConfigurationService(
	toolkits indexingapp.CurrentToolkitReader,
	settings indexingapp.CurrentToolkitSettingsValidator,
	external ExternalConfigurationWriter,
) (*ConfigurationService, error) {
	if toolkits == nil || settings == nil || external == nil {
		return nil, errors.New("current index configuration dependencies are required")
	}
	return &ConfigurationService{toolkits: toolkits, settings: settings, external: external}, nil
}

// SaveConfiguration resolves the project-owned PgVector target and replaces the
// stored configuration of one index.
func (s *ConfigurationService) SaveConfiguration(ctx context.Context, request ConfigurationRequest) error {
	if s == nil || s.toolkits == nil || s.settings == nil || s.external == nil || ctx == nil {
		return ErrInvalidCurrentIndexMetaRequest
	}
	if err := request.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := ResolveCurrentTarget(
		ctx,
		s.toolkits,
		s.settings,
		Request{
			ProjectID:   request.ProjectID,
			ActorUserID: request.ActorUserID,
			ToolkitID:   request.ToolkitID,
		},
		MaxCurrentIndexMetaRows,
	)
	if err != nil {
		return err
	}
	// The resolved connection string exists for this one synchronous call and
	// is never copied into a result or an error.
	if err := s.external.SaveConfiguration(ctx, target, request.IndexName, request.Configuration); err != nil {
		if errors.Is(err, ErrCurrentIndexMetaNotFound) {
			return ErrCurrentIndexMetaNotFound
		}
		return currentIndexMetaDependencyError(ctx, ErrCurrentIndexConfigurationUnavailable, err)
	}
	return nil
}

var _ interface {
	SaveConfiguration(context.Context, ConfigurationRequest) error
} = (*ConfigurationService)(nil)
