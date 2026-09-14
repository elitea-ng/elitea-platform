package toolkitcalltool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ResultRequestReader looks up an existing admission. It never submits work.
type ResultRequestReader interface {
	FindToolkitCallToolExecution(context.Context, string, string) (string, error)
}

func validRequestKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, ch := range key {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

// The admission digest binds the immutable inputs separately. Reusing this key
// with changed inputs must conflict, rather than admit a second execution.
func recoverableRequestKey(projectID, actorID, toolkitID int64, key string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/%s", projectID, actorID, toolkitID, key)))
	return "toolkit-call-tool-request-v1:" + hex.EncodeToString(digest[:])
}

func (s *RunService) resolveResultRequest(ctx context.Context, request ResultRequest) (ResultRequest, error) {
	if request.RequestKey == "" {
		return request, nil
	}
	reader, ok := s.settlements.(ResultRequestReader)
	if !ok {
		return ResultRequest{}, fmt.Errorf("tool run request lookup unavailable")
	}
	scope := fmt.Sprintf("%d/%d/%d", request.ProjectID, request.ProjectID, request.ActorUserID)
	key := recoverableRequestKey(request.ProjectID, request.ActorUserID, request.ToolkitID, request.RequestKey)
	executionID, err := reader.FindToolkitCallToolExecution(ctx, scope, key)
	if err != nil {
		return ResultRequest{}, err
	}
	request.ExecutionID, request.RequestKey = executionID, ""
	return request, request.Validate()
}
