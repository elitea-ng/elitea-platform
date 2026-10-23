package indexmeta

import (
	"encoding/json"
	"errors"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
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
	// of the save. It never carries the underlying driver error.
	ErrCurrentIndexConfigurationUnavailable = errors.New("current index configuration save failed")
)

// ConfigurationRequest is the already-authenticated, already-authorized save.
// No storage target is accepted from HTTP input: the registry resolves the
// index from the saved toolkit.
type ConfigurationRequest struct {
	ProjectID     int64
	ActorUserID   int64
	ToolkitID     int64
	IndexName     string
	Configuration json.RawMessage
}
