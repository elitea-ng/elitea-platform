package applications

import "context"

type Repository interface {
	List(ctx context.Context, req ListRequest) (ListResponse, error)
	Get(ctx context.Context, projectID, applicationID string) (Application, error)
	Create(ctx context.Context, req CreateRequest) (Application, error)
	Update(ctx context.Context, req UpdateRequest) (Application, error)
	Delete(ctx context.Context, projectID, applicationID string) error
	GetVersion(ctx context.Context, projectID, applicationID, versionID string) (Version, error)
	ListVersions(ctx context.Context, projectID, applicationID string) ([]Version, error)
	CreateVersion(ctx context.Context, projectID, applicationID string, v Version) (Version, error)
	UpdateVersion(ctx context.Context, projectID, applicationID, versionID string, v Version) (Version, error)
	// UpdateWithVersion writes the application fields and one version in a single
	// transaction; any error rolls back both.
	UpdateWithVersion(ctx context.Context, req UpdateRequest, versionID string, v Version) (Application, Version, error)
	DeleteVersion(ctx context.Context, projectID, applicationID, versionID string) error
	SetDefaultVersion(ctx context.Context, projectID, applicationID, versionID string) error
	BatchReplaceVersion(ctx context.Context, projectID, oldVersionID, newVersionID string, deleteOld bool) error
}
