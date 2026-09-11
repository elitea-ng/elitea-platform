# Shared chat actor authority

Baseline: Main `c56d19e9`. Current Core: `b701a00aeff0af1a416916c4a537bfdd4b7d8337`.

Main owns this slice. Rust needs no new product-data implementation.
The `elitea_core/chat` category publishes eleven ordinary operations.
Message execution and continuation remain outside this slice.

## Current source mapping

Current paths below start at `projects/centry/pylon_main/plugins/elitea_core`.
Main paths start at `services/elitea-main`.

| Current source | Main owner | Preserved behavior |
| --- | --- | --- |
| `api/v2/conversations.py::PromptLibAPI.get`; `rpc/chat_conversation.py::list_conversations_rpc` | `internal/api/v2/conversations/handler.go::List`; `internal/infra/db/chatauthority/access.go::Scope.Predicate` | Public rows and actor participant rows remain visible. Project admins retain the list exception. |
| `api/v2/conversation.py::PromptLibAPI.get`; `utils/conversation_utils.py::get_conversation_details` | `internal/infra/db/repos/conversation_authority.go::AuthorizeChatResource` | Private detail requires user participation. Admin detail additionally permits conversations with a nonempty single-participant descriptor. |
| `api/v2/conversations.py::PromptLibAPI.post` | `internal/api/v2/conversations/handler.go::Create`; `internal/infra/db/repos/conversations.go::Create` | Authentication supplies the author. Creation stores privacy, source, instructions, metadata, and initial participants. |
| `utils/participant_utils.py::get_or_create_one`; `api/v2/conversations.py::PromptLibAPI.post` | `internal/infra/db/repos/conversations.go::addConversationParticipant` | One transaction creates the conversation, participant identities, settings, and required user and dummy mappings. |
| `api/v2/folder.py::PromptLibAPI.get` | `internal/api/v2/folders/handler.go::loadConversations`; `internal/infra/db/repos/folders.go::List` | Actor visibility filters grouped, paged, pinned, and folder conversation results. A folder is visible through ownership or a visible conversation. |
| `api/v2/folder.py::PromptLibAPI.post` | `internal/infra/db/repos/folders.go::Create` | The authenticated user owns a new folder. |
| `api/v2/conversation.py::PromptLibAPI.put` | `internal/api/v2/conversations/handler.go::Update`; `internal/infra/db/repos/conversations.go::Update` | Instructions, privacy, folder, and metadata updates remain available after the object access check. Public conversations cannot become private. |

The role lookup also maps `admin/rpc/roles.py::check_user_is_admin`.
Main reads project role assignments from the authoritative authentication tables.
It recognizes the stored `admin` and `super_admin` roles.
It does not infer administrator access from caller fields or arbitrary role-name substrings.

## Authority boundary

`auth.User.OwningUserID` resolves the authenticated user.
A token row ID cannot become a conversation author or a visibility identity.
Missing identity fails before protected lookup.
Incomplete role projections and failed role lookups return safe errors.
An absent role projection grants no administrator exception.

Private visibility follows `chat_participant_mapping` and user participant metadata.
Matching only `chat_conversations.author_id` is insufficient.
The optional `mine` filter further narrows the visible conversation set.
It cannot enable access to another user's private conversation.

`AuthorizeChatResource` resolves numeric IDs and UUIDs through their owning conversation.
Conversation, message, and canvas identifiers use separate database joins.
The shared handlers call this boundary before reads or mutations.
This includes participant settings, message feedback, exports, attachments, and context operations.
Missing and inaccessible resources return the same not-found response.

Normal folder listings use actor participation without the general conversation-list administrator exception.
The configured support project retains its administrator listing and detail exceptions for support conversations.
Ordinary support users require participation.
Main does not preserve the legacy support detail bypass for every project member.

Folder mutation and position rebalancing require actor ownership.
Visibility through a shared conversation does not grant folder mutation authority.
Moving a conversation also requires ownership of the target folder.
These checks close unscoped legacy mutations.

## Atomic creation

