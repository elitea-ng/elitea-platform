/** Pure execution-contract adapters used by `useChatBoxSend`. */
import { conversationApi } from '@/entities/conversation';
import type { useUploadAttachments } from '@/entities/conversation';
import type { ChatStreamContext } from '@/features/chat-messages';
import { markGrantsTurnedOff } from '@/features/unattended';

import type { StreamStartOutcome } from './useChatBoxHandlers.helpers';
import { addressedField, positiveParticipantId } from './useChatBoxSend.floor';

// Derive these types through the public entity API; avoid a cross-slice deep import.
type UploadAttachments = ReturnType<typeof useUploadAttachments>['uploadAttachments'];
type UploadAttachmentsParams = Parameters<UploadAttachments>[0];
export type UploadAttachmentsOutcome = Awaited<ReturnType<UploadAttachments>>;

/** The conversation-lifecycle and attachment-upload slices `useChatBoxSend` adapts. */
export interface SendDeps {
  readonly createConversation: (input: {
    name: string;
    isPrivate: boolean;
    participants?: readonly unknown[];
    meta?: Readonly<Record<string, unknown>>;
  }) => Promise<{ readonly id?: string | number; readonly uuid?: string } | undefined>;
  readonly uploadAttachments: (input: UploadAttachmentsParams) => Promise<UploadAttachmentsOutcome>;
}

/**
 * The slice of `useChatBoxSend`'s params this pure module reads. Declared
 * here, structurally, so the hook depends on its helpers and never the other
 * way around — importing the hook's own param type back into this file made
 * the pair circular (the layer gate's no-circular rule).
 */
export interface ChatStreamContextSource {
  /** The participant the turn goes to (`useResolvedTarget`); the answer is attributed to it. */
  readonly target?: unknown;
  readonly participants?: readonly unknown[] | undefined;
  readonly userName?: string | undefined;
  readonly userAvatar?: string | undefined;
}

