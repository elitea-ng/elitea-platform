/**
 * Wire shapes this slice's normalisers accept — a persisted `message_group`
 * DB row plus the sibling `users`/`participants` context arrays
 * `convertToUserQuestion`/`convertToAIAnswer` receive as separate
 * parameters. No OpenAPI schema exists for this resource (see
 * model/types.ts's docblock), so every field below is evidenced directly
 * from apps/elitea-ui/src/common/convertChatConversationMessages.js:35-313
 * (line ranges noted per interface) rather than a generated zod file.
 *
 * Two fields the source destructures UNGUARDED off an object that could in
 * principle be absent (`message_group.meta` at line 45; `foundUser.meta`/
 * `.entity_meta` at lines 68 and 72) are modelled here as optional and read
 * with `?.` in lib/normalise.ts — a deliberate, disclosed hardening (no
 * schema exists to confirm those are ever guaranteed non-null) rather than
 * a literal transcription of the source's latent null-deref risk.
 */

/** `message_group.message_items[]` entries (lines 39, 115, 291, 299). */
export interface MessageItemWire {
  readonly id: number;
  readonly item_details?: { readonly content?: string };
}

/**
 * A `users` array entry (lines 47, 53-62, 67-68, 72). `id` is the
 * chat_participants row id; the Go participants payload serialises it as a
 * NUMBER while socket-era payloads carried strings, so both spellings are
 * wire truth and every lookup against it compares through `String(...)`.
 */
export interface MessageAuthorWire {
  readonly id: string | number;
  readonly meta?: { readonly user_name?: string; readonly user_avatar?: string };
  readonly entity_meta?: { readonly email?: string; readonly id?: string | number };
}

/**
 * A `participants` array entry (lines 48, 74-78, 128-129, 205).
 *
 * `entity_meta` carries the AUTH USER id (`entity_meta.id`), which is a
 * different number from `id` (the chat_participants ROW id) — measured on the
 * live stack, participant row `1` carries `entity_meta.id: 6`. It is declared
 * here because the conversation-details payload serves it on every
 * participant and this app already relies on that: `convertMessagesToChatHistory`
 * and `useSyncChatMessage`'s `filterUserParticipants` both hand the SAME
 * participant objects to `normaliseUserMessage` as its `users:
 * MessageAuthorWire[]` argument, whose `userOptionalFields` reads
 * `entity_meta.id` off them. Same optionality and same `string | number`
 * union as `MessageAuthorWire` above, for the same reason.
 */
export interface MessageParticipantWire {
  readonly id: string;
  readonly meta?: { readonly tools?: readonly MessageParticipantToolWire[]; readonly user_name?: string; readonly user_avatar?: string };
  readonly entity_meta?: { readonly email?: string; readonly id?: string | number };
}

/** `foundParticipant.meta.tools[]` (line 205). */
export interface MessageParticipantToolWire {
  readonly name?: string;
  readonly toolkit_name?: string;
  readonly type?: string;
}

/** A `thinking_steps[]` entry (lines 152-190). */
export interface ThinkingStepWire {
  readonly text?: string;
  readonly thinking?: unknown;
  readonly message?: {
    readonly id?: string;
    readonly response_metadata?: { readonly model_name?: string; readonly tool_name?: string };
  };
  readonly parent_agent_name?: string;
  readonly timestamp_start?: string;
  readonly timestamp_finish?: string;
}

/** A `tool_calls` entry (lines 192-246). */
export interface ToolCallStepWire {
  readonly tool_name?: string;
  readonly name?: string;
  readonly toolkit_name?: string;
  readonly toolkit_type?: string;
  readonly tool_meta?: {
    readonly name?: string;
    readonly model_name?: string;
    readonly display_name?: string;
    readonly icon_meta?: unknown;
    readonly metadata?: {
      readonly toolkit_name?: string;
      readonly toolkit_type?: string;
      readonly display_name?: string;
      readonly agent_type?: string;
    };
  };
  readonly metadata?: {
    readonly original_name?: string;
    readonly checkpoint_ns?: string;
    readonly toolkit_type?: string;
    readonly parent_agent_name?: string;
    readonly parent_agent_call_id?: string;
    readonly mcp_server_url?: string;
    readonly langgraph_node?: string;
  };
  readonly tool_run_id?: string;
  readonly tool_inputs?: unknown;
  readonly tool_output?: unknown;
  readonly timestamp_start?: string;
  readonly timestamp_finish?: string;
  readonly content?: string;
  readonly error?: unknown;
}

