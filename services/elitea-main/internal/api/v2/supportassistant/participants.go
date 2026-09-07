package supportassistant

// THE TWO PARTICIPANT ROWS A SUPPORT TURN CANNOT RUN WITHOUT.
//
// `ResolveCurrentApplicationTurn` (internal/db/queries/agent_chat.sql) is not a
// lookup with optional joins. It is one row or nothing, and it demands:
//
//	JOIN chat_participants AS author_participant
//	  ON  ... entity_name = 'user'
//	  AND (entity_meta ->> 'id')::bigint = <caller>
//	JOIN application_versions AS application_version
//	  ON  application_version.id = (target_mapping.entity_settings ->> 'version_id')::integer
//
// So a support conversation needs a `user` participant FOR THE CALLER, and its
// agent mapping needs an `entity_settings.version_id`. This package wrote
// neither: `CreateConversation` inserted a conversation and stopped, and
// `ensureAgentParticipant` passed `entity_name` and `entity_meta` only, which
// `ConversationsRepo.AddParticipant` stores with `entity_settings = '{}'`.
// Both halves were individually correct; the wiring was the whole defect, and
// every support question answered 502.
//
// # Why the repair path exists
//
// The mapping insert is
// `ON CONFLICT (participant_id, conversation_id) DO NOTHING`
// (repos/conversations.go). A conversation that already holds the `{}` mapping
// therefore cannot be corrected by attaching the agent again — the insert is a
// no-op and the empty document survives. Every deployment that tried the
// assistant before this change has such rows. So the attach path READS what is
// there and UPDATEs it when it is wrong, rather than trusting the insert.
//
// # Why the version is resolved per turn
//
// The operator configures an AGENT, not a version. The version is resolved on
// every turn (store.agentVersionOf) so that publishing a new version of the
// support agent takes effect on the next message, instead of pinning the
// assistant to whatever version existed when somebody first asked a question.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// applicationEntityName is the `chat_participants.entity_name` an agent
// participant carries, matching what the chat surface writes.
const applicationEntityName = "application"

// userEntityName is the `chat_participants.entity_name` the turn resolver's
// author join demands. It is a literal in that SQL, so it is a literal here.
const userEntityName = "user"

// errAgentProjectNotSupportProject reports the one configuration this facade
// cannot serve.
//
// A turn reads `application_versions` from the tenant schema of the project it
// runs in, and a support turn runs in the SUPPORT project because that is where
// its conversation lives. The resolver also refuses when the target
// participant's `entity_meta.project_id` is not that project. So an agent kept
// in another project cannot answer, whatever this package writes.
//
// The reference allows the split (`agent_project_id or support_project_id`)
// because its RPC layer opens a session per project. This service's turn
// contract does not carry a second project, so the honest answer is to refuse
// and say so, rather than to attach a participant that will resolve to nothing.
var errAgentProjectNotSupportProject = errors.New(
	"support assistant: the configured agent project is not the support project; " +
		"the answering agent must live in the support project")

// ensureTurnParticipants makes the conversation resolvable by the turn resolver
// and returns the participant id the turn is addressed to.
//
// It runs on EVERY predict, not only on the first, for the same reason the
// reference re-attaches its application participant on every message: an
// operator can repoint the assistant at another agent, publish a new version,
// or fix a conversation that was created while this package was writing
// incomplete rows.
func (h *Handler) ensureTurnParticipants(
	ctx context.Context,
	settings platformconfig.SupportAssistant,
	conversationID, userID int64,
) (int64, error) {
	projectKey := strconv.FormatInt(settings.ProjectID, 10)
	conversationKey := strconv.FormatInt(conversationID, 10)

	if err := h.ensureUserParticipant(ctx, projectKey, conversationKey, settings.ProjectID, userID); err != nil {
		return 0, err
	}
	version, err := h.resolveAnsweringVersion(ctx, settings)
	if err != nil {
		return 0, err
	}
	return h.attachAgentParticipant(ctx, settings, conversationKey, version)
}

// resolveAnsweringVersion decides which version of the configured agent answers,
// and refuses the one configuration this facade cannot serve.
func (h *Handler) resolveAnsweringVersion(
	ctx context.Context, settings platformconfig.SupportAssistant,
) (agentVersion, error) {
	if h.chat == nil {
		return agentVersion{}, errChatStoreUnavailable
	}
	if settings.AgentProject() != settings.ProjectID {
		return agentVersion{}, errAgentProjectNotSupportProject
	}
	return h.store.agentVersionOf(ctx, settings.ProjectID, settings.AgentID)
}

