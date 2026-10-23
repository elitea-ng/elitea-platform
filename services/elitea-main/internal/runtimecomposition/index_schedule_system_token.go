package runtimecomposition

import (
	"context"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type ProjectSystemTokenSource interface {
	IssueProjectToken(context.Context, int64) (authsvc.ProjectSystemToken, error)
}

type currentProjectSystemTokenAdapter struct {
	source ProjectSystemTokenSource
}

func (adapter currentProjectSystemTokenAdapter) IssueProjectToken(
	ctx context.Context,
	projectID int64,
) (storage.ProjectSystemToken, error) {
	token, err := adapter.source.IssueProjectToken(ctx, projectID)
	if err != nil {
		return storage.ProjectSystemToken{}, err
	}
	return storage.NewProjectSystemToken(
		token.ProjectID(),
		token.UserID(),
		token.Token(),
	), nil
}

var _ storage.ProjectSystemTokenIssuer = currentProjectSystemTokenAdapter{}