The repository derives the author again at the persistence boundary.
It ignores a caller-supplied author, including a conflicting token identifier.
It preserves the supplied privacy value and normalized source.
An omitted source defaults to `elitea`.
The configured public project refuses public conversations during creation and update.

The transaction inserts the conversation and all participant mappings.
The shared participant helper preserves identity reuse and per-conversation settings.
User and dummy participants are always present.
Duplicate input for those identities does not create duplicate mappings.
The response returns persisted participant mappings and metadata.
`AddParticipants` also uses one transaction for a complete ordinary addition batch.
The shared handler validates all participant identities before writing the first mapping.
The ordinary REST addition accepts a single participant object or an array, as current Core does.
A participant failure rolls back the conversation and newly created participant identities.

## Creation defaults and participant settings

`utils/conversation_utils.py::resolve_persona_instructions` maps to
`internal/api/v2/conversations/handler.go::applyCreatePersonalization`.
`internal/infra/db/repos/user_context_defaults.go::Personalization` reads only the authenticated actor.
The selected persona's instructions take precedence over flat legacy instructions.
An explicit persona map with no matching entry does not fall back to another persona or flat text.
Explicit conversation instructions remain intact.

`utils/chat_feature_flags.py::get_context_manager_feature_flag` maps to
`internal/api/chat_defaults.go::ContextManagementEnabled`.
It resolves only the `context_manager` project secret through the shared vault handler.
An absent value defaults to enabled. A vault failure is an error.
`utils/context_analytics.py::set_context_strategy` maps to
`internal/api/v2/conversations/create_defaults.go::applyCreateContextDefaults` and
`internal/domain/contextsettings/userdefaults.go::Resolve`.
The resolved actor defaults are stored in the creation transaction's `meta.context_strategy`.
No secret value enters a chat or MCP response.

`api/v2/entity_settings.py::PromptLibAPI._put` maps to
`internal/api/v2/conversations/handler.go::UpdateEntitySettings` and
`internal/api/v2/conversations/participant_settings.go`.
The shared handler requires an actual mapping, normalizes a numeric version ID,
rejects temperature/reasoning conflicts, and checks private-agent overrides against the normalized version baseline.
Its baseline comparison includes reasoning effort and model project identity.
Unversioned private-agent overrides are refused.
The composed `chatDefaults.SupportsReasoning` reads configuration model metadata, without credentials.
As current Core does, a failed optional capability lookup does not block a valid-shaped settings write.
Successful settings updates return the participant and stored settings.

`utils/participant_utils.py::delete_participant_from_conversation` maps to
`internal/infra/db/repos/conversations.go::RemoveParticipant`.
The conversation author cannot be removed. Removing another participant clears an attachment-participant reference in the same transaction.

## Ordinary MCP tools

`internal/api/v2/mcp/internal_chat_catalog.go::internalChatTools` owns the explicit schemas.
`internal_chat_execute.go::handlerInternalChatExecutor.Execute` validates schemas again on execution.
It invokes the composed shared handlers through `invokeInternalHandler`.
`Handler.callInternalChatTool` uses `callInternalTool` for endpoint project authority,
authenticated token-owner identity, exact permission checks, and safe errors.
`internal/api/router.go` constructs the conversation and folder handlers once for REST and MCP.