/** Builds the participant identity consumed by the stream reducer. */
export function buildChatStreamContext(
  params: ChatStreamContextSource,
): ChatStreamContext {
  const participantId = (
    params.target as { id?: string | number } | undefined
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

/** Picker settings own the model identity; the saved chat model supplies only the default. */
export function resolveSendModelName(
  settings: Readonly<Record<string, unknown>> | undefined,
  fallbackName: string | undefined,
): string | undefined {
  const configuredName = settings?.['model_name'];
  return typeof configuredName === 'string' && configuredName ? configuredName : fallbackName;
}

/**
 * The turn's `llm_settings`, with the implied default's project filled in.
 *
 * A chat with no configured model runs on the saved-or-default model. That
 * model can be shared from another project, so the turn must name ITS project.
 * Without it the server resolves the name in the chat's own project (UI-PD-2).
 * A configured model keeps its own `model_project_id` untouched. A configured
 * model with NO `model_project_id` gets the project the picker resolved
 * (`configuredModelProjectId`), so the turn runs the model the picker shows.
 */
export function resolveSendLlmSettings(
  settings: Readonly<Record<string, unknown>> | undefined,
  model: { readonly name?: string | undefined; readonly projectId?: string | number | undefined } | null | undefined,
  configuredModelProjectId?: number,
): Readonly<Record<string, unknown>> | undefined {
  const configuredName = settings?.['model_name'];
  if (typeof configuredName === 'string' && configuredName !== '') {
    // A configured model with its own project keeps it.
    return settings?.['model_project_id'] == null
      ? withModelProject(settings, configuredModelProjectId)
      : settings;
  }
  return model?.name === undefined ? settings : withModelProject(settings, Number(model.projectId));
}

/** `settings` naming `projectId` as the model's project, when it is a positive integer. */
function withModelProject(
  settings: Readonly<Record<string, unknown>> | undefined,
  projectId: number | undefined,
): Readonly<Record<string, unknown>> | undefined {
  if (projectId === undefined || !Number.isInteger(projectId) || projectId <= 0) return settings;
  return { ...settings, model_project_id: projectId };
}

/** Returns the validated loop bound that belongs in conversation metadata. */
function executionStepsLimit(
  settings: Readonly<Record<string, unknown>> | undefined,
): number | undefined {
  const value = settings?.['steps_limit'];
  return typeof value === 'number' && Number.isInteger(value) && value > 0 ? value : undefined;
}

export { positiveParticipantId };

/** ADR-0050 D7c sticky target: the newest history row whose `participantId` (its `sent_to_id`) names an agent or the conversation's model. */
export function stickyAgentParticipantId(
  messages: readonly { readonly participantId?: string | undefined }[] | undefined,
  participants: readonly unknown[] | undefined,
): string | undefined {
  const agentIds = new Set(
    (participants ?? []).filter((p) => isApplicationParticipant(p) || isAdhocModelParticipant(p)).map((p) => String((p as { id?: unknown }).id)),
  );
  if (agentIds.size === 0) return undefined;
  for (let index = (messages?.length ?? 0) - 1; index >= 0; index -= 1) {
    const id = messages?.[index]?.participantId;
    if (id !== undefined && agentIds.has(id)) return id;
  }
  return undefined;
}

/** With nothing selected: the only agent, else the sticky one; unresolved, the server answers `agent_target_required` (issue 1602). */
export function resolveTargetParticipant(activeParticipant: unknown, participants: readonly unknown[] | undefined, stickyParticipantId?: string): unknown {
  if (isApplicationParticipant(activeParticipant) || isAdhocModelParticipant(activeParticipant)) return activeParticipant;
  if (participantEntityName(activeParticipant) === 'user') return activeParticipant;
  const adhocModel = (participants ?? []).find(isAdhocModelParticipant);
  if (activeParticipant !== undefined && activeParticipant !== null) return adhocModel;
  const applications = (participants ?? []).filter(isApplicationParticipant);
  if (applications.length === 1) return applications[0];
  const sticky = stickyParticipantId === undefined
    ? undefined
    : [...applications, adhocModel].find((p) => p !== undefined && String((p as { id?: unknown }).id) === stickyParticipantId);
  return sticky ?? activeParticipant;
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

/**
 * The project skill ids as NUMBERS, each once. Anything that is not a
 * positive integer is dropped: it cannot name a skill. A list past the five a
 * turn may use is NOT cut here: the start route answers 400 and the person
 * sees the refusal, rather than a message that silently ran without a skill.
 */
function projectSkillIDs(raw: unknown): number[] {
  if (!Array.isArray(raw)) return [];
  const ids: number[] = [];
  for (const value of raw) {
    const numeric = Number(value);
    if (Number.isInteger(numeric) && numeric > 0 && !ids.includes(numeric)) ids.push(numeric);
  }
  return ids;
}

function projectSkillsField(isApplicationTurn: boolean, ids: readonly number[]): { skills?: { source: 'project'; id: number }[] } {
  return isApplicationTurn && ids.length > 0 ? { skills: ids.map((id) => ({ source: 'project' as const, id })) } : {};
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
  /** The "Run to completion" grant (ADR-0045, `ChatUnattendedRequest`); absent while the control is off. */
  readonly unattended?: Readonly<Record<string, unknown>> | undefined;
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
  const projectSkills = projectSkillIDs(payload['projectSkillIds']);
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
    ...addressedField(payload), // ADR-0050 7c: the agents the message addresses
    ...(params.unattended === undefined ? {} : { unattended: params.unattended }),
    payload: {
      user_input: question,
      ...(payload['attachments'] ? { attachments: payload['attachments'] } : {}),
      // Project skills picked in the "~" menu: ids only, application turns only.
      ...projectSkillsField(params.isApplicationTurn, projectSkills),
    },
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
    { entity_name: 'dummy', entity_meta: {}, entity_settings: { llm_settings: llmSettings } },
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

/**
 * ADR-0045: Main refused the sent grant because an administrator turned
 * grants off. The control hides for this page load. True when the same
 * message goes again without the grant: a grant with only the continue parts
 * (an older Main refuses it). A grant that names sensitive tools keeps the
 * refusal, because the person asked for those answers.
 */
export function sendAgainWithoutGrant(outcome: StreamStartOutcome, grant: Readonly<Record<string, unknown>> | undefined): boolean {
  if (grant === undefined || outcome.started || outcome.reason !== 'rejected' || outcome.grantsOff !== true) return false;
  markGrantsTurnedOff();
  const tools = grant['sensitive_tools'];
  return !(Array.isArray(tools) && tools.length > 0);
}
