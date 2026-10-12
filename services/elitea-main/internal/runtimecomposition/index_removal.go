package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file composes the `remove_index` tool's Main half (ADR-0030 I1,
// ADR-0031): the worker asks over RuntimeControlService.RemoveIndex, and the
// claim remover runs the registry's delete flow as the claimed run's principal.
// It exists only under the Rust indexing runtime; the control server's
// IndexRemovals stays nil otherwise and the RPC answers UNSUPPORTED_CAPABILITY.

// newClaimIndexRemover builds the remover over THE registry Service the index
// routes and the runtime's index path run (indexRegistryComposition.service,
// review round 2), the content repository's claim authorizer (the one the
// worker's PAT redemption goes through) and the deployment's permission
// resolver.
func newClaimIndexRemover(
	contentPool *pgxpool.Pool,
	registry *indexRegistryComposition,
	permissions auth.PermissionResolver,
	logger *slog.Logger,
) (*indexregistryapp.ClaimRemover, error) {
	if contentPool == nil || registry == nil || registry.service == nil || permissions == nil || logger == nil {
		return nil, errors.New("index removal dependencies are required")
	}
	service := registry.service
	content, err := storage.NewPostgresContentRepository(contentPool)
	if err != nil {
		return nil, fmt.Errorf("construct index removal claim authorizer: %w", err)
	}
	return indexregistryapp.NewClaimRemover(
		claimAuthorizerAdapter{repository: content},
		permissionCheckerAdapter{resolver: permissions},
		service,
		func(err error) { logger.Error("index removal cleanup failed", "err", err) },
	)
}

// newIndexRegistryService builds the one registry Service of a rust-mode
// deployment, which the composition root stores in
// indexRegistryComposition.service for the index routes, the index runtime
// and the claim remover alike.
func newIndexRegistryService(pool *pgxpool.Pool, registry *indexRegistryComposition) (*indexregistryapp.Service, error) {
	if pool == nil || registry == nil || registry.repo == nil {
		return nil, errors.New("index registry service dependencies are required")
	}
	builtInSchemas, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		return nil, fmt.Errorf("load current SDK toolkit schema snapshot: %w", err)
	}
	names, err := NewCurrentBuiltInToolkitNameDeriver(builtInSchemas)
	if err != nil {
		return nil, err
	}
	toolkitRows, err := repos.NewCurrentToolkitsRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("construct current toolkit repository: %w", err)
	}
	toolkits, err := NewCurrentToolkitReaderAdapter(toolkitRows, names)
	if err != nil {
		return nil, err
	}
	schedules, err := repos.NewCurrentIndexMetaScheduleRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("construct current index metadata schedule repository: %w", err)
	}
	service, err := indexregistryapp.NewService(toolkits, registry.repo, schedules, registry.vectors, registry.report)
	if err != nil {
		return nil, fmt.Errorf("construct index registry service: %w", err)
	}
	return service, nil
}

// claimAuthorizerAdapter resolves a worker claim through the content
// repository's runtime-context authorizer, with the workload identity the
// control service authenticated instead of a peer certificate.
type claimAuthorizerAdapter struct {
	repository *storage.PostgresContentRepository
}

func (a claimAuthorizerAdapter) AuthorizeClaim(
	ctx context.Context,
	claim indexregistryapp.ClaimRef,
) (indexregistryapp.ClaimPrincipal, error) {
	authorization, err := a.repository.AuthorizeRuntimeContextForWorkload(ctx, storage.ContentClaim{
		ExecutionID: claim.ExecutionID,
		Generation:  claim.Generation,
		ClaimID:     claim.ClaimID,
		FenceToken:  claim.FenceToken,
	}, claim.WorkloadIdentity)
	if err != nil {
		if errors.Is(err, storage.ErrContentUnauthorized) {
			return indexregistryapp.ClaimPrincipal{}, indexregistryapp.ErrClaimUnauthorized
		}
		return indexregistryapp.ClaimPrincipal{}, fmt.Errorf("%w: %w", indexregistryapp.ErrClaimRemovalUnavailable, err)
	}
	return indexregistryapp.ClaimPrincipal{
		ProjectID: authorization.ResourceProjectID,
		ActorID:   authorization.ActorID,
		Initiator: authorization.Initiator,
	}, nil
}

// permissionCheckerAdapter asks the permission resolver what the REST route's
// RBAC middleware asks: the user's resolved permissions in the project, in the
// default mode. A principal the resolver refuses (no membership, a suspended
// user) holds nothing.
type permissionCheckerAdapter struct {
	resolver auth.PermissionResolver
}

func (a permissionCheckerAdapter) HasProjectPermission(
	ctx context.Context,
	userID, projectID int64,
	permission string,
) (bool, error) {
	id := strconv.FormatInt(userID, 10)
	resolution, err := a.resolver.ResolvePermissions(
		ctx,
		auth.User{ID: id, UserID: id},
		auth.PermissionModeDefault,
		strconv.FormatInt(projectID, 10),
	)
	if err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			return false, nil
		}
		return false, err
	}
	return slices.Contains(resolution.Permissions, permission), nil
}
