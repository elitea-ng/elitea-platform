package execution

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const (
	ConfigurationValidationCapability = "configuration.validate.v1"
	IndexIngestCapability             = "index.ingest.v1"
	AgentApplicationCapability        = "agent.execute.application.v1"
	AgentAdhocCapability              = "agent.execute.adhoc.v1"
	ToolkitExecuteReadCapability      = "toolkit.execute.read.v1"
	ToolkitCallToolCapability         = "toolkit.call_tool.v1"
	SettingsJSONMediaType             = "application/json"
	AgentExecutionInputMediaType      = "application/vnd.elitea.agent-execution-input.v1+protobuf"
	ToolkitExecuteReadInputMediaType  = "application/vnd.elitea.toolkit-execute-read-input.v1+protobuf"
	InputBundleManifestMediaType      = "application/x-protobuf"
	MaxInputBundleEntries             = 16
	MaxInputEntryContentBytes         = 256 * 1024
	MaxAgentExecutionInputBytes       = 1024 * 1024
	MaxToolkitExecuteReadInputBytes   = 1024 * 1024

	IndexToolkitConfigurationRole     = "index.toolkit_configuration"
	IndexToolParametersRole           = "index.tool_parameters"
	IndexLLMModelRole                 = "index.llm_model"
	IndexLLMConfigurationRole         = "index.llm_configuration"
	IndexMCPTokensRole                = "index.mcp_tokens"
	IndexEmbeddingBindingRole         = "index.embedding_binding"
	AgentExecutionRequestRole         = "agent.execution_request"
	ToolkitExecuteReadRequestRole     = "toolkit.execute_read_request"
	ToolkitCallToolRuntimeContextRole = "toolkit.call_tool.runtime_context"
	ToolkitCallToolSettingsRole       = "toolkit.call_tool.settings"
	ToolkitCallToolArgumentsRole      = "toolkit.call_tool.arguments"
	MaxIndexMetaIDBytes               = 256
	MaxIndexMetaCorrelationBytes      = 512
	// MaxSafeCommandStringBytes is the bound every worker applies to a bounded
	// command string. Exceeding it here would produce a command the worker
	// refuses, so the refusal belongs on this side where the caller can see it.
	MaxSafeCommandStringBytes = 256
)

var (
	ErrInvalidInputBundle = errors.New("invalid immutable input bundle")
	ErrInvalidJob         = errors.New("invalid execution job")
	ErrInvalidOutbox      = errors.New("invalid command outbox record")
)

type InputEntry struct {
	ID                    string
	Version               string
	SemanticRole          string
	ContentID             string
	MediaType             string
	Classification        string
	RequiredGrantAudience string
	ContentDigest         runtimedomain.Digest
	ContentLength         int64
	Content               []byte
}

// InputBundle holds the exact immutable manifest and content admitted for one
// job. Redis dispatch receives only BundleID/Digest and EntryID, never these
// byte slices.
type InputBundle struct {
	ID        string
	Version   string
	MediaType string
	Digest    runtimedomain.Digest
	Manifest  []byte
	Entries   []InputEntry
}

func (b InputBundle) Validate() error {
	if b.ID == "" || b.Version == "" || b.MediaType != InputBundleManifestMediaType || b.Digest.IsZero() || len(b.Manifest) == 0 {
		return ErrInvalidInputBundle
	}
	if runtimedomain.SHA256(b.Manifest) != b.Digest {
		return fmt.Errorf("%w: manifest digest mismatch", ErrInvalidInputBundle)
	}
	if len(b.Entries) == 0 || len(b.Entries) > MaxInputBundleEntries {
		return ErrInvalidInputBundle
	}
	entryIDs := make(map[string]struct{}, len(b.Entries))
	for _, entry := range b.Entries {
		if entry.ID == "" || entry.Version == "" || entry.SemanticRole == "" || entry.ContentID == "" || !validInputEntryMediaType(entry.MediaType) || entry.Classification == "" || entry.RequiredGrantAudience == "" || entry.ContentDigest.IsZero() || entry.ContentLength <= 0 || entry.ContentLength > maxInputEntryContentBytes(entry.MediaType) {
			return ErrInvalidInputBundle
		}
		if _, duplicate := entryIDs[entry.ID]; duplicate {
			return fmt.Errorf("%w: duplicate entry ID", ErrInvalidInputBundle)
		}
		entryIDs[entry.ID] = struct{}{}
		if int64(len(entry.Content)) != entry.ContentLength || runtimedomain.SHA256(entry.Content) != entry.ContentDigest {
			return fmt.Errorf("%w: entry content mismatch", ErrInvalidInputBundle)
		}
	}
	return nil
}

