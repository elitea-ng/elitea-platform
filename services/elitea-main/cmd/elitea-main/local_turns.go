package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	v2convs "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	localturnsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/localturns"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// composeLocalTurns builds startLocalTurn / commitLocalTurn (ADR-0029
// decision 5c). It needs a database and nothing from the runtime: no worker
// runs a local turn. The policy reader is the router's own
// nativepolicy.Service, so an admin save that turns local work off reaches the
// next start on this replica at once. The recall resolver is a MemoriesRepo,
// the same implementation cloud admission uses.
//
// The answer is a nil interface, never a typed nil, when there is no pool.
func composeLocalTurns(
	pool *pgxpool.Pool,
	policy localturn.PolicyReader,
	recorder audit.Recorder,
	groupAuth apimw.AuthConfig,
	runBriefs *runbrief.Service,
	objectStore storage.ObjectStore,
	logger *slog.Logger,
) (http.Handler, error) {
	// No database, or no credential plane at all (the zero group AuthConfig):
	// nothing to serve, and nothing to authenticate a token with.
	if pool == nil || groupAuth.PrincipalValidator == nil {
		return nil, nil
	}
	repo := dbrepos.NewLocalTurnsRepo(pool)
	service, err := localturn.NewService(
		repo,
		policy,
		dbrepos.NewMemoriesRepo(pool),
		recorder,
		newLocalTurnExecutionID,
		logger,
	)
	if err != nil {
		return nil, err
	}
	// Unattended grants (ADR-0045): the approvals section, read live.
	service.WithUnattended(platformconfig.UnattendedSettingsSource{Pool: pool})
	route, err := localturnsapi.NewRoute(service, groupAuth, legacyrbac.NewPostgresResolver(pool))
	if err != nil {
		return nil, err
	}
	if runBriefs != nil {
		// The desktop run brief and goal check (ADR-0045 decision 11).
		route.WithRunBriefs(runBriefs)
	}
	// The computer use setting and screenshot upload (ADR-0047 decision 9).
	// Without an object store a screenshot has nowhere to go: the start
	// still answers the setting, and the upload answers 503.
	route.WithComputerUse(localTurnComputerUse{
		LocalTurnsRepo: repo, objects: objectStore, attachments: api.NewConversationAttachmentStore(pool),
	})
	return route, nil
}

// localTurnComputerUse stores a desktop turn's screenshots as attachments of
// its conversation, through the chat attachment path.
type localTurnComputerUse struct {
	*dbrepos.LocalTurnsRepo
	objects     storage.ObjectStore
	attachments v2convs.AttachmentStore
}

func (c localTurnComputerUse) StoreScreenshot(
	ctx context.Context, projectID int64, conversationUUID, name, mediaType string, content []byte,
) (string, error) {
	return v2convs.StoreConversationAttachment(ctx, c.objects, c.attachments, projectID, conversationUUID, name, mediaType, content)
}

// newLocalTurnExecutionID mints the runtime's execution id shape: 16 random
// bytes as 32 lowercase hex characters.
func newLocalTurnExecutionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