/** Raw persisted HITL interrupt dict (chat.helpers.js:85-105). */
export interface HitlInterruptRawWire {
  readonly message?: string;
  readonly node_name?: string;
  readonly available_actions?: readonly string[];
  readonly routes?: unknown;
  readonly edit_state_key?: string;
  readonly guardrail_type?: string;
  readonly tool_name?: string;
  readonly toolkit_name?: string;
  readonly toolkit_type?: string;
  readonly action_label?: string;
  readonly tool_args?: unknown;
  readonly policy_message?: string;
  readonly tool_call_id?: string;
  readonly child_thread_id?: string;
  readonly parent_agent_name?: string;
  readonly thread_id?: string;
}

/** `message_group.meta` (lines 45, 121, 131-139, 271-275). */
export interface MessageGroupMetaWire {
  readonly interaction_uuid?: string;
  readonly references?: unknown;
  readonly is_error?: boolean;
  readonly error?: unknown;
  readonly thinking_steps?: readonly ThinkingStepWire[];
  readonly tool_calls?: Readonly<Record<string, ToolCallStepWire>> | readonly ToolCallStepWire[];
  readonly first_tool_timestamp_start?: string;
  readonly context?: { readonly included?: boolean };
  readonly hitl_interrupt?: HitlInterruptRawWire;
  readonly hitl_interrupts?: readonly HitlInterruptRawWire[];
  readonly thread_id?: string;
  readonly output_limit_reached?: boolean;
  readonly output_limit_sequence?: number | string;
  readonly authorization_requests?: readonly Record<string, unknown>[];
  /**
   * How many of the caller's persistent, cross-conversation memories (#870)
   * this turn's recall injected into the prompt. Stamped by
   * `internal/infra/db/repos.MemoriesRepo.RecordCurrentMemoryUsage`, a
   * best-effort merge (`meta || jsonb_build_object(...)`) into this SAME
   * `chat_message_group.meta` column after the turn is admitted — never
   * present on a turn that recalled nothing (0 is not stamped at all, so
   * "absent" and "0" mean the same thing to a reader).
   */
  readonly memories_used?: number;
}

/**
 * A persisted `message_group` row (lines 36-46, 112-126).
 *
 * `id`/`reply_to_id`/`question_id` are no longer an unevidenced guess: BOTH
 * spellings are wire truth, and ONE payload carries both. The transcript
 * route `GET /elitea_core/messages/prompt_lib/{project}/{conversation}`
 * answers `{"id": "1121", ...}` beside `{"id": "1122", "reply_to_id": 1121}`
 * — measured on the live stack, project 90106 conversation 471 — because Go
 * types the row id `Message.ID string` (`strconv.Itoa` of the DB int) and the
 * FK `ReplyToID *int`
 * (services/elitea-main/internal/api/v2/conversations/handler.go:41,57),
 * while the other producer, `ListMessageGroups`, numbers BOTH
 * (internal/infra/db/repos/conversations.go:1618,1631).
 *
 * No producer emits `question_id` on a persisted row at all — the only writer
 * in this app is `pages/chat/useChatPageData.ts`'s adapter, which synthesises
 * it from a sibling row's `id` and so carries whichever spelling that row
 * had. So the union stays, and every comparison against these three goes
 * through `isMessageRow` in lib/normalise.ts rather than `===`.
 */
export interface MessageGroupWire {
  readonly id: string | number;
  readonly uuid: string;
  /**
   * The author's chat_participants row id. The Go transcript endpoint
   * (`GET /elitea_core/messages/...`) serialises it as a NUMBER; the
   * message-groups shape carried strings. Absent = the row states no author.
   */
  readonly author_participant_id?: string | number;
  readonly content: string;
  readonly message_items?: readonly MessageItemWire[];
  readonly created_at: string;
  readonly updated_at?: string;
  readonly reply_to_id?: string | number;
  readonly question_id?: string | number;
  readonly task_id?: string;
  readonly is_streaming?: boolean;
  readonly sent_to_id?: string | number;
  readonly sent_to?: { readonly entity_name?: string };
  readonly likes?: number;
  readonly meta?: MessageGroupMetaWire;
}
