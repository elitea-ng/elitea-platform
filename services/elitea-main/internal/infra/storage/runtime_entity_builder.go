package storage

import (
	"context"
	"errors"
	"math"
	"strings"
	"unicode/utf8"
)

// The two chat-authored builder modules (#940 A8): `skills_builder` and
// `project_context_builder`. A conversation that enables one of them gives the
// model a tool that WRITES a product entity — a Skill, or the project's
// Project Context — from the conversation itself.
//
// They are served here, on the private claim-bound mTLS listener, for the same
// reason the attachment read is (runtime_attachment_object.go's own doc
// comment): the native runtime holds no vault, materializes no toolkit family
// that could reach the public API, and its egress allowlist reaches the model
// gateway alone. A tool that could not write would be a toggle that does
// nothing, which is the exact failure #866 existed to end.
//
// WHAT AUTHORIZES THE WRITE. Nothing the request carries. The project is the
// claimed execution's own `resource_project_id`, resolved by the AGENT-scoped
// authorizer (so an `index.ingest.v1` claim — which carries a perfectly real
// project — is refused before any statement runs), and the request selects
// only the CONTENT. There is no project, user, or entity id on the wire that
// could point the write somewhere else. This is the same boundary
// RuntimeApplicationVersionService draws for its read, applied to a write:
// authorize first, take the project only from what the claim resolved.
//
// WHY THERE IS NO PERMISSION CHECK ON THE CALLER'S BEHALF. There is no caller
// in the HTTP sense — the peer is a workload certificate, and the durable
// claim is the authorization. The turn that reached this tool was itself
// admitted against the conversation's project, and the tool is only bound at
// all when the conversation's `meta.internal_tools` names the module
// (internal/application/agentexecution/start.go's currentRuntimeInternalTools,
// and the SQL admission in internal/db/queries/agent_chat.sql). A user who
// cannot open a conversation in a project cannot make a turn run there, and a
// turn cannot reach a project other than its own.
const (
	// RuntimeSkillWriteSchemaVersion and RuntimeProjectContextWriteSchemaVersion
	// are the exact discriminators the worker compares before it will read a
	// response body at all (SKILL_WRITE_SCHEMA / PROJECT_CONTEXT_WRITE_SCHEMA,
	// services/elitea-worker-rust/src/transport/runtime_context.rs).
	RuntimeSkillWriteSchemaVersion          = "elitea.runtime.skill-write.v1"
	RuntimeProjectContextWriteSchemaVersion = "elitea.runtime.project-context-write.v1"

	// maxRuntimeBuilderRequestBytes bounds the REQUEST body. A skill's
	// instructions and a project context are both prompt-sized documents a
	// model composed inside one turn; 64 KiB is already far more than any
	// turn can afford to have written back into it, and a larger body is
	// REFUSED rather than truncated — half an instruction set saved as though
	// it were the whole one is the kind of silent corruption a user only finds
	// later, from the model behaving oddly.
	maxRuntimeBuilderRequestBytes = 64 * 1024

	// maxRuntimeBuilderResponseBytes is the envelope ceiling, restated on this
	// side so the server REFUSES rather than sends a body the worker will
	// reject after buffering it (MAX_BUILDER_RESPONSE_BYTES on the worker).
	maxRuntimeBuilderResponseBytes = 128 * 1024

	maxRuntimeSkillNameBytes         = 128
	maxRuntimeSkillDescriptionBytes  = 4 * 1024
	maxRuntimeSkillInstructionsBytes = 48 * 1024
	maxRuntimeProjectContextBytes    = 48 * 1024

	runtimeContextStageSkillWrite          = "skill_write"
	runtimeContextStageProjectContextWrite = "project_context_write"
	runtimeContextStageProjectContextRead  = "project_context_read"
)

// RuntimeSkillWriteRequest is the wire body the worker POSTs for
// `skills_builder`.
//
// There is deliberately no `id` and no `project_id`. WHICH skill is selected
// by NAME inside the claim's own project — an existing name updates that
// skill's `base` version, a new name creates one — because a model that could
// name a row id could name somebody else's, and because "update the skill I
// just made" is a name in the conversation, not an id the model reliably
// carries. Case ELITEA-2787's duplicate-free update is the same rule applied
// to the project context, which has exactly one row per project anyway.
type RuntimeSkillWriteRequest struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
}

// RuntimeProjectContextWriteRequest is the wire body for
// `project_context_builder`.
//
// `Enabled` is a POINTER so "the model did not say" is distinguishable from
// "the model said false". An update that omits it keeps whatever the project
// already had — a builder tool that silently switched the context OFF while
// editing its text would remove it from every prompt in the project, which is
// the opposite of what the user asked for.
type RuntimeProjectContextWriteRequest struct {
	Content string `json:"content"`
	Enabled *bool  `json:"enabled"`
}

