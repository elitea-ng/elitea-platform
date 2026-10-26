package drafts

// schema.go — the published agent schemas (ADR-0037 decision 5).
//
// THE CONTRACT IS THE SOURCE. AgentDefinition and LocalAgentDefinition are
// components of api/openapi/v2.yaml, with examples. This file reads them out
// of the embedded spec once, serves them as JSON Schema on
// GET /agent_schema/prompt_lib/{projectID}, and compiles AgentDefinition into
// the validator both draft routes run before they answer. There is no second
// copy: no checked-in JSON fixture, no Go literal. A change to the component
// changes the route, the validator and the generated client mocks together,
// and schema_test.go fails if the component and an example disagree.
//
// Both components are written without $ref and without `nullable`, so each is
// a self-contained JSON Schema as well as an OpenAPI schema object. The
// OpenAPI-only keyword they carry (`example`) is not a JSON Schema assertion
// and the compiler ignores it.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"gopkg.in/yaml.v3"

	specfiles "github.com/elitea-ng/elitea-platform/services/elitea-main/api/openapi"
)

// The component names in v2.yaml.
const (
	agentDefinitionComponent      = "AgentDefinition"
	localAgentDefinitionComponent = "LocalAgentDefinition"
)

// DraftInvalidCode is the machine code of a generated agent draft that fails
// AgentDefinition. Both draft routes answer it: the one-shot route in a 422
// body, the streaming route in its draft.error frame.
const DraftInvalidCode = "agent_draft_invalid"

// AgentSchemas is the body of the schema route.
type AgentSchemas struct {
	// SchemaVersion is the first 12 hex characters of the SHA-256 of the two
	// schemas in canonical JSON. It changes exactly when a schema changes.
	SchemaVersion        string          `json:"schema_version"`
	AgentDefinition      json.RawMessage `json:"agent_definition"`
	LocalAgentDefinition json.RawMessage `json:"local_agent_definition"`
}

type compiledSchemas struct {
	body      AgentSchemas
	encoded   []byte
	validator *jsonschema.Schema
}

var (
	schemasOnce sync.Once
	schemas     *compiledSchemas
	schemasErr  error
)

// loadSchemas parses the embedded spec once. The spec is part of the binary,
// so a failure here is a build defect, and schema_test.go is what catches it
// before release; at runtime it answers 500 rather than a fabricated schema.
func loadSchemas() (*compiledSchemas, error) {
	schemasOnce.Do(func() { schemas, schemasErr = compileSchemas(specfiles.SpecYAML) })
	return schemas, schemasErr
}

func compileSchemas(spec []byte) (*compiledSchemas, error) {
	var document struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(spec, &document); err != nil {
		return nil, fmt.Errorf("drafts: parse the embedded spec: %w", err)
	}
	agent, err := componentJSON(document.Components.Schemas, agentDefinitionComponent)
	if err != nil {
		return nil, err
	}
	local, err := componentJSON(document.Components.Schemas, localAgentDefinitionComponent)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	digest.Write(agent)
	digest.Write([]byte{0})
	digest.Write(local)

	body := AgentSchemas{
		SchemaVersion:        hex.EncodeToString(digest.Sum(nil))[:12],
		AgentDefinition:      agent,
		LocalAgentDefinition: local,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("drafts: encode the agent schemas: %w", err)
	}

	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(agent))
	if err != nil {
		return nil, fmt.Errorf("drafts: decode %s: %w", agentDefinitionComponent, err)
	}
	compiler := jsonschema.NewCompiler()
	const url = "elitea:agent-definition.json"
	if err := compiler.AddResource(url, instance); err != nil {
		return nil, fmt.Errorf("drafts: add %s: %w", agentDefinitionComponent, err)
	}
	validator, err := compiler.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("drafts: compile %s: %w", agentDefinitionComponent, err)
	}
	return &compiledSchemas{body: body, encoded: encoded, validator: validator}, nil
}

// componentJSON returns one component as JSON. encoding/json sorts map keys,
// so the bytes are canonical for the same YAML.
func componentJSON(components map[string]any, name string) ([]byte, error) {
	component, ok := components[name]
	if !ok {
		return nil, fmt.Errorf("drafts: v2.yaml has no %s component", name)
	}
	encoded, err := json.Marshal(component)
	if err != nil {
		return nil, fmt.Errorf("drafts: encode %s: %w", name, err)
	}
	if bytes.Contains(encoded, []byte(`"$ref"`)) {
		return nil, fmt.Errorf("drafts: %s must be self-contained (no $ref)", name)
	}
	return encoded, nil
}

// definitionOf projects a draft onto AgentDefinition: the fields the model
// wrote. The suggestion lists are Go-side matches, not part of the definition.
func definitionOf(draft ApplicationDraft) map[string]any {
	starters := make([]any, 0, len(draft.ConversationStarters))
	for _, starter := range draft.ConversationStarters {
		starters = append(starters, starter)
	}
	return map[string]any{
		"name":                  draft.Name,
		"description":           draft.Description,
		"instructions":          draft.Instructions,
		"welcome_message":       draft.WelcomeMessage,
		"conversation_starters": starters,
	}
}

// ValidateApplicationDraft checks a draft against AgentDefinition. Both draft
// routes call it after truncation, so they accept and refuse the same drafts.
func ValidateApplicationDraft(draft ApplicationDraft) error {
	compiled, err := loadSchemas()
	if err != nil {
		return err
	}
	if err := compiled.validator.Validate(definitionOf(draft)); err != nil {
		var validation *jsonschema.ValidationError
		if errors.As(err, &validation) {
			return errors.New(validationSummary(validation))
		}
		return err
	}
	return nil
}

// validationSummary names the failing fields without echoing their values: a
// value is model output and can be long.
func validationSummary(err *jsonschema.ValidationError) string {
	fields := map[string]struct{}{}
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, cause := range e.Causes {
				walk(cause)
			}
			return
		}
		if required, ok := e.ErrorKind.(*kind.Required); ok {
			for _, missing := range required.Missing {
				fields[strings.Join(append(append([]string{}, e.InstanceLocation...), missing), ".")+" (missing)"] = struct{}{}
			}
			return
		}
		fields[strings.Join(e.InstanceLocation, ".")] = struct{}{}
	}
	walk(err)
	names := make([]string, 0, len(fields))
	for field := range fields {
		names = append(names, field)
	}
	sort.Strings(names)
	return "the draft does not match AgentDefinition: " + strings.Join(names, ", ")
}

// GetAgentSchema serves GET /agent_schema/prompt_lib/{projectID}. It reads no
// tenant state: every authorized project gets the same bytes.
func (h *Handler) GetAgentSchema(w http.ResponseWriter, _ *http.Request) {
	compiled, err := loadSchemas()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the agent schemas could not be loaded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"`+compiled.body.SchemaVersion+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(compiled.encoded)
}
