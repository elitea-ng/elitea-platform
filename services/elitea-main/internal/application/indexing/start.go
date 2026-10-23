package indexing

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

const (
	IndexDataToolName         = "index_data"
	MaxToolParametersBytes    = executiondomain.MaxInputEntryContentBytes
	MaxRequestedLLMBytes      = 128 << 10
	MaxClientCorrelationBytes = 512
	MaxCurrentIndexNameRunes  = 32
	CurrentIndexSIOEvent      = "chat_predict"
	TestToolkitSIOEvent       = "test_toolkit_tool"
)

var (
	ErrInvalidIndexStart = errors.New("invalid index start request")
	ErrToolkitNotVisible = errors.New("toolkit is not visible in the requested project")
)

// StartRequest contains only caller-controlled invocation data and stable
// server-derived identities. Toolkit settings, deployment tokens and expanded
// secrets are deliberately absent. RequestedLLMSettings is an untrusted UI
// preference object; the injected use case must allowlist it, resolve the
// visible/default model name and derived invocation metadata from Configurations,
// and check toolkit visibility. AI provider credentials remain behind Main's
// authenticated LiteLLM facade and never enter this worker command.
type StartRequest struct {
	ProjectID            int64
	ActorUserID          int64
	ToolkitID            int64
	ToolParameters       json.RawMessage
	RequestedLLMModel    *string
	RequestedLLMSettings json.RawMessage
	StreamID             string
	MessageID            string
	SIOEvent             string
}

func (r StartRequest) Validate() error {
	if r.ProjectID <= 0 || r.ActorUserID <= 0 || r.ToolkitID <= 0 {
		return ErrInvalidIndexStart
	}
	if !validJSONObject(r.ToolParameters, MaxToolParametersBytes) ||
		!validJSONObject(r.RequestedLLMSettings, MaxRequestedLLMBytes) {
		return ErrInvalidIndexStart
	}
	if _, err := indexNameFromToolParameters(r.ToolParameters); err != nil {
		return ErrInvalidIndexStart
	}
	if r.RequestedLLMModel != nil && !validOptionalText(*r.RequestedLLMModel, MaxClientCorrelationBytes) {
		return ErrInvalidIndexStart
	}
	if !validOptionalText(r.StreamID, MaxClientCorrelationBytes) ||
		!validOptionalText(r.MessageID, MaxClientCorrelationBytes) {
		return ErrInvalidIndexStart
	}
	if !validIndexSIOEvent(r.SIOEvent) {
		return ErrInvalidIndexStart
	}
	return nil
}

func validIndexSIOEvent(value string) bool {
	return value == "" || value == CurrentIndexSIOEvent || value == TestToolkitSIOEvent
}

// Clone prevents a use case from retaining aliases into an HTTP decoder's
// buffers. Requested LLM settings remain untrusted preferences; a use case may
// allowlist them but must never treat them as credential material.
func (r StartRequest) Clone() StartRequest {
	r.ToolParameters = bytes.Clone(r.ToolParameters)
	r.RequestedLLMSettings = bytes.Clone(r.RequestedLLMSettings)
	if r.RequestedLLMModel != nil {
		model := *r.RequestedLLMModel
		r.RequestedLLMModel = &model
	}
	return r
}

type StartOutcome struct {
	TaskID  string
	Created bool
}

// ScheduledStartRequest is accepted only from Main's schedule executor after
// current Configurations and the index's registry state have been resolved. Inputs must
// contain frozen references; transient unsecreted preflight values must never
// be persisted in this request.
type ScheduledStartRequest struct {
	ProjectID              int64
	AttributionActorUserID int64
	ToolkitID              int64
	Inputs                 AuthoritativeInputs
	IdempotencyKey         string
	CorrelationID          string
}

func (request ScheduledStartRequest) Validate() error {
	if request.ProjectID <= 0 ||
		request.AttributionActorUserID <= 0 ||
		request.ToolkitID <= 0 ||
		request.IdempotencyKey == "" ||
		len(request.IdempotencyKey) > 200 ||
		request.CorrelationID == "" ||
		!validOptionalText(request.CorrelationID, MaxClientCorrelationBytes) ||
		request.Inputs.validate() != nil {
		return ErrInvalidIndexStart
	}
	if _, err := indexNameFromToolParameters(
		request.Inputs.ToolParameters,
	); err != nil {
		return ErrInvalidIndexStart
	}
	return nil
}

func validJSONObject(value []byte, limit int) bool {
	if len(value) == 0 || len(value) > limit || !json.Valid(value) {
		return false
	}
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func validOptionalText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func indexNameFromToolParameters(parameters []byte) (string, error) {
	var value struct {
		IndexName string `json:"index_name"`
	}
	if err := json.Unmarshal(parameters, &value); err != nil {
		return "", ErrInvalidIndexStart
	}
	length := utf8.RuneCountInString(value.IndexName)
	if length < 1 || length > MaxCurrentIndexNameRunes || strings.TrimSpace(value.IndexName) == "" {
		return "", ErrInvalidIndexStart
	}
	return value.IndexName, nil
}