// ensureUserParticipant attaches the caller to the conversation as its author.
//
// The support routes never take a `{projectID}`, so the caller is the only
// person who can ever author in their own support conversation, and the row is
// written with the SAME shape the chat surface's REST attach path produces:
// `entity_meta.id` for identity plus the project id that path defaults in
// (conversations/handler.go's defaultEntityProjectID). One shape means one
// participant row per user, whichever surface created it.
func (h *Handler) ensureUserParticipant(
	ctx context.Context, projectKey, conversationKey string, projectID, userID int64,
) error {
	if h.chat == nil {
		return errChatStoreUnavailable
	}
	participants, err := h.chat.ListParticipants(ctx, projectKey, conversationKey)
	if err != nil {
		return err
	}
	for _, participant := range participants {
		if participant.EntityName == userEntityName && metaInt(participant.EntityMeta, "id") == userID {
			return nil
		}
	}
	return h.chat.AddParticipant(ctx, projectKey, conversationKey, map[string]any{
		"entity_name": userEntityName,
		"entity_meta": map[string]any{
			"id":         userID,
			"project_id": projectID,
		},
	})
}

// errChatStoreUnavailable is the programming-error case: a deployment that
// wired no chat repository. The routes already answer 503 before reaching here.
var errChatStoreUnavailable = errors.New("support assistant: chat store is not wired")

// attachAgentParticipant attaches the configured agent to the conversation with
// the entity settings the turn resolver reads, and returns its participant id.
//
// The version arrives as an argument rather than being read here so that the
// two decisions stay separable: WHICH version answers is a store read, and
// WHETHER this conversation already carries it is participant bookkeeping.
func (h *Handler) attachAgentParticipant(
	ctx context.Context,
	settings platformconfig.SupportAssistant,
	conversationKey string,
	version agentVersion,
) (int64, error) {
	if h.chat == nil {
		return 0, errChatStoreUnavailable
	}
	projectKey := strconv.FormatInt(settings.ProjectID, 10)
	wanted := agentEntitySettings(version)

	participantID, current, found, err := h.findAgentParticipant(
		ctx, projectKey, conversationKey, settings.AgentID, settings.ProjectID)
	if err != nil {
		return 0, err
	}
	if !found {
		if err := h.chat.AddParticipant(ctx, projectKey, conversationKey, map[string]any{
			"entity_name": applicationEntityName,
			"entity_meta": map[string]any{
				"id":         settings.AgentID,
				"project_id": settings.ProjectID,
			},
			"entity_settings": wanted,
		}); err != nil {
			return 0, err
		}
		participantID, current, found, err = h.findAgentParticipant(
			ctx, projectKey, conversationKey, settings.AgentID, settings.ProjectID)
		if err != nil {
			return 0, err
		}
		if !found {
			// The insert reported success and the row is not there. Rather than
			// starting a turn addressed to participant 0, refuse.
			return 0, errors.New("support assistant: agent participant not found after attach")
		}
	}

	// THE REPAIR. `AddParticipant`'s mapping insert is ON CONFLICT DO NOTHING,
	// so a mapping that already exists keeps whatever entity_settings it was
	// created with — including the `{}` every conversation opened before this
	// change carries. Without this update those conversations stay permanently
	// unanswerable, and the user sees a widget that fails on send forever.
	if repaired, changed := mergeAgentEntitySettings(current, wanted); changed {
		if err := h.chat.UpdateEntitySettings(ctx, projectKey, conversationKey,
			strconv.FormatInt(participantID, 10), repaired); err != nil {
			return 0, err
		}
	}
	return participantID, nil
}

// agentEntitySettings is the document the turn resolver reads off the agent's
// mapping row, in the shape the chat surface writes:
//
//	{"icon_meta":{},"variables":[],"agent_type":"openai","version_id":"1"}
//
// `version_id` is a STRING because that is what the chat surface stores and
// what the resolver's `(entity_settings ->> 'version_id')::integer` reads back
// either way. One spelling across both surfaces keeps a transcript readable by
// whichever one opens it.
func agentEntitySettings(version agentVersion) map[string]any {
	return map[string]any{
		"version_id": strconv.FormatInt(version.ID, 10),
		"agent_type": version.AgentType,
		"variables":  []any{},
		"icon_meta":  map[string]any{},
	}
}