// RuntimeSkillWriteContext is the response document. Its fields are the
// complete set the worker accepts (`SkillWriteResponse` is
// `deny_unknown_fields`), so one extra key here fails every skill write with a
// malformed-response error that names nothing.
type RuntimeSkillWriteContext struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	SkillID       string `json:"skill_id"`
	Name          string `json:"name"`
	// Created distinguishes a new skill from an updated one so the tool's own
	// answer to the model can say which happened — the user asked for one or
	// the other and a wrong verb reads as a bug even when the row is right.
	Created bool `json:"created"`
}

// RuntimeProjectContextWriteContext is the project-context response document.
//
// It deliberately does NOT echo the stored content back. The worker caps a
// runtime-context response at 32 KiB (MAX_RUNTIME_CONTEXT_BYTES,
// services/elitea-worker-rust/src/bootstrap.rs) while a context may be 48 KiB,
// so echoing it would make every large write fail on the reply to a write that
// already succeeded — the worst possible failure mode, since the tool would
// report an error the model would then retry into a duplicate. ContentBytes is
// what the tool's own answer needs anyway: the model wrote the text, it does
// not need to be told it back.
type RuntimeProjectContextWriteContext struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	ContentBytes  int    `json:"content_bytes"`
	Enabled       bool   `json:"enabled"`
	Created       bool   `json:"created"`
}

// RuntimeSkillRecord is one written skill as the tenant schema holds it.
type RuntimeSkillRecord struct {
	SkillID string
	Name    string
	Created bool
}

// RuntimeProjectContextRecord is one project's context row.
type RuntimeProjectContextRecord struct {
	Content string
	Enabled bool
	Found   bool
}

// RuntimeSkillSink writes one skill inside the project the CLAIM selected.
// Implementations must not accept a project from the request: the caller
// passes the authorized one.
type RuntimeSkillSink interface {
	UpsertRuntimeSkillByName(
		ctx context.Context,
		projectID int64,
		name string,
		description string,
		instructions string,
	) (RuntimeSkillRecord, error)
}

// RuntimeProjectContextSink reads and writes one project's context row.
//
// The READ is on the interface because an update has to be able to keep what
// it is not changing: `enabled` when the model did not state one, and the
// existing text when the model asked to extend rather than replace it.
type RuntimeProjectContextSink interface {
	ReadRuntimeProjectContext(ctx context.Context, projectID int64) (RuntimeProjectContextRecord, error)
	WriteRuntimeProjectContext(ctx context.Context, projectID int64, content string, enabled bool) error
}

// RuntimeEntityBuilderService serves both builder writes under one durable
// claim. The two live on one service (rather than two, the way the two READ
// routes are split) because they share the whole authorization path and
// nothing else: splitting them would duplicate it twice over with no
// difference between the copies, and the composition root already has three
// constructor branches to pick between.
type RuntimeEntityBuilderService struct {
	authorizer     AgentRuntimeContextAuthorizer
	skills         RuntimeSkillSink
	projectContext RuntimeProjectContextSink
}

func NewRuntimeEntityBuilderService(
	authorizer AgentRuntimeContextAuthorizer,
	skills RuntimeSkillSink,
	projectContext RuntimeProjectContextSink,
) (*RuntimeEntityBuilderService, error) {
	if authorizer == nil || skills == nil || projectContext == nil {
		return nil, errors.New("runtime entity builder dependencies are required")
	}
	return &RuntimeEntityBuilderService{
		authorizer:     authorizer,
		skills:         skills,
		projectContext: projectContext,
	}, nil
}

// authorizeProject runs the shared half of both writes: the agent-scoped claim
// check and the project identity it resolves. It returns the project id and
// nothing else, so neither caller can accidentally read a request-supplied
// value in its place.
func (service *RuntimeEntityBuilderService) authorizeProject(
	ctx context.Context,
	claim ContentClaim,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	authorization, err := service.authorizer.AuthorizeAgentRuntimeContext(ctx, claim)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return 0, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) {
			return 0, ErrContentUnauthorized
		}
		return 0, runtimeContextUnavailable(runtimeContextStageClaimAuthorize)
	}
	if authorization.ResourceProjectID <= 0 || authorization.ResourceProjectID > math.MaxInt32 {
		return 0, runtimeContextUnavailable(runtimeContextStageProjectIdentity)
	}
	return authorization.ResourceProjectID, nil
}

