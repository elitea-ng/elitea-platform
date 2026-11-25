package repos

// The notice to the owners of `inherit` tokens (ADR-0059 decision 2, "Token
// owners get 30 days from delivery", decision 7, "`inherit`"; story R3.15b,
// issue 2619): the second candidate query of the expiry notifier.
//
// The notifier lists the owners here and queues one notice each in the
// access notice outbox (elitea_auth.admin_access_notices, shared 0277); the
// access notice sender delivers it and stamps
// elitea_identity.token_lifecycle.inherit_notice_delivered_at (shared 0278)
// on each listed token. An owner stays a candidate until that stamp exists,
// so a failed send is listed again, and the outbox's dedupe key keeps the
// queued notice to one row.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/patexpiry"
)

var _ patexpiry.InheritStore = (*PATExpiryNotificationRepository)(nil)

// NewPATNotifier builds the expiry notifier with both halves over one
// repository: the expiry notice and the `inherit` token notice, whose sunset
// date is the registry's (adminaccess.InheritEndsOn). The scheduled sweep
// and the operator's "run now" route both use it, so they run the same pass.
func NewPATNotifier(pool *pgxpool.Pool) (*patexpiry.Notifier, error) {
	store, err := NewPATExpiryNotificationRepository(pool)
	if err != nil {
		return nil, err
	}
	notifier, err := patexpiry.New(store)
	if err != nil {
		return nil, err
	}
	if err := notifier.WithInheritNotices(store, adminaccess.InheritEndsOn); err != nil {
		return nil, err
	}
	return notifier, nil
}

// inheritTokenOwnerQuery selects the owners due an `inherit` notice, with
// their tokens:
//
//	admin_access = 'inherit'         only the backfill value; a `none` or
//	                                 `scoped` token is never a candidate.
//	uuid IS NOT NULL                 a personal access token, not a native
//	                                 device anchor (shared 0265).
//	inherit_notice_delivered_at NULL not delivered yet. LEFT JOIN: a token
//	                                 with no lifecycle row was never told.
//	expires NULL or in the future    an expired token needs no notice.
//	unsuspended                      a suspended owner's token resolves
//	                                 nothing.
//	no notice waiting                an owner whose notice is queued and not
//	                                 sent yet does not take a slot of the
//	                                 limit again (the mailer may be down).
//
// An owner with no address IS listed, with Email "": the notifier counts
// them as unreachable (Result.InheritUnreachable) instead of dropping them
// silently; no notice can reach them, so their tokens end at the sunset date.
const inheritTokenOwnerQuery = `
SELECT owner.id, COALESCE(btrim(owner.email), ''), COALESCE(owner.name, ''),
       array_agg(token.id::bigint ORDER BY token.id),
       array_agg(COALESCE(token.name, '')::text ORDER BY token.id)
FROM public.auth_core__token AS token
JOIN public.auth_core__user AS owner ON owner.id = token.user_id
LEFT JOIN elitea_identity.token_lifecycle AS lifecycle ON lifecycle.token_id = token.id
WHERE token.admin_access = 'inherit'
  AND token.uuid IS NOT NULL
  AND lifecycle.inherit_notice_delivered_at IS NULL
  AND (token.expires IS NULL OR token.expires > ($1::timestamptz AT TIME ZONE 'UTC'))
  AND owner.suspended = false
  AND NOT EXISTS (
      SELECT 1 FROM elitea_auth.admin_access_notices AS waiting
      WHERE waiting.kind = 'inherit_token' AND waiting.recipient_user_id = owner.id
        AND waiting.sent_at IS NULL)
GROUP BY owner.id, owner.email, owner.name
ORDER BY owner.id
LIMIT $2`

// ListInheritTokenOwners answers the owners of `inherit` tokens whose notice
// was not delivered, at most limit, each with those tokens.
func (r *PATExpiryNotificationRepository) ListInheritTokenOwners(
	ctx context.Context, now time.Time, limit int32,
) ([]patexpiry.InheritOwner, error) {
	rows, err := r.pool.Query(ctx, inheritTokenOwnerQuery, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list owners of inherit tokens: %w", err)
	}
	defer rows.Close()
	owners := make([]patexpiry.InheritOwner, 0)
	for rows.Next() {
		var owner patexpiry.InheritOwner
		var ids []int64
		var names []string
		if err := rows.Scan(&owner.UserID, &owner.Email, &owner.Name, &ids, &names); err != nil {
			return nil, fmt.Errorf("scan an owner of inherit tokens: %w", err)
		}
		if len(ids) != len(names) {
			return nil, fmt.Errorf("owner %d of inherit tokens: %d ids and %d names", owner.UserID, len(ids), len(names))
		}
		for i, id := range ids {
			owner.Tokens = append(owner.Tokens, patexpiry.InheritToken{ID: id, Name: names[i]})
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read owners of inherit tokens: %w", err)
	}
	return owners, nil
}

// QueueInheritTokenNotice queues the owner's notice in the access notice
// outbox, once per owner, date and token list.
func (r *PATExpiryNotificationRepository) QueueInheritTokenNotice(
	ctx context.Context, owner patexpiry.InheritOwner, sunsetAfter string, now time.Time,
) (bool, error) {
	candidate := adminaccess.InheritTokenOwner{UserID: owner.UserID, Email: owner.Email, Name: owner.Name}
	for _, token := range owner.Tokens {
		candidate.Tokens = append(candidate.Tokens, adminaccess.NoticeToken{ID: token.ID, Name: token.Name})
	}
	return adminaccess.QueueInheritTokenNotice(ctx, r.pool, candidate, sunsetAfter, now)
}
