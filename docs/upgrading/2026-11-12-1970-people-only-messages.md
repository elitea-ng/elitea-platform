# A message to people starts no AI turn (#1970)

A chat message that mentions people (`@user`, `@everyone`) and addresses no
agent used to be answered by the conversation's model or by the agent last
addressed, which spoke on behalf of the person who was asked. It is now a
message to those people only: it is stored in the conversation, the people it
names are notified, and **no agent or model turn starts**.

- **Who is affected:** everyone who mentions a person in a chat; API and SDK
  clients that send `user_ids` or `is_mentioning_everyone` with a
  `participant_id` and no `addressed_to`; client builders (client contract 1.53).
- **What to do:** nothing in the web, desktop and mobile apps. A client that
  means an agent for a message with mentions sends it in `addressed_to`
  (`@Agent` / `#Agent`, or the agent picked as the target in the web composer).
  `participant_id` alone no longer addresses an agent when a message mentions
  people, because clients fill it in for every unaddressed message.
- **Behaviour:** client contract 1.53 (additive). `sendChatMessage` answers
  `201` with `ChatMessagePosted` (`message_id`, `created`, `turn_started: false`)
  and no `events_url` for such a message. The decision is made in Main before
  `participant_id`, `llm_settings` and the execution contract are read.
  `question_id` is still the idempotency key (a replay answers `created: false`
  and notifies nobody again). A message that addresses an agent still goes to
  it, with its people notified; a message with no mention is unchanged.
  `@everyone` counts as mentioning people. Only the people who would really be
  told count (project members other than the sender); a message that tags only
  non-members or only its sender is an ordinary message. Only a person's own
  start (composer or API client) is decided this way, never an automated one.
  In a conversation with a single agent among other people, `@Agent` now
  addresses that agent (web and mobile); `addressed_to` in a conversation with
  fewer than two agents is checked against its agent and routes to it.
- **History.** Later agent turns see the post as a user-role message from its
  author, marked `elitea_addressed: "people"` and led by a note that it was not
  addressed to the agent. It is identified by the `people_only` marker in the
  message group's `meta`, so the unanswered question of a failed turn stays out
  of the history. The reader's eligibility mode still applies.
- **No migration.** The message is a normal sender message group with no reply
  group, no execution and no addressee; `meta.people_only` marks it.
