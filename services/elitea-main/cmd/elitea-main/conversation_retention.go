package main

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	v2convs "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/conversationretention"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// startConversationRetention runs the conversation retention sweep (ADR-0042
// decision 4, issue #1495) on the routine registry: one replica at a time,
// hourly by default, switchable on the admin Schedules page. With the seeded
// policy (keep forever) every pass deletes nothing and reads no tenant.
//
// Without an object store the attachment bytes cannot exist, so the purger is
// conversations.DeleteStoredAttachments with a nil store, which does nothing.
func startConversationRetention(
	ctx context.Context,
	pool *pgxpool.Pool,
	objectStore storage.ObjectStore,
	logger *slog.Logger,
) error {
	if pool == nil {
		return nil
	}
	attachmentStore := api.NewConversationAttachmentStore(pool)
	purge := func(ctx context.Context, projectID int64, refs []conversationretention.AttachmentRef) error {
		converted := make([]v2convs.AttachmentRef, 0, len(refs))
		for _, ref := range refs {
			converted = append(converted, v2convs.AttachmentRef{Bucket: ref.Bucket, Name: ref.Name})
		}
		return v2convs.DeleteStoredAttachments(ctx, objectStore, attachmentStore,
			strconv.FormatInt(projectID, 10), converted)
	}
	// Fail closed: an unreadable maintenance switch stops the pass.
	gate := func(ctx context.Context) (bool, error) {
		state, err := platformconfig.LoadMaintenance(ctx, pool)
		if err != nil {
			return false, err
		}
		return state.Enabled, nil
	}
	sweeper, err := conversationretention.New(
		dbrepos.NewRetentionPolicyRepo(pool),
		dbrepos.NewConversationsRepo(pool),
		purge, gate, conversationretention.Config{}, logger,
	)
	if err != nil {
		return err
	}
	loop := &routines.Loop{
		Backend: routines.NewStore(pool),
		Def: routines.Definition{
			ID:              conversationretention.RoutineID,
			Owner:           routines.OwnerMain,
			Description:     "Deletes conversations older than the retention policy's maximum, except those under legal hold, and their stored attachments.",
			DefaultSchedule: "1h",
			MinInterval:     5 * time.Minute,
		},
		Logger: logger,
	}
	go loop.Run(ctx, sweeper.Pass)
	return nil
}