// WriteSkill creates or updates one skill for the claimed execution.
//
// ErrContentRejected is the taxonomy's one addition and it means "the request
// is exactly what it claims to be and this route will never write it" —
// an empty name, a name or body over the cap, or bytes that are not UTF-8.
// It is 422 on the wire, and the worker turns it into a tool RESULT the model
// can read and retry from, never a failed turn: a model that wrote too much is
// able to write less, and killing the turn would lose the conversation that
// produced the draft.
func (service *RuntimeEntityBuilderService) WriteSkill(
	ctx context.Context,
	claim ContentClaim,
	request RuntimeSkillWriteRequest,
) (RuntimeSkillWriteContext, error) {
	if service == nil || service.authorizer == nil || service.skills == nil {
		return RuntimeSkillWriteContext{}, runtimeContextUnavailable(runtimeContextStageSkillWrite)
	}
	name := strings.TrimSpace(request.Name)
	description := strings.TrimSpace(request.Description)
	instructions := request.Instructions
	if !boundedBuilderText(name, maxRuntimeSkillNameBytes) || name == "" ||
		!boundedBuilderText(description, maxRuntimeSkillDescriptionBytes) ||
		!boundedBuilderText(instructions, maxRuntimeSkillInstructionsBytes) ||
		strings.TrimSpace(instructions) == "" {
		return RuntimeSkillWriteContext{}, ErrContentRejected
	}
	projectID, err := service.authorizeProject(ctx, claim)
	if err != nil {
		return RuntimeSkillWriteContext{}, err
	}
	record, err := service.skills.UpsertRuntimeSkillByName(
		ctx, projectID, name, description, instructions,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeSkillWriteContext{}, contextErr
		}
		if errors.Is(err, ErrContentRejected) {
			return RuntimeSkillWriteContext{}, ErrContentRejected
		}
		return RuntimeSkillWriteContext{}, runtimeContextUnavailable(runtimeContextStageSkillWrite)
	}
	if record.SkillID == "" {
		return RuntimeSkillWriteContext{}, runtimeContextUnavailable(runtimeContextStageSkillWrite)
	}
	return RuntimeSkillWriteContext{
		SchemaVersion: RuntimeSkillWriteSchemaVersion,
		ProjectID:     projectID,
		SkillID:       record.SkillID,
		Name:          record.Name,
		Created:       record.Created,
	}, nil
}

// WriteProjectContext replaces the claimed project's context row.
//
// There is exactly one context row per project (the `configuration` row with
// `type = 'project_context'`, which eliteacore's own handler upserts the same
// way), so "update instead of duplicating" — case ELITEA-2787 — is a property
// of the storage shape rather than of a lookup this route performs. What this
// route has to get right is the OTHER half: an update that does not state
// `enabled` must keep the project's current value, not default it.
func (service *RuntimeEntityBuilderService) WriteProjectContext(
	ctx context.Context,
	claim ContentClaim,
	request RuntimeProjectContextWriteRequest,
) (RuntimeProjectContextWriteContext, error) {
	if service == nil || service.authorizer == nil || service.projectContext == nil {
		return RuntimeProjectContextWriteContext{}, runtimeContextUnavailable(
			runtimeContextStageProjectContextWrite,
		)
	}
	content := request.Content
	if !boundedBuilderText(content, maxRuntimeProjectContextBytes) || strings.TrimSpace(content) == "" {
		return RuntimeProjectContextWriteContext{}, ErrContentRejected
	}
	projectID, err := service.authorizeProject(ctx, claim)
	if err != nil {
		return RuntimeProjectContextWriteContext{}, err
	}
	existing, err := service.projectContext.ReadRuntimeProjectContext(ctx, projectID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeProjectContextWriteContext{}, contextErr
		}
		return RuntimeProjectContextWriteContext{}, runtimeContextUnavailable(
			runtimeContextStageProjectContextRead,
		)
	}
	// Absent `enabled` keeps what the project already had; a project with no
	// row at all gets `true`, because a context written from chat that is not
	// in effect is indistinguishable, from the user's side, from one that was
	// never written.
	enabled := true
	if existing.Found {
		enabled = existing.Enabled
	}
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	if err := service.projectContext.WriteRuntimeProjectContext(ctx, projectID, content, enabled); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeProjectContextWriteContext{}, contextErr
		}
		return RuntimeProjectContextWriteContext{}, runtimeContextUnavailable(
			runtimeContextStageProjectContextWrite,
		)
	}
	return RuntimeProjectContextWriteContext{
		SchemaVersion: RuntimeProjectContextWriteSchemaVersion,
		ProjectID:     projectID,
		ContentBytes:  len(content),
		Enabled:       enabled,
		Created:       !existing.Found,
	}, nil
}

// boundedBuilderText is the one content rule both writes share: real UTF-8,
// within the cap, no NUL. It accepts the empty string — each caller decides
// separately whether empty is allowed for its own field, because a blank
// description is fine and a blank skill name is not.
func boundedBuilderText(value string, maxBytes int) bool {
	return len(value) <= maxBytes &&
		utf8.ValidString(value) &&
		!strings.ContainsRune(value, 0)
}
