package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialuse"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
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
	projected, err := m.resolveMCPTokenReference(ctx, binding, reference.Reference, reference.Revision)
	if err != nil {
		return nil, toolkitTokenMaterializationError(ctx, err)
	}
	delete(object, "mcp_token_reference")
	object["mcp_tokens"] = map[string]any{reference.Resource: projected}
	return encodeCurrentMaterializationObject(object, maxBytes)
}

// resolveMCPTokenReference is the one release path of a delegated MCP grant
// (FU-250): a toolkit call's frozen reference and a fan-out wake's
// mcp_token_references both come here, only from the claim-checked content
// listener. The binding's project and actor are the live claim's, so a
// reference resolves only for the run's own actor; Validate re-checks the
// full binding (project, actor, toolkit, resource) and the exact revision
// before Load decrypts. Only the returned map holds token material.
func (m *CurrentConfigurationsMaterializer) resolveMCPTokenReference(
	ctx context.Context, binding mcpoauth.TokenBinding, reference string, revision int64,
) (map[string]string, error) {
	resolved, err := m.toolkitTokens.Validate(ctx, reference, binding)
	if err != nil {
		return nil, err
	}
	if resolved.Reference != reference || resolved.Revision != revision {
		return nil, mcpoauth.ErrTokenUnavailable
	}
	token, err := m.toolkitTokens.Load(ctx, reference, binding)
	if err != nil {
		return nil, err
	}
	// The credential inventory's last use (ADR-0044 decision 5).
	credentialuse.Default().Touch(credentialuse.KindMCPOAuthToken, reference, int64(binding.ProjectID))
	if !strings.EqualFold(token.TokenType, "Bearer") {
		return nil, mcpoauth.ErrTokenUnavailable
	}
	projected := map[string]string{"access_token": token.AccessToken}
	if token.SessionID != "" {
		projected["session_id"] = token.SessionID
	}
	return projected, nil
}

func toolkitTokenMaterializationError(ctx context.Context, err error) error {
	if errors.Is(err, mcpoauth.ErrTokenUnavailable) {
		return ErrContentRejected
	}
	return currentMaterializationError(ctx, err)
}
