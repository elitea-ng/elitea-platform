package agentexecution

import (
	"context"
	"errors"
)

// PEOPLE-ONLY MESSAGES.
//
// A chat message whose only addressees are people (it tags project members
// with `@user` or `@everyone` and addresses no agent) is a message to those
// people. It is stored in the conversation as the sender's message and
// notifies the people it names; it starts NO turn: not the conversation's
// default model, not the agent the history last addressed, and not a lead
// agent. A model that answered it spoke on behalf of the person who was
// asked.
//
// The decision is the server's. A client says whom a message is for with
// `addressed_to` (the agents) and `user_ids` / `is_mentioning_everyone` (the
// people); the start route calls PeopleOnly on exactly those fields, before
// it looks at a participant or an execution contract, because
// `participant_id` is also what a client fills in for an unaddressed
// message (the picked or the last addressed agent), so it cannot say that
// an agent was addressed. A message with no mention is unchanged, and a
// message that addresses an agent goes to that agent whether or not it also
// mentions people.

// ErrPeopleMessagesUnavailable refuses a people-only message on a service
// that cannot store one (no HumanMessageStore). The route answers 422
// `unsupported_agent_execution`: starting a turn instead would be the
// defect this file exists to end.
var ErrPeopleMessagesUnavailable = errors.New("people-only messages are not available")

// PeopleOnly reports whether a message is for people alone: it mentions at
// least one person (named, or `@everyone`) and addresses no agent.
func PeopleOnly(mentionedUserIDs []int64, mentionsEveryone bool, addressedTo []int64) bool {
	return (len(mentionedUserIDs) > 0 || mentionsEveryone) && len(addressedTo) == 0
}

// IsPeopleOnly decides a start on its EFFECTIVE audience: the wire rule
// (PeopleOnly) and at least one person who would really be told, the project
// members the message names other than its sender (mentionAudience, the rule
// the notifications use) or `@everyone` of a project that has other members.
// A message that tags only people who are not members, or only its sender,
// has no people to talk to and is an ordinary message. When the membership
// cannot be read the wire rule stands: no turn is started on a doubt.
func (service *CurrentApplicationStartService) IsPeopleOnly(ctx context.Context, request CurrentApplicationStartRequest) bool {
	if !PeopleOnly(request.MentionedUserIDs, request.MentionsEveryone, request.AddressedTo) {
		return false
	}
	if service == nil || service.mentions == nil {
		return true
	}
	members, err := service.mentions.ProjectMemberUserIDs(ctx, request.ProjectID)
	if err != nil {
		return true
	}
	return len(mentionAudience(request, members)) > 0
}

// HumanMessage is a person's message stored without a turn.
type HumanMessage struct {
	ProjectID        int64
	AuthorUserID     int64
	ConversationUUID string
	// QuestionID is the message group's uuid and the idempotency key: a
	// replay of the same id stores nothing again.
	QuestionID     string
	QuestionItemID string
	UserInput      string
	// Meta is the message group's `meta` (interaction_uuid and the mentions,
	// as currentQuestionMeta writes them for a turn's question).
	Meta        []byte
	Attachments []CurrentTurnAttachment
}

// HumanMessageStore stores a message with no execution behind it
// (repos.HumanMessageRepo).
type HumanMessageStore interface {
	// PostHumanMessage stores the message as the author's, addressed to no
	// agent. created is false when the message was already stored (a replay
	// of the same QuestionID). ErrUnsupportedCurrentAgentStart answers a
	// conversation the author is not a participant of.
	PostHumanMessage(ctx context.Context, message HumanMessage) (created bool, err error)
}

// WithHumanMessages attaches the store people-only messages are written to.
func (service *CurrentApplicationStartService) WithHumanMessages(store HumanMessageStore) *CurrentApplicationStartService {
	if service != nil {
		service.humanMessages = store
	}
	return service
}

// PostedMessage is the outcome of PostPeopleMessage.
type PostedMessage struct {
	// MessageID is the stored message group's uuid (the request's question
	// id).
	MessageID string
	// Created is false when a replay found the message already stored.
	Created bool
}

// PostPeopleMessage stores a people-only message and notifies the people it
// names. It starts no execution and reads no model, agent or floor.
func (service *CurrentApplicationStartService) PostPeopleMessage(
	ctx context.Context,
	request CurrentApplicationStartRequest,
) (PostedMessage, error) {
	if request.ProjectID <= 0 || request.ActorUserID <= 0 || !request.TriggerOrigin.Valid() ||
		!validUUID(request.ConversationUUID) || !validUUID(request.QuestionID) ||
		!validCurrentAgentInput(request.UserInput, false) ||
		(request.InteractionUUID != "" && !validUUID(request.InteractionUUID)) ||
		!PeopleOnly(request.MentionedUserIDs, request.MentionsEveryone, request.AddressedTo) {
		return PostedMessage{}, ErrInvalidCurrentAgentStart
	}
	if service == nil || service.humanMessages == nil {
		return PostedMessage{}, ErrPeopleMessagesUnavailable
	}
	attachments, err := currentTurnAttachmentsWithImages(
		ctx, request.ProjectID, service.attachmentImages,
		request.QuestionID, request.ConversationUUID, request.Attachments,
	)
	if err != nil {
		return PostedMessage{}, err
	}
	created, err := service.humanMessages.PostHumanMessage(ctx, HumanMessage{
		ProjectID: request.ProjectID, AuthorUserID: request.ActorUserID,
		ConversationUUID: request.ConversationUUID,
		QuestionID:       request.QuestionID,
		QuestionItemID:   currentTurnUUID(request.QuestionID, "question-item"),
		UserInput:        request.UserInput,
		Meta:             currentQuestionMeta(request.InteractionUUID, request.MentionedUserIDs, request.MentionsEveryone),
		Attachments:      attachments,
	})
	if err != nil {
		return PostedMessage{}, err
	}
	// As for a turn's question: only a new message notifies, so a client
	// retrying a response it lost does not ring the same bell again.
	if created {
		service.notifyMentionedUsers(ctx, request)
	}
	return PostedMessage{MessageID: request.QuestionID, Created: created}, nil
}