func validInputEntryMediaType(mediaType string) bool {
	return maxInputEntryContentBytes(mediaType) > 0
}

func maxInputEntryContentBytes(mediaType string) int64 {
	switch mediaType {
	case SettingsJSONMediaType:
		return MaxInputEntryContentBytes
	case AgentExecutionInputMediaType:
		return MaxAgentExecutionInputBytes
	case ToolkitExecuteReadInputMediaType:
		return MaxToolkitExecuteReadInputBytes
	default:
		return 0
	}
}

func (b InputBundle) Clone() InputBundle {
	b.Manifest = append([]byte(nil), b.Manifest...)
	b.Entries = append([]InputEntry(nil), b.Entries...)
	for index := range b.Entries {
		b.Entries[index].Content = append([]byte(nil), b.Entries[index].Content...)
	}
	return b
}

func (b InputBundle) entryByID(entryID string) (InputEntry, bool) {
	for _, entry := range b.Entries {
		if entry.ID == entryID {
			return entry, true
		}
	}
	return InputEntry{}, false
}

type JobState string

const (
	JobPending     JobState = "PENDING"
	JobDispatched  JobState = "DISPATCHED"
	JobClaimed     JobState = "CLAIMED"
	JobRunning     JobState = "RUNNING"
	JobSettling    JobState = "SETTLING"
	JobSucceeded   JobState = "SUCCEEDED"
	JobFailed      JobState = "FAILED"
	JobCancelled   JobState = "CANCELLED"
	JobQuarantined JobState = "QUARANTINED"
)

func (s JobState) Valid() bool {
	switch s {
	case JobPending, JobDispatched, JobClaimed, JobRunning, JobSettling, JobSucceeded, JobFailed, JobCancelled, JobQuarantined:
		return true
	default:
		return false
	}
}

type Job struct {
	ID                  string
	CommandID           string
	TenantID            string
	ResourceProjectID   string
	ProjectionProjectID string
	ActorID             string
	CapabilityID        string
	Generation          uint64
	State               JobState
	CreatedAt           time.Time
}

func (j Job) Validate() error {
	if j.ID == "" || j.CommandID == "" || j.TenantID == "" || j.ResourceProjectID == "" || j.ProjectionProjectID == "" || j.ActorID == "" {
		return ErrInvalidJob
	}
	if !SupportedCapability(j.CapabilityID) || j.Generation == 0 || !j.State.Valid() || j.State != JobPending || j.CreatedAt.IsZero() {
		return ErrInvalidJob
	}
	return nil
}

func SupportedCapability(capabilityID string) bool {
	switch capabilityID {
	case ConfigurationValidationCapability,
		IndexIngestCapability,
		AgentApplicationCapability,
		AgentAdhocCapability,
		ToolkitExecuteReadCapability,
		ToolkitCallToolCapability, ToolkitAvailableToolsCapability:
		return true
	default:
		return false
	}
}

// ToolkitExecuteReadBinding binds one direct read request to one immutable
// protobuf input. The toolkit snapshot, arguments and policy stay off Redis.
type ToolkitExecuteReadBinding struct {
	RequestEntryID string
}

func (b ToolkitExecuteReadBinding) Validate(bundle InputBundle) error {
	if b.RequestEntryID == "" || len(bundle.Entries) != 1 {
		return ErrInvalidInputBundle
	}
	entry := bundle.Entries[0]
	if entry.ID != b.RequestEntryID ||
		entry.SemanticRole != ToolkitExecuteReadRequestRole ||
		entry.MediaType != ToolkitExecuteReadInputMediaType {
		return ErrInvalidInputBundle
	}
	return nil
}

func NodeEventCapability(capabilityID string) bool {
	switch capabilityID {
	case IndexIngestCapability, AgentApplicationCapability, AgentAdhocCapability:
		return true
	default:
		return false
	}
}

type IndexIngestInitiator string

const (
	IndexIngestInitiatorUser     IndexIngestInitiator = "user"
	IndexIngestInitiatorLLM      IndexIngestInitiator = "llm"
	IndexIngestInitiatorSchedule IndexIngestInitiator = "schedule"
)

func (i IndexIngestInitiator) Valid() bool {
	switch i {
	case IndexIngestInitiatorUser, IndexIngestInitiatorLLM, IndexIngestInitiatorSchedule:
		return true
	default:
		return false
	}
}

