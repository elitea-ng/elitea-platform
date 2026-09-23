/** Pure execution-contract adapters used by `useChatBoxSend`. */
import { conversationApi } from '@/entities/conversation';
import type { ChatStreamContext } from '@/features/chat-messages';

/**
 * The slice of `useChatBoxSend`'s params this pure module reads. Declared
 * here, structurally, so the hook depends on its helpers and never the other
 * way around — importing the hook's own param type back into this file made
 * the pair circular (the layer gate's no-circular rule).
 */
export interface ChatStreamContextSource {
  readonly activeParticipant?: unknown;
  readonly participants?: readonly unknown[] | undefined;
  readonly userName?: string | undefined;
  readonly userAvatar?: string | undefined;
}

/** Builds the participant identity consumed by the stream reducer. */
export function buildChatStreamContext(
  params: ChatStreamContextSource,
): ChatStreamContext {
  const target = resolveTargetParticipant(
    params.activeParticipant,
    params.participants,
  );
  const participantId = (
    target as { id?: string | number } | undefined
  )?.id;
  return {
    ...(participantId !== undefined
      ? { participantId: String(participantId) }
      : {}),
    ...(params.userName !== undefined ? { name: params.userName } : {}),
    ...(params.userAvatar !== undefined ? { avatar: params.userAvatar } : {}),
    ...(params.participants !== undefined
      ? {
          participants:
            params.participants as ChatStreamContext['participants'],
        }
      : {}),
  };
}

function participantEntityName(participant: unknown): string {
  const name = (participant as { readonly entity_name?: unknown } | null | undefined)?.entity_name;
  return typeof name === 'string' ? name : '';
}

function isApplicationParticipant(participant: unknown): boolean {
  const name = participantEntityName(participant);
  return name === 'application' || name === 'pipeline';
}

function isAdhocModelParticipant(participant: unknown): boolean {
  return participantEntityName(participant) === 'dummy';
}

/** The execution-loop bound is not a provider/model parameter. */
function modelRequestSettings(
  settings: Readonly<Record<string, unknown>> | undefined,
): Readonly<Record<string, unknown>> {
  const { steps_limit: _stepsLimit, ...modelSettings } = settings ?? {};
  return modelSettings;
}

/** Returns the validated loop bound that belongs in conversation metadata. */
export function executionStepsLimit(
  settings: Readonly<Record<string, unknown>> | undefined,
): number | undefined {
  const value = settings?.['steps_limit'];
  return typeof value === 'number' && Number.isInteger(value) && value > 0 ? value : undefined;
}

export function positiveParticipantId(raw: unknown): number | undefined {
  if (raw === undefined || raw === null || raw === '') return undefined;
  const numeric = Number(raw);
  return Number.isInteger(numeric) && numeric > 0 ? numeric : undefined;
}

export function resolveTargetParticipant(activeParticipant: unknown, participants: readonly unknown[] | undefined): unknown {
  if (isApplicationParticipant(activeParticipant) || isAdhocModelParticipant(activeParticipant)) return activeParticipant;
  if (participantEntityName(activeParticipant) === 'user') return activeParticipant;
  const adhocModel = (participants ?? []).find(isAdhocModelParticipant);
  if (activeParticipant !== undefined && activeParticipant !== null) return adhocModel;
  const applications = (participants ?? []).filter(isApplicationParticipant);
  return applications.length === 1 ? applications[0] : activeParticipant;
}

export function resolveStartContract(target: unknown): string {
  return isApplicationParticipant(target) ? conversationApi.contracts.application : conversationApi.contracts.adhoc;
}

/**
 * The mention list the start route parses, as NUMBERS.
 *
 * `route.go`'s `parseMentionedUserIDs` unmarshals `user_ids` into `[]int64`
 * and answers 400 for anything else, so a list of strings — which is what the
 * composer's payload holds, `ResolvedUserMention.userId` being a string —
 * would refuse the whole turn rather than the mention. Anything that is not a
 * positive integer is dropped HERE: it cannot name a user, and including it
 * would trade a lost notification for a lost message.
 */
function mentionedUserIDs(raw: unknown): number[] {
  if (!Array.isArray(raw)) return [];
  const ids: number[] = [];
  for (const value of raw) {
    const numeric = Number(value);
    if (Number.isInteger(numeric) && numeric > 0 && !ids.includes(numeric)) ids.push(numeric);
  }
  return ids;
}

export function buildStartBody(params: {
  readonly mcpTokens?: Readonly<Record<string, unknown>>;
  readonly conversationUuid: string;
  readonly projectId: string | undefined;
  readonly payload: Record<string, unknown>;
  readonly llmSettings: Readonly<Record<string, unknown>> | undefined;
  readonly modelName: string | undefined;
  readonly isApplicationTurn: boolean;
  readonly participantId: number | undefined;
}): Record<string, unknown> | undefined {
  const { payload } = params;
  const question = typeof payload['question'] === 'string' ? payload['question'] : '';
  const numericProjectID = Number(params.projectId);
  // THE @MENTIONS, which used to stop here.
  //
  // The composer resolves an `@` into `isSendingToUser`/`userIds` and puts
  // them on its `chat_predict` payload; this builder emitted `user_input` and
  // `attachments` alone, so every mention made through the UI reached the
  // start route as an ordinary message and notified nobody. The server half
  // (#977) was complete the whole time — it parses `user_ids`/`userIds` at the
  // TOP level of the body and `is_mentioning_everyone` beside it — and the E2E
  // that covered it replayed a captured POST with the field injected, so CI
  // never exercised the composer that is supposed to produce it.
  const mentioned = mentionedUserIDs(payload['userIds']);
  const mentionsEveryone = payload['isMentioningEveryone'] === true;
  const mentions = {
    ...(mentioned.length > 0 ? { user_ids: mentioned } : {}),
    // Sent only when true. `@everyone` is re-resolved from the project's
    // membership server-side, so the ids above are a hint the server may
    // ignore for it — but the FLAG is the only thing that asks it to.
    ...(mentionsEveryone ? { is_mentioning_everyone: true } : {}),
  };
  const base = {
    project_id: Number.isFinite(numericProjectID) ? numericProjectID : params.projectId,
    conversation_uuid: params.conversationUuid,
    question_id: payload['question_id'],
    interaction_uuid: crypto.randomUUID(),
    mcp_tokens: params.mcpTokens ?? {},
    ...mentions,
    payload: { user_input: question, ...(payload['attachments'] ? { attachments: payload['attachments'] } : {}) },
  };
  if (params.isApplicationTurn) {
    if (params.participantId === undefined) return undefined;
    return { ...base, participant_id: params.participantId };
  }
  return {
    ...base,
    participant_id: params.participantId ?? 0,
    llm_settings: {
      ...modelRequestSettings(params.llmSettings),
      ...(params.modelName !== undefined ? { model_name: params.modelName } : {}),
      stream: true,
    },
  };
}

