package mcp

const internalChatCategory = "elitea_core/chat"

type internalChatOperation string

const (
	internalChatList                 internalChatOperation = "list"
	internalChatCreate               internalChatOperation = "create"
	internalChatGet                  internalChatOperation = "get"
	internalChatUpdate               internalChatOperation = "update"
	internalChatParticipantGet       internalChatOperation = "participant_get"
	internalChatParticipantDelete    internalChatOperation = "participant_delete"
	internalChatParticipantsAdd      internalChatOperation = "participants_add"
	internalChatParticipantConfigure internalChatOperation = "participant_configure"
	internalChatFoldersList          internalChatOperation = "folders_list"
	internalChatFolderCreate         internalChatOperation = "folder_create"
	internalChatFolderUpdate         internalChatOperation = "folder_update"
)

func internalChatTools() []Tool {
	id := func(description string) map[string]any { return integerRangeProperty(description, 1, 2147483647) }
	text := func(description string) map[string]any { return boundedStringProperty(description, 0, 32768) }
	object := map[string]any{"type": "object", "additionalProperties": true}
	participant := objectSchema(map[string]any{"entity_name": enumProperty("Participant type.", "user", "dummy", "llm", "application", "toolkit"), "entity_meta": object, "entity_settings": object}, "entity_name", "entity_meta")
	participants := map[string]any{"type": "array", "maxItems": 100, "items": participant}
	tools := []Tool{}
	add := func(name, permission, description string, op internalChatOperation, fields map[string]any, required ...string) {
		fields["project_id"] = id("Current project ID, checked against the endpoint.")
		tools = append(tools, Tool{Name: name, Description: description, permission: permission, internalChatOperation: op, InputSchema: objectSchema(fields, append([]string{"project_id"}, required...)...)})
	}
	add("get_elitea_core_conversations", "models.chat.conversations.list", "List actor-visible conversations with bounded pagination.", internalChatList, map[string]any{"limit": integerRangeProperty("Maximum rows.", 1, 100), "offset": integerRangeProperty("Offset.", 0, 100000), "source": text("Exact source filter."), "entity_name": text("Participant type filter."), "entity_meta_id": id("Participant entity ID."), "mine": map[string]any{"type": "boolean"}})
	add("post_elitea_core_conversations", "models.chat.conversations.create", "Create a conversation atomically with authenticated author, required user/dummy participants, and the actor's persona defaults.", internalChatCreate, map[string]any{"name": boundedStringProperty("Conversation name.", 3, 255), "is_private": map[string]any{"type": "boolean"}, "source": boundedStringProperty("Source, defaults to elitea.", 0, 64), "instructions": text("System instructions."), "meta": object, "participants": participants}, "name")
	add("get_elitea_core_conversation", "models.chat.conversation.details", "Read an actor-visible conversation and optionally its latest message groups.", internalChatGet, map[string]any{"conversation_id": id("Conversation ID."), "messages_limit": integerRangeProperty("Maximum message groups, zero omits groups.", 0, 100), "sort_order": enumProperty("Message order.", "asc", "acs", "desc")}, "conversation_id")
	add("put_elitea_core_conversation", "models.chat.conversation.update", "Update an actor-visible conversation. Public conversations cannot become private; public projects cannot contain public conversations.", internalChatUpdate, map[string]any{"conversation_id": id("Conversation ID."), "name": boundedStringProperty("Conversation name.", 3, 255), "instructions": text("System instructions; empty clears them."), "is_private": map[string]any{"type": "boolean"}, "folder_id": map[string]any{"type": []string{"integer", "null"}, "minimum": 1}, "meta": object}, "conversation_id")
	for _, entry := range []struct {
		name, permission, description string
		op                            internalChatOperation
	}{
		{"get_elitea_core_participant", "models.chat.participant.get", "Read a participant mapped to an actor-visible conversation.", internalChatParticipantGet},
		{"delete_elitea_core_participant", "models.chat.participant.delete", "Remove a participant mapping. The conversation author cannot be removed.", internalChatParticipantDelete},
	} {
		add(entry.name, entry.permission, entry.description, entry.op, map[string]any{"conversation_id": id("Conversation ID."), "participant_id": id("Mapped participant ID.")}, "conversation_id", "participant_id")
	}
	add("post_elitea_core_participants", "models.chat.participants.create", "Add bounded participants to an actor-visible conversation.", internalChatParticipantsAdd, map[string]any{"conversation_id": id("Conversation ID."), "participants": participants}, "conversation_id", "participants")
	add("patch_elitea_core_entity_settings", "models.chat.entity_settings.update", "Configure a mapped participant using the shared conversation settings validation.", internalChatParticipantConfigure, map[string]any{"conversation_id": id("Conversation ID."), "participant_id": id("Mapped participant ID."), "version_id": id("Application version ID."), "id": id("Legacy version ID alias."), "variables": map[string]any{"type": "array", "maxItems": 1000, "items": object}, "chat_history_template": map[string]any{"anyOf": []any{enumProperty("History mode.", "all", "interaction", "context_managed"), integerRangeProperty("History length.", 0, 100000)}}, "icon_meta": object, "llm_settings": object}, "conversation_id", "participant_id")
	add("get_elitea_core_folder", "models.chat.folders.get", "List actor-visible folders and conversations, optionally grouped for the sidebar.", internalChatFoldersList, map[string]any{"query": boundedStringProperty("Name search.", 0, 1024), "limit": integerRangeProperty("Maximum rows.", 1, 100), "offset": integerRangeProperty("Offset.", 0, 100000), "grouped": map[string]any{"type": "boolean"}, "folder_id": id("Folder ID."), "date_group": enumProperty("Date group.", "today", "this_week", "older"), "source": boundedStringProperty("Sources, comma separated.", 0, 256), "sort_by": enumProperty("Sort field.", "name", "created_at", "updated_at"), "sort_order": enumProperty("Sort direction.", "asc", "desc")})
	add("post_elitea_core_folder", "models.chat.folders.create", "Create a folder owned by the authenticated actor.", internalChatFolderCreate, map[string]any{"name": boundedStringProperty("Folder name.", 1, 255)}, "name")
	add("put_elitea_core_folder", "models.chat.folders.update", "Rename or reorder the authenticated actor's folder.", internalChatFolderUpdate, map[string]any{"folder_id": id("Folder ID."), "name": boundedStringProperty("Folder name.", 1, 255), "position": map[string]any{"type": "integer", "minimum": -2147483648, "maximum": 2147483647}}, "folder_id", "name")
	return tools
}