// IndexIngestBinding binds the current index identity and typed command entry
// references to one immutable input bundle. Entry content stays in the input
// data plane and is never copied into this command metadata.
type IndexIngestBinding struct {
	ToolkitConfigurationEntryID string
	ToolParametersEntryID       string
	LLMModelEntryID             string
	LLMConfigurationEntryID     string
	MCPTokensEntryID            string
	EmbeddingBindingEntryID     string
	EmbeddingBindingDigest      runtimedomain.Digest
	ClientStreamID              string
	ClientMessageID             string
	SIOEvent                    string
	IndexMetaID                 string
	IndexMetaCorrelationID      string
	ToolkitID                   int32
	IndexName                   string
	Initiator                   IndexIngestInitiator
}

func (b IndexIngestBinding) Validate(bundle InputBundle) error {
	if b.ToolkitConfigurationEntryID == "" || b.ToolParametersEntryID == "" ||
		!validIndexMetaText(b.IndexMetaID, MaxIndexMetaIDBytes) ||
		!validIndexMetaText(b.IndexMetaCorrelationID, MaxIndexMetaCorrelationBytes) ||
		!validOptionalIndexMetaText(b.ClientStreamID, MaxIndexMetaCorrelationBytes) ||
		!validOptionalIndexMetaText(b.ClientMessageID, MaxIndexMetaCorrelationBytes) ||
		b.ToolkitID <= 0 || b.IndexName == "" || len(b.IndexName) > 256 || !b.Initiator.Valid() {
		return ErrInvalidInputBundle
	}
	if b.SIOEvent != "" && b.SIOEvent != "chat_predict" && b.SIOEvent != "test_toolkit_tool" {
		return ErrInvalidInputBundle
	}
	references := []struct {
		id       string
		role     string
		required bool
	}{
		{id: b.ToolkitConfigurationEntryID, role: IndexToolkitConfigurationRole, required: true},
		{id: b.ToolParametersEntryID, role: IndexToolParametersRole, required: true},
		{id: b.LLMModelEntryID, role: IndexLLMModelRole},
		{id: b.LLMConfigurationEntryID, role: IndexLLMConfigurationRole},
		{id: b.MCPTokensEntryID, role: IndexMCPTokensRole},
		{id: b.EmbeddingBindingEntryID, role: IndexEmbeddingBindingRole},
	}
	seen := make(map[string]struct{}, len(references))
	bound := 0
	for _, reference := range references {
		if reference.id == "" {
			if reference.required {
				return ErrInvalidInputBundle
			}
			continue
		}
		if _, duplicate := seen[reference.id]; duplicate {
			return fmt.Errorf("%w: index input entry is bound more than once", ErrInvalidInputBundle)
		}
		seen[reference.id] = struct{}{}
		entry, found := bundle.entryByID(reference.id)
		if !found || entry.SemanticRole != reference.role {
			return fmt.Errorf("%w: index input binding mismatch", ErrInvalidInputBundle)
		}
		if reference.id == b.EmbeddingBindingEntryID &&
			(b.EmbeddingBindingDigest.IsZero() || entry.ContentDigest != b.EmbeddingBindingDigest) {
			return fmt.Errorf("%w: embedding binding digest mismatch", ErrInvalidInputBundle)
		}
		bound++
	}
	if b.EmbeddingBindingEntryID == "" && !b.EmbeddingBindingDigest.IsZero() {
		return fmt.Errorf("%w: incomplete embedding binding", ErrInvalidInputBundle)
	}
	if bound != len(bundle.Entries) {
		return fmt.Errorf("%w: unbound index input entry", ErrInvalidInputBundle)
	}
	return nil
}

func validIndexMetaText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func validOptionalIndexMetaText(value string, limit int) bool {
	return value == "" || validIndexMetaText(value, limit)
}

// AgentExecutionBinding connects one immutable agent request to the bounded
// command correlation fields used by Main and the browser. The request bytes
// stay in the input data plane and are never copied into a Redis command.
type AgentExecutionBinding struct {
	RequestEntryID            string
	ClientStreamID            string
	ClientMessageID           string
	ClientExecutionGeneration string
	SIOEvent                  string
}

func (b AgentExecutionBinding) Validate(bundle InputBundle) error {
	if b.RequestEntryID == "" ||
		!validIndexMetaText(b.ClientStreamID, MaxIndexMetaCorrelationBytes) ||
		!validIndexMetaText(b.ClientMessageID, MaxIndexMetaCorrelationBytes) ||
		!validIndexMetaText(b.ClientExecutionGeneration, MaxIndexMetaCorrelationBytes) ||
		(b.SIOEvent != "chat_predict" && b.SIOEvent != "chat_continue_predict") ||
		len(bundle.Entries) != 1 {
		return ErrInvalidInputBundle
	}
	entry := bundle.Entries[0]
	if entry.ID != b.RequestEntryID ||
		entry.SemanticRole != AgentExecutionRequestRole ||
		entry.MediaType != AgentExecutionInputMediaType {
		return ErrInvalidInputBundle
	}
	return nil
}