// mergeAgentEntitySettings decides whether a stored mapping needs correcting,
// and what to write.
//
// It MERGES rather than replaces. `UpdateEntitySettings` overwrites the whole
// document, and a conversation may legitimately carry keys this package does
// not manage — `llm_settings` from an operator's own edit, a
// `chat_history_template`. Replacing wholesale would silently drop them. Only
// the keys a turn cannot resolve without are forced.
func mergeAgentEntitySettings(current, wanted map[string]any) (map[string]any, bool) {
	merged := make(map[string]any, len(current)+len(wanted))
	for key, value := range current {
		merged[key] = value
	}
	changed := false
	// version_id and agent_type are AUTHORITATIVE: the resolved version wins
	// over whatever a previous turn wrote, so a republished agent takes effect.
	// An EMPTY resolved value is not a correction, though — `agent_type` is
	// COALESCEd out of a nullable column, and writing "" over a stored
	// "openai" would lose information the read simply did not have.
	for _, key := range []string{"version_id", "agent_type"} {
		if text, isText := wanted[key].(string); isText && text == "" {
			continue
		}
		if !sameJSONValue(merged[key], wanted[key]) {
			merged[key] = wanted[key]
			changed = true
		}
	}
	// variables and icon_meta are DEFAULTED, never overwritten: they carry the
	// conversation's own state once something has written them.
	for _, key := range []string{"variables", "icon_meta"} {
		if _, present := merged[key]; !present {
			merged[key] = wanted[key]
			changed = true
		}
	}
	return merged, changed
}

// sameJSONValue compares two decoded JSON scalars without caring whether the
// stored one arrived as a number, a string or a float64. `entity_settings` is a
// jsonb column read back through `encoding/json`, so `1`, `"1"` and `1.0` are
// all spellings of the same version id, and treating them as different would
// make every turn write an UPDATE that changes nothing.
func sameJSONValue(stored, wanted any) bool {
	if stored == nil || wanted == nil {
		return stored == nil && wanted == nil
	}
	if storedNumber, ok := metaValueInt(stored); ok {
		if wantedNumber, ok := metaValueInt(wanted); ok {
			return storedNumber == wantedNumber
		}
		return false
	}
	storedText, storedIsText := stored.(string)
	wantedText, wantedIsText := wanted.(string)
	return storedIsText && wantedIsText && storedText == wantedText
}

// findAgentParticipant looks for the agent among the conversation's
// participants, and returns its stored entity settings with it.
//
// The match is on ENTITY IDENTITY (`application` + agent id + agent project),
// not on position or on name, because a conversation legitimately holds several
// participants — the user, the agent, and whatever an operator's repointing has
// left behind — and picking the wrong one sends the question to a different
// agent. The comparison goes through `json.Number`-tolerant reads because
// `entity_meta` is a JSON document whose numbers arrive as float64.
func (h *Handler) findAgentParticipant(
	ctx context.Context, projectKey, conversationKey string, agentID, agentProjectID int64,
) (int64, map[string]any, bool, error) {
	participants, err := h.chat.ListParticipants(ctx, projectKey, conversationKey)
	if err != nil {
		return 0, nil, false, err
	}
	for _, participant := range participants {
		if participant.EntityName != applicationEntityName {
			continue
		}
		if metaInt(participant.EntityMeta, "id") != agentID {
			continue
		}
		// A participant written before an operator set `agent_project_id`
		// carries no project. It still identifies the same agent in the same
		// project, so an ABSENT project matches; a DIFFERENT one does not.
		if project, present := metaIntPresent(participant.EntityMeta, "project_id"); present && project != agentProjectID {
			continue
		}
		return int64(participant.ID), participant.EntitySettings, true, nil
	}
	return 0, nil, false, nil
}

func metaInt(meta map[string]any, key string) int64 {
	value, _ := metaIntPresent(meta, key)
	return value
}

func metaIntPresent(meta map[string]any, key string) (int64, bool) {
	raw, ok := meta[key]
	if !ok || raw == nil {
		return 0, false
	}
	return metaValueInt(raw)
}

func metaValueInt(raw any) (int64, bool) {
	switch typed := raw.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case json.Number:
		value, err := typed.Int64()
		return value, err == nil
	case string:
		value, err := strconv.ParseInt(typed, 10, 64)
		return value, err == nil
	}
	return 0, false
}
