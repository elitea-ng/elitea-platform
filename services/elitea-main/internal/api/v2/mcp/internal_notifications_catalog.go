package mcp

// The fixed notifications category mirrors only the three current operations
// marked mcp_tool=True. Bulk mutation and deletion remain ordinary REST API
// operations and are intentionally absent from the model-facing catalogue.
const internalNotificationsCategory = "notifications"

type internalNotificationOperation string

const (
	internalListNotifications internalNotificationOperation = "list_notifications"
	internalGetNotification   internalNotificationOperation = "get_notification"
	internalMarkNotification  internalNotificationOperation = "mark_notification_seen"
)

type internalNotificationToolDefinition struct {
	name        string
	description string
	permission  string
	operation   internalNotificationOperation
	schema      map[string]any
}

var internalNotificationToolDefinitions = []internalNotificationToolDefinition{
	{
		name: "get_notifications_notifications",
		description: "List the authenticated user's notifications with bounded pagination, filtering, and sorting. " +
			"Set only_total to return only the matching count.",
		permission: "models.notifications.notifications.list",
		operation:  internalListNotifications,
		schema: objectSchema(map[string]any{
			"project_id": intProperty("Current project ID. The server verifies this value."),
			"limit":      integerRangeProperty("Maximum notifications to return.", 1, 1000),
			"offset":     integerMinProperty("Pagination offset.", 0),
			"sort_by": enumProperty("Sort field.",
				"id", "uuid", "is_seen", "project_id", "user_id", "meta", "event_type", "created_at", "updated_at"),
			"sort_order": enumProperty("Sort direction.", "asc", "desc"),
			"only_new":   map[string]any{"type": "boolean", "description": "Return only unseen notifications."},
			"only_total": map[string]any{"type": "boolean", "description": "Return only the matching count."},
			"search": boundedStringProperty(
				"Case-insensitive message search. At most 32 whitespace-separated terms of 256 bytes each.", 0, 8223,
			),
			"event_type": boundedStringProperty("Exact event type filter.", 0, 255),
		}, "project_id"),
	},
	{
		name:        "get_notifications_notification",
		description: "Read one notification owned by the authenticated user.",
		permission:  "models.notifications.notification.details",
		operation:   internalGetNotification,
		schema: objectSchema(map[string]any{
			"project_id":      intProperty("Current project ID. The server verifies this value."),
			"notification_id": intProperty("Notification ID."),
		}, "project_id", "notification_id"),
	},
	{
		name:        "put_notifications_notification",
		description: "Idempotently mark one notification owned by the authenticated user as seen.",
		permission:  "models.notifications.notification.update",
		operation:   internalMarkNotification,
		schema: objectSchema(map[string]any{
			"project_id":      intProperty("Current project ID. The server verifies this value."),
			"notification_id": intProperty("Notification ID."),
		}, "project_id", "notification_id"),
	},
}

func internalNotificationTools() []Tool {
	tools := make([]Tool, 0, len(internalNotificationToolDefinitions))
	for _, definition := range internalNotificationToolDefinitions {
		tools = append(tools, Tool{
			Name:                          definition.name,
			Description:                   definition.description,
			InputSchema:                   definition.schema,
			internalNotificationOperation: definition.operation,
			permission:                    definition.permission,
		})
	}
	return tools
}