// ToolkitCallToolBinding connects one tool run to the exact immutable entries
// the SDK call will consume.
//
// The settings and the arguments are two SEPARATE entries, not one blob,
// because they carry different trust. The settings are redeemed by this service
// from the saved toolkit row and hold credentials. The arguments come from the
// caller. Binding them apart is what lets the worker refuse a bundle that put
// caller content where the platform's own settings belong.
type ToolkitCallToolBinding struct {
	ToolkitType      string
	ToolName         string
	ToolkitID        int64
	ToolkitVersion   string
	SettingsEntryID  string
	ArgumentsEntryID string
}

func (b ToolkitCallToolBinding) Validate(bundle InputBundle) error {
	if b.ToolkitType == "" || b.ToolName == "" || b.ToolkitID <= 0 ||
		b.SettingsEntryID == "" || b.ArgumentsEntryID == "" ||
		b.SettingsEntryID == b.ArgumentsEntryID ||
		!validIndexMetaText(b.ToolkitType, MaxSafeCommandStringBytes) ||
		!validIndexMetaText(b.ToolName, MaxSafeCommandStringBytes) ||
		!validOptionalIndexMetaText(b.ToolkitVersion, MaxSafeCommandStringBytes) ||
		len(bundle.Entries) != 3 {
		return ErrInvalidInputBundle
	}
	for _, reference := range []struct {
		id   string
		role string
	}{
		{id: b.SettingsEntryID, role: ToolkitCallToolSettingsRole},
		{id: b.ArgumentsEntryID, role: ToolkitCallToolArgumentsRole},
		{id: "toolkit-runtime-context", role: ToolkitCallToolRuntimeContextRole},
	} {
		entry, found := bundle.entryByID(reference.id)
		if !found || entry.SemanticRole != reference.role {
			return fmt.Errorf("%w: tool-run input binding mismatch", ErrInvalidInputBundle)
		}
	}
	return nil
}

type OutboxRecord struct {
	ID          string
	CommandID   string
	ExecutionID string
	Generation  uint64
	CreatedAt   time.Time
}

func (o OutboxRecord) Validate() error {
	if o.ID == "" || o.CommandID == "" || o.ExecutionID == "" || o.Generation == 0 || o.CreatedAt.IsZero() {
		return ErrInvalidOutbox
	}
	return nil
}

// Admission is persisted atomically: input bytes, job and outbox are either
// all committed or all absent.
type Admission struct {
	IdempotencyScope string
	IdempotencyKey   string
	RequestDigest    runtimedomain.Digest
	InputBundle      InputBundle
	Job              Job
	Outbox           OutboxRecord
}

func (a Admission) Validate() error {
	if a.IdempotencyScope == "" || a.IdempotencyKey == "" || a.RequestDigest.IsZero() {
		return errors.New("invalid idempotency binding")
	}
	if err := a.InputBundle.Validate(); err != nil {
		return err
	}
	if err := a.Job.Validate(); err != nil {
		return err
	}
	if err := a.Outbox.Validate(); err != nil {
		return err
	}
	if a.Job.ID != a.Outbox.ExecutionID || a.Job.CommandID != a.Outbox.CommandID || a.Job.Generation != a.Outbox.Generation {
		return errors.New("job and outbox identity mismatch")
	}
	return nil
}

const ToolkitAvailableToolsCapability = "toolkit.available_tools.v1"
const ToolkitAvailableToolsSettingsRole = "toolkit.available_tools.settings"

type ToolkitAvailableToolsBinding struct {
	ToolkitType     string
	ToolkitID       int64
	ToolkitVersion  string
	SettingsEntryID string
}

func (b ToolkitAvailableToolsBinding) Validate(bundle InputBundle) error {
	if b.ToolkitID <= 0 || !validIndexMetaText(b.ToolkitType, MaxSafeCommandStringBytes) || !validOptionalIndexMetaText(b.ToolkitVersion, MaxSafeCommandStringBytes) || b.SettingsEntryID == "" || len(bundle.Entries) != 2 {
		return ErrInvalidInputBundle
	}
	entry, found := bundle.entryByID(b.SettingsEntryID)
	if !found || entry.SemanticRole != ToolkitAvailableToolsSettingsRole {
		return ErrInvalidInputBundle
	}
	contextEntry, found := bundle.entryByID("toolkit-runtime-context")
	if !found || contextEntry.SemanticRole != "toolkit.available_tools.runtime_context" {
		return ErrInvalidInputBundle
	}
	return nil
}