export function buildRegenerateBody(params: {
  readonly mcpTokens?: Readonly<Record<string, unknown>>;
  readonly conversationUuid: string;
  readonly projectId: string | undefined;
  readonly responseMessageId: string;
  readonly questionId: string;
  readonly question: string;
  readonly llmSettings: Readonly<Record<string, unknown>> | undefined;
  readonly modelName: string | undefined;
  readonly isApplicationTurn: boolean;
  readonly participantId: number | undefined;
  readonly updatedItems?: readonly unknown[] | undefined;
}): Record<string, unknown> | undefined {
  // THE TEXT THIS REGENERATION RUNS FROM. An edited question runs from the
  // text the user just typed, not from the question the transcript still
  // holds (issue 980); a retry runs from the stored one. Reading it off the
  // item rather than off `question` keeps the body's `user_input` and its
  // `updated_items` describing the SAME question — the server validates the
  // first and rewrites the second, so a mismatch would answer one question
  // and store another.
  const edited = editedQuestionText(params.updatedItems);
  const userInput = edited ?? params.question;
  if (userInput === '' || params.questionId === '' || params.responseMessageId === '') return undefined;
  const numericProjectID = Number(params.projectId);
  if (!Number.isFinite(numericProjectID)) return undefined;
  return {
    payload: {
      user_input: userInput,
      attachments_info: [],
      mcp_tokens: params.mcpTokens ?? {},
      ...(!params.isApplicationTurn
        ? {
            llm_settings: {
              ...modelRequestSettings(params.llmSettings),
              ...(params.modelName !== undefined ? { model_name: params.modelName } : {}),
              stream: true,
            },
          }
        : {}),
    },
    project_id: numericProjectID,
    participant_id: params.participantId ?? 0,
    conversation_uuid: params.conversationUuid,
    question_id: params.questionId,
    message_id: params.responseMessageId,
    stream_id: params.responseMessageId,
    regeneration_id: crypto.randomUUID(),
    // The edit itself, in the shape the regenerate route parses (one
    // `text_message` entry, its `uuid` only when the message carries a stored
    // item). Empty for a retry, which is what every regeneration sent before
    // the route learned to accept one.
    updated_items: params.updatedItems ?? [],
  };
}

/**
 * The text of the one `text_message` entry in `updated_items`, or `undefined`
 * when this regeneration carries no edit.
 *
 * Narrow on purpose: the route admits exactly one such entry, so anything else
 * here is a body this client should not be building.
 */
function editedQuestionText(updatedItems: readonly unknown[] | undefined): string | undefined {
  if (updatedItems === undefined || updatedItems.length !== 1) return undefined;
  const item = updatedItems[0];
  if (typeof item !== 'object' || item === null) return undefined;
  const record = item as { readonly item_type?: unknown; readonly content?: unknown };
  if (record.item_type !== 'text_message' || typeof record.content !== 'string') return undefined;
  return record.content.trim() === '' ? undefined : record.content;
}

export function adhocParticipants(input: {
  readonly userId: string | undefined;
  readonly modelName: string;
  readonly llmSettings: Readonly<Record<string, unknown>> | undefined;
}): { readonly entity_name: string; readonly entity_meta?: Record<string, unknown>; readonly entity_settings?: Record<string, unknown> }[] {
  const llmSettings = { ...modelRequestSettings(input.llmSettings), model_name: input.modelName, stream: true };
  return [
    ...(input.userId !== undefined ? [{ entity_name: 'user', entity_meta: { id: Number(input.userId) } }] : []),
    { entity_name: 'dummy', entity_meta: { name: input.modelName }, entity_settings: { llm_settings: llmSettings } },
  ];
}

export function creationMeta(settings: Readonly<Record<string, unknown>> | undefined, internalTools: readonly string[] | undefined): Record<string, unknown> {
  const stepsLimit = executionStepsLimit(settings);
  return {
    ...(stepsLimit !== undefined ? { steps_limit: stepsLimit } : {}),
    ...(internalTools !== undefined ? { internal_tools: internalTools } : {}),
  };
}

export async function internalToolsSaveFailure(getTools: (() => Promise<readonly string[]>) | undefined): Promise<{ readonly started: false; readonly reason: 'rejected'; readonly message: string } | undefined> {
  try { await getTools?.(); return undefined; }
  catch { return { started: false, reason: 'rejected', message: 'Internal tools configuration could not be saved. Select the tools again and retry.' }; }
}
