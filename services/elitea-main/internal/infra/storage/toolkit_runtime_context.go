package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"strings"
)

type CurrentToolkitMCPTokenLoader interface {
	Validate(context.Context, string, mcpoauth.TokenBinding) (mcpoauth.TokenReference, error)
	Load(context.Context, string, mcpoauth.TokenBinding) (mcpoauth.AccessToken, error)
}
type CurrentMaterializerOption func(*CurrentConfigurationsMaterializer)

func WithCurrentToolkitMCPAuthorization(tokens *mcpoauth.Tokens) CurrentMaterializerOption {
	return func(m *CurrentConfigurationsMaterializer) {
		if tokens != nil {
			m.toolkitTokens = tokens
		}
	}
}

// The content listener checks the live claim before it calls this method.
// Only the resulting response contains credential material.
func (m *CurrentConfigurationsMaterializer) materializeToolkitRuntimeContext(ctx context.Context, authorization ContentAuthorization, source []byte, maxBytes int64) ([]byte, error) {
	object, err := decodeCurrentMaterializationObject(source)
	if err != nil {
		return nil, ErrContentRejected
	}
	if _, exists := object["mcp_tokens"]; exists {
		return nil, ErrContentRejected
	}
	referenceValue, exists := object["mcp_token_reference"]
	if !exists {
		return source, nil
	}
	if m.toolkitTokens == nil {
		return nil, ErrContentRejected
	}
	encoded, err := json.Marshal(referenceValue)
	if err != nil {
		return nil, ErrContentRejected
	}
	var reference toolkitcalltool.MCPTokenReference
	if json.Unmarshal(encoded, &reference) != nil || reference.ToolkitID <= 0 || reference.Revision <= 0 {
		return nil, ErrContentRejected
	}
	projectID, ok := positiveCurrentMaterializationID(authorization.ResourceProjectID)
	if !ok {
		return nil, ErrContentRejected
	}
	actorID, ok := positiveCurrentMaterializationID(authorization.ActorID)
	if !ok {
		return nil, ErrContentRejected
	}
	binding := mcpoauth.TokenBinding{ProjectID: projectID, ActorID: actorID, ToolkitID: reference.ToolkitID, Resource: reference.Resource}
	resolved, err := m.toolkitTokens.Validate(ctx, reference.Reference, binding)
	if err != nil {
		return nil, toolkitTokenMaterializationError(ctx, err)
	}
	if resolved.Reference != reference.Reference || resolved.Revision != reference.Revision {
		return nil, ErrContentRejected
	}
	token, err := m.toolkitTokens.Load(ctx, reference.Reference, binding)
	if err != nil {
		return nil, toolkitTokenMaterializationError(ctx, err)
	}
	delete(object, "mcp_token_reference")
	if !strings.EqualFold(token.TokenType, "Bearer") {
		return nil, ErrContentRejected
	}
	projected := map[string]string{"access_token": token.AccessToken}
	if token.SessionID != "" {
		projected["session_id"] = token.SessionID
	}
	object["mcp_tokens"] = map[string]any{reference.Resource: projected}
	return encodeCurrentMaterializationObject(object, maxBytes)
}

func toolkitTokenMaterializationError(ctx context.Context, err error) error {
	if errors.Is(err, mcpoauth.ErrTokenUnavailable) {
		return ErrContentRejected
	}
	return currentMaterializationError(ctx, err)
}
