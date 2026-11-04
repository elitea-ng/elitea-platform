package admin

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"

// notificationsSection is the platform's notification settings (ADR-0045
// decision 6, #1437). It is LIVE: the web push dispatcher and the
// push_config route read web_push on each pass and each call.
func notificationsSection() map[string]any {
	const section = platformconfig.SectionNotifications
	defaults := platformconfig.DefaultNotificationSettings()
	return map[string]any{
		"id":    section,
		"title": "Notifications",
		"description": "How the platform tells people that a run ended or waits for them. The bell and the " +
			"desktop app always show these notices. Web push sends them to the browsers that people opted in.",
		"order":               9,
		"icon":                "notifications",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeyNotificationsWebPush,
				"type":  "boolean",
				"title": "Web Push",
				"description": "Send a browser notification when a run that a person may have left ends, or waits " +
					"for an answer. The notification holds no content: it shows a fixed text and opens the " +
					"conversation. Main sends it to the browser's push service (Google, Mozilla, Apple or " +
					"Microsoft), through the egress proxy when one is set. Turn this off to send no push. The " +
					"bell and the desktop notices do not change.",
				"section": section,
				"default": defaults.WebPush,
			},
		},
	}
}
