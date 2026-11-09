package api

import (
	"net/http"

	v2convs "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

// teamConversations is the chat half of the saved team routes (ADR-0050
// decision 8, #1620): the conversations repository's read rule and the
// conversations handler's ordinary create, so a team's conversation is made
// exactly like a hand-made one.
type teamConversations struct {
	v2convs.Repository
	handler *v2convs.Handler
}

func (c teamConversations) CreateConversation(r *http.Request, projectID string, conv v2convs.Conversation) (v2convs.Conversation, error) {
	return c.handler.CreateConversation(r, projectID, conv)
}
