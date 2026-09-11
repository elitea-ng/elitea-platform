package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	conversationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	foldersapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type internalChatConversationHandler interface {
	List(http.ResponseWriter, *http.Request)
	Create(http.ResponseWriter, *http.Request)
	Get(http.ResponseWriter, *http.Request)
	Update(http.ResponseWriter, *http.Request)
	GetParticipant(http.ResponseWriter, *http.Request)
	RemoveParticipant(http.ResponseWriter, *http.Request)
	AddParticipant(http.ResponseWriter, *http.Request)
	UpdateEntitySettings(http.ResponseWriter, *http.Request)
}
type internalChatFolderHandler interface {
	List(http.ResponseWriter, *http.Request)
	Create(http.ResponseWriter, *http.Request)
	Update(http.ResponseWriter, *http.Request)
}
type internalChatExecutor interface {
	Execute(context.Context, int64, int64, internalChatOperation, map[string]any) (internalApplicationExecution, error)
}
type handlerInternalChatExecutor struct {
	conversations internalChatConversationHandler
	folders       internalChatFolderHandler
}

func WithInternalChatTools(conversations *conversationsapi.Handler, folders *foldersapi.Handler) Option {
	return func(h *Handler) {
		if conversations != nil && folders != nil {
			h.internalChat = &handlerInternalChatExecutor{conversations, folders}
		}
	}
}
func (h *Handler) callInternalChatTool(r *http.Request, projectID int64, target Tool, arguments map[string]any) map[string]any {
	if h.internalChat == nil {
		return errorResult("this deployment cannot execute internal chat tools; nothing was executed")
	}
	return h.callInternalTool(r, projectID, target, arguments, "chat", func(actorID int64) (internalApplicationExecution, error) {
		return h.internalChat.Execute(r.Context(), projectID, actorID, target.internalChatOperation, arguments)
	})
}
func (e *handlerInternalChatExecutor) Execute(ctx context.Context, projectID, actorID int64, op internalChatOperation, args map[string]any) (internalApplicationExecution, error) {
	if e == nil || e.conversations == nil || e.folders == nil || ctx == nil || projectID <= 0 || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("invalid internal chat context")
	}
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}
	var schema map[string]any
	for _, tool := range internalChatTools() {
		if tool.internalChatOperation == op {
			schema = tool.InputSchema
			break
		}
	}
	if schema == nil {
		return internalApplicationExecution{}, errors.New("unknown internal chat operation")
	}
	// Validate the explicit schema on execution too; callers need not honor tools/list.
	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return internalApplicationExecution{}, err
	}
	var schemaDocument any
	if err = json.Unmarshal(schemaBytes, &schemaDocument); err != nil {
		return internalApplicationExecution{}, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:elitea:chat", schemaDocument); err != nil {
		return internalApplicationExecution{}, err
	}
	compiled, err := compiler.Compile("urn:elitea:chat")
	if err != nil {
		return internalApplicationExecution{}, err
	}
	normalized := map[string]any{}
	encoded, err := json.Marshal(args)
	if err != nil {
		return internalNotificationBadRequest("invalid chat arguments")
	}
	if err = json.Unmarshal(encoded, &normalized); err != nil {
		return internalNotificationBadRequest("invalid chat arguments")
	}
	normalized["project_id"] = float64(projectID)
	if err = compiled.Validate(normalized); err != nil {
		return internalNotificationBadRequest("chat arguments do not match the published schema")
	}
	actor := strconv.FormatInt(actorID, 10)
	principal, ok := auth.UserFromContext(ctx)
	if !ok {
		principal = auth.User{ID: actor}
	}
	principal.UserID = actor
	ctx = auth.ContextWithUser(ctx, principal)
	params := map[string]string{"projectID": strconv.FormatInt(projectID, 10)}
	body := map[string]any{}
	for key, value := range args {
		switch key {
		case "project_id":
		case "conversation_id":
			params["conversationID"] = scalarArgument(value)
		case "participant_id":
			params["participantID"] = scalarArgument(value)
		case "folder_id":
			if op == internalChatFolderUpdate {
				params["folderID"] = scalarArgument(value)
			} else {
				body[key] = value
			}
		default:
			body[key] = value
		}
	}
	method := http.MethodGet
	query := url.Values{}
	var handler http.HandlerFunc
	switch op {
	case internalChatList:
		handler = e.conversations.List
	case internalChatCreate:
		method = http.MethodPost
		handler = e.conversations.Create
	case internalChatGet:
		handler = e.conversations.Get
		query.Set("messages_limit", "100")
	case internalChatUpdate:
		method = http.MethodPut
		handler = e.conversations.Update
	case internalChatParticipantGet:
		handler = e.conversations.GetParticipant
	case internalChatParticipantDelete:
		method = http.MethodDelete
		handler = e.conversations.RemoveParticipant
	case internalChatParticipantConfigure:
		method = http.MethodPut
		handler = e.conversations.UpdateEntitySettings
	case internalChatParticipantsAdd:
		method = http.MethodPost
		participants, err := json.Marshal(args["participants"])
		if err != nil {
			return internalNotificationBadRequest("invalid participants")
		}
		handler = func(w http.ResponseWriter, r *http.Request) {
			r.Body = io.NopCloser(bytes.NewReader(participants))
			e.conversations.AddParticipant(w, r)
		}
		body = nil
	case internalChatFoldersList:
		handler = e.folders.List
	case internalChatFolderCreate:
		method = http.MethodPost
		handler = e.folders.Create
	case internalChatFolderUpdate:
		method = http.MethodPut
		handler = e.folders.Update
	}
	if method == http.MethodGet {
		for key, value := range body {
			query.Set(key, scalarArgument(value))
		}
		body = nil
	}
	return invokeInternalHandler(ctx, method, query, body, params, handler)
}
