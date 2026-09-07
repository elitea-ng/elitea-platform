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
  const base = {
    project_id: Number.isFinite(numericProjectID) ? numericProjectID : params.projectId,
    conversation_uuid: params.conversationUuid,
    question_id: payload['question_id'],
    interaction_uuid: crypto.randomUUID(),
    mcp_tokens: params.mcpTokens ?? {},
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
  if (params.question === '' || params.questionId === '' || params.responseMessageId === '') return undefined;
  if ((params.updatedItems?.length ?? 0) > 0) return undefined;
  const numericProjectID = Number(params.projectId);
  if (!Number.isFinite(numericProjectID)) return undefined;
  return {
    payload: {
      user_input: params.question,
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
    updated_items: [],
  };
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
