# A message to people starts no AI turn (#1970)

A chat message that mentions people (`@user`, `@everyone`) and addresses no
agent used to be answered by the conversation's model or by the agent last
addressed, which spoke on behalf of the person who was asked. It is now a
message to those people only: it is stored in the conversation, the people it
names are notified, and **no agent or model turn starts**.

- **Who is affected:** everyone who mentions a person in a chat; API and SDK
  clients that send `user_ids` or `is_mentioning_everyone` with a
  `participant_id` and no `addressed_to`; client builders (client contract 1.51).
- **What to do:** nothing in the web, desktop and mobile apps. A client that
  means an agent for a message with mentions sends it in `addressed_to`
  (`@Agent` / `#Agent`, or the agent picked as the target in the web composer).
  `participant_id` alone no longer addresses an agent when a message mentions
  people, because clients fill it in for every unaddressed message.
- **Behaviour:** client contract 1.51 (additive). `sendChatMessage` answers
  `201` with `ChatMessagePosted` (`message_id`, `created`, `turn_started: false`)
  and no `events_url` for such a message. The decision is made in Main before
  `participant_id`, `llm_settings` and the execution contract are read.
  `question_id` is still the idempotency key (a replay answers `created: false`
  and notifies nobody again). A message that addresses an agent still goes to
  it, with its people notified; a message with no mention is unchanged.
  `@everyone` counts as mentioning people. In a conversation with a single
  agent, mobile offers no agent mention, so a question to the agent is its own
  message.
- **No migration.** The message is a normal sender message group with no reply
  group, no execution and no addressee.
