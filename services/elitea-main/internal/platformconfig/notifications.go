package platformconfig

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionNotifications holds the platform's notification settings (ADR-0045
// decision 6, #1437).
const SectionNotifications = "notifications"

// KeyNotificationsWebPush turns web push on or off for the install. Default
// on. Off: Main sends no push, and push_config answers enabled false, so the
// browsers show no opt-in. The bell and the desktop notices do not change.
const KeyNotificationsWebPush = "web_push"

// NotificationSettings is SectionNotifications.
type NotificationSettings struct {
	WebPush bool
}

// DefaultNotificationSettings is the section that nobody saved.
func DefaultNotificationSettings() NotificationSettings {
	return NotificationSettings{WebPush: true}
}

// NotificationSettingsFromValues is the overlay of SectionNotifications. A
// value of the wrong type keeps the default.
func NotificationSettingsFromValues(values Values) NotificationSettings {
	return NotificationSettings{WebPush: values.Bool(KeyNotificationsWebPush, true)}
}

// LoadNotificationSettings reads SectionNotifications. A database without
// the table never saved it and answers the defaults.
func LoadNotificationSettings(ctx context.Context, pool *pgxpool.Pool) (NotificationSettings, error) {
	values, err := Load(ctx, pool, SectionNotifications)
	if UndefinedTable(err) {
		return DefaultNotificationSettings(), nil
	}
	if err != nil {
		return DefaultNotificationSettings(), err
	}
	return NotificationSettingsFromValues(values), nil
}
