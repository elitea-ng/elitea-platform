# Each agent answer records what it saw; re-adding an agent with "share from now" moves its join time (#1669)

- **What changed.**
  - Main records, for each agent answer, the view of the conversation it sent the agent: mode, window, digest,
    and the id of every visible message with the reason any of them was left out. Message bodies are never stored.
    `GET /api/v2/elitea_core/message_context_view/prompt_lib/{project_id}/{message_id}` serves it, behind the
    `models.chat.messages.details` permission and the conversation's own access check.
  - Adding an agent again with `entity_settings.context_history: since_joined` used to leave the old participant row
    as it was. Now it sets the mode and moves the join time to the new add.
- **What you need to do.** Nothing beyond the usual migrate. Tenant migration `0166_chat_message_context_views` adds
  the `chat_message_context_views` table to every tenant schema. Removing a message removes its view with it.
- **What to expect.** Answers from before the upgrade have no view, and the inspector says so.
