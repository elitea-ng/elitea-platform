package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// The notification types of the Marketplace. The message in the meta is what
// the notification bell shows.
const (
	NotifyVersionYanked     = "marketplace_version_yanked"
	NotifyUpdateAvailable   = "marketplace_update_available"
	NotifyUpgradeNeedsHuman = "marketplace_upgrade_needs_approval"
	NotifyUpgradeApplied    = "marketplace_upgrade_applied"
)

// Notify writes one notification to a person. key names the event and the
// person, so a retried pass or a second click never repeats it: the uuid is
// derived from the key and a conflict is ignored. projectID is the project the
// notification belongs to.
func Notify(ctx context.Context, db DB, key string, projectID, userID int64, eventType string, meta map[string]any) error {
	if userID <= 0 || projectID <= 0 {
		return nil
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marketplace: encode the notification: %w", err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES ($1::uuid, FALSE, $2, $3, $4::jsonb, $5, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
ON CONFLICT (uuid) DO NOTHING`, notificationUUID(key), int32(projectID), int32(userID), encoded, eventType); err != nil {
		return fmt.Errorf("marketplace: write the notification: %w", err)
	}
	return nil
}

// notificationUUID derives a UUIDv4-shaped id from key.
func notificationUUID(key string) string {
	digest := sha256.Sum256([]byte(key))
	value := append([]byte(nil), digest[:16]...)
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// OwnersOf lists the people to tell about an agent: the person who hired it and
// the business and technical owners of its registry row.
func OwnersOf(ctx context.Context, db DB, projectID, applicationID, installedBy int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(id *int64) {
		if id != nil && *id > 0 && !seen[*id] {
			seen[*id] = true
			out = append(out, *id)
		}
	}
	hirer := installedBy
	add(&hirer)
	var business, technical *int64
	if err := db.QueryRow(ctx, `
SELECT business_owner_id, technical_owner_id FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, applicationID).Scan(&business, &technical); err == nil {
		add(business)
		add(technical)
	}
	return out
}