| MCP name | Current opt-in source | Shared Main handler | Permission |
| --- | --- | --- | --- |
| `get_elitea_core_conversations` | `api/v2/conversations.py::PromptLibAPI.get` | `conversations.Handler.List` | `models.chat.conversations.list` |
| `post_elitea_core_conversations` | `api/v2/conversations.py::PromptLibAPI.post` | `conversations.Handler.Create` | `models.chat.conversations.create` |
| `get_elitea_core_conversation` | `api/v2/conversation.py::PromptLibAPI.get` | `conversations.Handler.Get` | `models.chat.conversation.details` |
| `put_elitea_core_conversation` | `api/v2/conversation.py::PromptLibAPI.put` | `conversations.Handler.Update` | `models.chat.conversation.update` |
| `get_elitea_core_participant` | `api/v2/participant.py::PromptLibAPI.get` | `conversations.Handler.GetParticipant` | `models.chat.participant.get` |
| `delete_elitea_core_participant` | `api/v2/participant.py::PromptLibAPI.delete` | `conversations.Handler.RemoveParticipant` | `models.chat.participant.delete` |
| `post_elitea_core_participants` | `api/v2/participants.py::PromptLibAPI.post` | `conversations.Handler.AddParticipant` | `models.chat.participants.create` |
| `patch_elitea_core_entity_settings` | `api/v2/entity_settings.py::PromptLibAPI.patch` | `conversations.Handler.UpdateEntitySettings` | `models.chat.entity_settings.update` |
| `get_elitea_core_folder` | `api/v2/folder.py::PromptLibAPI.get` | `folders.Handler.List` | `models.chat.folders.get` |
| `post_elitea_core_folder` | `api/v2/folder.py::PromptLibAPI.post` | `folders.Handler.Create` | `models.chat.folders.create` |
| `put_elitea_core_folder` | `api/v2/folder.py::PromptLibAPI.put` | `folders.Handler.Update` | `models.chat.folders.update` |

Participant reads require a conversation ID to avoid the legacy unscoped participant lookup.
Participant settings use flat fields, including `version_id`, `variables`, and `llm_settings`.
The MCP addition schema wraps its bounded array in `participants`; the adapter sends the shared handler's array body.
Author and folder-owner arguments are not published and are rejected by schema validation.
Read pagination is bounded; unsupported fields are refused instead of being silently ignored.
The MCP detail schema exposes `messages_limit` and ordering, but does not claim message-offset support.
The MCP update schema exposes instructions, privacy, name, folder, and metadata; it does not publish hidden-state or attachment-storage mutation arguments.

## Verification

`internal/api/v2/conversations/authority_postgres_integration_test.go` uses isolated PostgreSQL databases.
It tests these boundaries:

- Forged authors, token owners, missing identities, and stored create fields.
- Required participants, initial settings, persisted response metadata, and transaction rollback.
- Private and public reads through numeric IDs and UUIDs.
- Denied conversation, participant, message, canvas, and folder operations.
- Participant visibility without authorship and inaccessible folder contents.
- Ordinary administrator exceptions, foreign project roles, and support administrator behavior.
- Safe role-lookup failures and public-project creation refusal.
- Persona isolation, explicit instructions, and enabled/disabled context-default snapshots.
- Author removal refusal, mapping removal, attachment-reference clearing, and batch rollback.
- Missing settings mappings, private-agent baseline checks, and reasoning-model validation.
- Inaccessible selected-conversation IDs are omitted from grouped folder responses.

The existing conversation filter and folder paging/pinning suites include explicit participant fixtures.
Their tests retain pagination and pin behavior without depending on private data exposure.
The prior unfiltered-private-list assertion now requires actor exclusion.

The integration rerun passes 496 chat and MCP tests without skips.
A separate PostgreSQL regression verifies missing participant settings return 404.
Mapped participant settings still persist successfully.
The MCP adapter retains token identity when it sets the owning user.

`internal/api/v2/mcp/internal_chat_test.go` checks all eleven schemas and dispatch methods,
array and flat-settings transport, preserved token identity, forged authority refusals,
exact permission failures, endpoint project clamping, and a successful protocol mutation.

This proof uses real handlers and repositories against PostgreSQL.
It does not prove deployed browser behavior or independent service execution.

## Remaining boundaries

Send and continuation still need bounded waiting, cancellation, HITL, authorization resume, and terminal-result projection.
Participant display enrichment and cross-entity availability resolution retain the existing Main participant implementation.
They are not a claim of deployed worker execution or complete legacy entity-enrichment parity.
Concurrent participant identity creation retains the existing transaction behavior.
Object authorization occurs at request admission; this slice does not claim atomic revocation during an admitted operation.
Broader chat category parity remains a separate operation-level gate.
