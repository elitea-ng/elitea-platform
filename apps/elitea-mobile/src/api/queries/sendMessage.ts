import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { ApiError, parseRetryAfter } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { isSameOriginApiPath } from '@/api/paths';

/**
 * Chat send: `sendChatMessage` (client contract), with the application
 * contract for an agent and the adhoc contract for a plain model.
 *
 * The body is serialised ONCE and the same string is sent on every retry:
 * the server de-duplicates by `question_id` only for a byte-identical body
 * (same body → the original execution with `created:false`; a different body
 * with the same id → 409).
 */

export type ChatSendRequest = components['schemas']['ChatSendRequest'];

export const EXECUTION_CONTRACT = 'agent.execute.application.v1';
/** A plain model turn (a conversation with no agent): `llm_settings` names the model. */
export const ADHOC_EXECUTION_CONTRACT = 'agent.execute.adhoc.v1';
export type ExecutionContract = typeof EXECUTION_CONTRACT | typeof ADHOC_EXECUTION_CONTRACT;
export const MAX_TURN_BODY_BYTES = 512 * 1024;
export const QUESTION_ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** The model a plain model turn runs on. */
export interface TurnModel {
  name: string;
  /** The project the model belongs to, when it is shared from another one. */
  projectId: number | null;
}

/** Who a message mentions (client contract `user_ids` / `is_mentioning_everyone`). */
export interface TurnMentions {
  /** USER ids (`entity_meta.id` of a user participant), never participant row ids. */
  userIds: number[];
  /** `@Everyone`: every project member except the sender is notified. */
  everyone: boolean;
}

/** One uploaded file the message carries (`payload.attachments`). */
export interface TurnAttachment {
  /** Exactly the `filepath` the upload returned: `/{bucket}/{conversation_uuid}/{name}`. */
  filepath: string;
  /** Display name; the server ignores it. */
  name: string;
}

/** The server refuses more mention ids than this (400). */
export const MAX_MENTION_IDS = 64;
/** The most attachments one body may carry (the policy's `max_files` is lower). */
export const MAX_TURN_ATTACHMENTS = 64;

export interface TurnRequest {
  projectId: number;
  conversationUuid: string;
  /**
   * The addressed participant: the agent (application contract, at least 1),
   * or the conversation's model participant, 0 when it has none (`model` set).
   */
  participantId: number;
  questionId: string;
  userInput: string;
  /** Set for a plain model turn (adhoc contract); absent for an agent turn. */
  model?: TurnModel | null;
  mentions?: TurnMentions | null;
  attachments?: TurnAttachment[] | null;
}

export interface PreparedTurn {
  request: TurnRequest;
  /** The exact bytes sent on every attempt. */
  bodyJson: string;
}

export class TurnInputError extends Error {
  constructor(readonly reason: 'question_id' | 'too_large' | 'empty' | 'invalid' | 'mentions' | 'attachments') {
    super(`invalid turn: ${reason}`);
    this.name = 'TurnInputError';
  }
}

const utf8Bytes = (s: string) => new TextEncoder().encode(s).byteLength;

/** Builds and serialises the send body; throws `TurnInputError` when it cannot be sent. */
export function prepareTurn(request: TurnRequest): PreparedTurn {
  if (!QUESTION_ID_RE.test(request.questionId)) throw new TurnInputError('question_id');
  if (request.userInput.trim() === '') throw new TurnInputError('empty');
  if (!Number.isSafeInteger(request.projectId) || request.projectId <= 0) throw new TurnInputError('invalid');
  const model = request.model ?? null;
  const minParticipant = model === null ? 1 : 0;
  if (!Number.isSafeInteger(request.participantId) || request.participantId < minParticipant) {
    throw new TurnInputError('invalid');
  }
  if (!/^[0-9a-fA-F-]{36}$/.test(request.conversationUuid)) throw new TurnInputError('invalid');
  if (model !== null) {
    if (model.name.trim() === '' || model.name.length > 500) throw new TurnInputError('invalid');
    if (model.projectId !== null && (!Number.isSafeInteger(model.projectId) || model.projectId <= 0)) {
      throw new TurnInputError('invalid');
    }
  }
  const userIds = normaliseMentionIds(request.mentions?.userIds ?? []);
  const everyone = request.mentions?.everyone === true;
  const attachments = checkAttachments(request.attachments ?? [], request.conversationUuid);
  const body: ChatSendRequest = {
    payload: {
      user_input: request.userInput,
      ...(attachments.length > 0 ? { attachments: attachments.map((a) => ({ filepath: a.filepath, name: a.name })) } : {}),
    },
    project_id: request.projectId,
    participant_id: request.participantId,
    conversation_uuid: request.conversationUuid,
    question_id: request.questionId,
    ...(userIds.length > 0 ? { user_ids: userIds } : {}),
    ...(everyone ? { is_mentioning_everyone: true } : {}),
    ...(model === null
      ? {}
      : {
          llm_settings: {
            model_name: model.name,
            ...(model.projectId !== null ? { model_project_id: model.projectId } : {}),
            stream: true,
          },
        }),
  };
  const bodyJson = JSON.stringify(body);
  if (utf8Bytes(bodyJson) > MAX_TURN_BODY_BYTES) throw new TurnInputError('too_large');
  return { request, bodyJson };
}

/**
 * A stored outbox body. Rows queued by earlier builds hold the overlay's
 * `ApplicationTurnRequest` (participant_id always present); the client
 * contract's `ChatSendRequest` makes participant_id optional (absent = 0, a
 * model chat). Both parse; the stored bytes are never rewritten.
 */
/**
 * Mention ids, deduped in first-seen order. Every entry must be a positive
 * safe integer (a string id is a caller bug: 400 on the server); more than
 * `MAX_MENTION_IDS` distinct ids is refused.
 */
function normaliseMentionIds(ids: readonly unknown[]): number[] {
  const out: number[] = [];
  for (const id of ids) {
    if (typeof id !== 'number' || !Number.isSafeInteger(id) || id <= 0) throw new TurnInputError('mentions');
    if (!out.includes(id)) out.push(id);
  }
  if (out.length > MAX_MENTION_IDS) throw new TurnInputError('mentions');
  return out;
}

const FILEPATH_MAX = 1024;

/**
 * Every attachment must be a filepath an upload into THIS conversation
 * returned (`/{bucket}/{conversation_uuid}/{name}`), so a stale or foreign
 * path never reaches the server.
 */
function checkAttachments(list: readonly TurnAttachment[], conversationUuid: string): TurnAttachment[] {
  if (list.length > MAX_TURN_ATTACHMENTS) throw new TurnInputError('attachments');
  for (const item of list) {
    if (typeof item?.filepath !== 'string' || item.filepath.length > FILEPATH_MAX) throw new TurnInputError('attachments');
    const [lead, bucket, uuid, name, ...rest] = item.filepath.split('/');
    const ok =
      lead === '' &&
      rest.length === 0 &&
      typeof bucket === 'string' &&
      /^[A-Za-z0-9._-]+$/.test(bucket) &&
      uuid?.toLowerCase() === conversationUuid.toLowerCase() &&
      typeof name === 'string' &&
      name !== '' &&
      name !== '.' &&
      name !== '..' &&
      !/[\u0000-\u001f\\]/.test(name);
    if (!ok) throw new TurnInputError('attachments');
    if (typeof item.name !== 'string' || item.name.trim() === '' || item.name.length > 255) throw new TurnInputError('attachments');
  }
  return [...list];
}

const StoredBody = z.object({
  payload: z.object({
    user_input: z.string(),
    attachments: z.array(z.object({ filepath: z.string(), name: z.string().optional() })).optional(),
  }),
  project_id: z.number(),
  participant_id: z.number().optional(),
  conversation_uuid: z.string(),
  question_id: z.string(),
  user_ids: z.array(z.number()).optional(),
  is_mentioning_everyone: z.boolean().optional(),
  llm_settings: z
    .looseObject({ model_name: z.string(), model_project_id: z.number().optional(), stream: z.boolean().optional() })
    .optional(),
});

/** The contract a prepared turn is sent with: a model turn is adhoc. */
export function executionContractOf(request: Pick<TurnRequest, 'model'>): ExecutionContract {
  return request.model ? ADHOC_EXECUTION_CONTRACT : EXECUTION_CONTRACT;
}

/**
 * A prepared turn back from its stored bytes (the outbox). The same checks
 * as `prepareTurn`, and the body is NOT re-serialised: the stored string is
 * sent as is, so the server sees the identical body every time.
 */
export function restorePreparedTurn(bodyJson: string): PreparedTurn {
  let parsed: z.infer<typeof StoredBody>;
  try {
    parsed = StoredBody.parse(JSON.parse(bodyJson));
  } catch {
    throw new TurnInputError('invalid');
  }
  const request: TurnRequest = {
    projectId: parsed.project_id,
    conversationUuid: parsed.conversation_uuid,
    participantId: parsed.participant_id ?? 0,
    questionId: parsed.question_id,
    userInput: parsed.payload.user_input,
    model: parsed.llm_settings
      ? { name: parsed.llm_settings.model_name, projectId: parsed.llm_settings.model_project_id ?? null }
      : null,
    // Present only when the stored body carried them.
    ...(parsed.user_ids !== undefined || parsed.is_mentioning_everyone !== undefined
      ? { mentions: { userIds: parsed.user_ids ?? [], everyone: parsed.is_mentioning_everyone === true } }
      : {}),
    ...(parsed.payload.attachments
      ? {
          attachments: parsed.payload.attachments.map((a) => ({ filepath: a.filepath, name: a.name ?? basenameOf(a.filepath) })),
        }
      : {}),
  };
  // Validates exactly like a fresh send; the stored bytes are what is sent.
  prepareTurn(request);
  return { request, bodyJson };
}

function basenameOf(filepath: string): string {
  const slash = filepath.lastIndexOf('/');
  return slash >= 0 ? filepath.slice(slash + 1) : filepath;
}

const TurnStarted = z.looseObject({
  execution_id: z.string().min(1).max(512),
  events_url: z.string().min(1).max(2048),
  response_message_id: z.string().max(512).nullish(),
  task_id: z.string().max(512).nullish(),
  command_id: z.string().max(512).nullish(),
  created: z.boolean().nullish(),
});

const TurnStartErrorBody = z.looseObject({
  error: z.string().max(1000).nullish(),
  message: z.string().max(2000).nullish(),
  retryable: z.boolean().nullish(),
});

export interface StartedTurn {
  executionId: string;
  /** Validated same-origin API path of the execution's event stream. */
  eventsPath: string;
  responseMessageId: string | null;
  created: boolean;
}

/** Why a send failed, for the retry policy and the UI. */
export type TurnStartFailure =
  /** No answer (`status` absent), or 5xx/429 with the HTTP status. */
  | { kind: 'transient'; retryAfterMs?: number; status?: number }
  /** 409 for this question_id with a different body: a client bug. */
  | { kind: 'idempotency_conflict' }
  | { kind: 'unsupported_agent' }
  /** A continuation for a pause that was answered elsewhere (409 `*_already_resolved`). */
  | { kind: 'already_resolved' }
  /** Another 409 with the server's own message and retry hint. */
  | { kind: 'conflict'; message: string | null; retryable: boolean; retryAfterMs?: number }
  | { kind: 'rejected'; status: number; message: string | null }
  /** The server answered 200 with something we will not follow. */
  | { kind: 'bad_response' };

export class TurnStartError extends Error {
  constructor(readonly failure: TurnStartFailure, readonly cause?: unknown) {
    super(`turn start failed: ${failure.kind}`);
    this.name = 'TurnStartError';
  }
}

/**
 * POSTs the prepared body. Resolves with the validated execution, or throws
 * `TurnStartError` (send-specific outcomes) or `ApiError` (session-level
 * failures already handled by the guarded fetch: 401, 426, origin).
 */
export async function startTurn(
  prepared: PreparedTurn,
  options: { client?: ApiClient; signal?: AbortSignal } = {},
): Promise<StartedTurn> {
  const client = options.client ?? sessionApiClient();
  const { request, bodyJson } = prepared;
  let result;
  try {
    result = await client.POST('/elitea_core/messages/prompt_lib/{project_id}/{conversation_id}', {
      params: {
        path: { project_id: String(request.projectId), conversation_id: request.conversationUuid },
        query: { execution_contract: executionContractOf(request) },
      },
      body: JSON.parse(bodyJson) as ChatSendRequest,
      bodySerializer: () => bodyJson,
      headers: { 'Content-Type': 'application/json' },
      signal: options.signal,
    });
  } catch (error) {
    if (error instanceof ApiError && (error.kind === 'network' || error.kind === 'timeout')) {
      throw new TurnStartError({ kind: 'transient' }, error);
    }
    throw error;
  }

  return readStarted(result);
}

type PostResult = { data?: unknown; error?: unknown; response: Response };

/** A send's or a continuation's answer: the validated execution, or a TurnStartError. */
function readStarted({ data, error, response }: PostResult): StartedTurn {
  if (response.ok) {
    const parsed = TurnStarted.safeParse(data);
    if (!parsed.success || !isSameOriginApiPath(parsed.data.events_url)) {
      throw new TurnStartError({ kind: 'bad_response' });
    }
    return {
      executionId: parsed.data.execution_id,
      eventsPath: parsed.data.events_url,
      responseMessageId: parsed.data.response_message_id ?? null,
      created: parsed.data.created !== false,
    };
  }

  const body = TurnStartErrorBody.safeParse(error);
  const errorCode = body.success ? (body.data.error ?? null) : null;
  const message = body.success ? (body.data.message ?? null) : null;
  const retryAfterMs = parseRetryAfter(response.headers.get('Retry-After'));
  const status = response.status;

  if (status === 503 || status === 429 || (status >= 500 && status !== 501)) {
    throw new TurnStartError({ kind: 'transient', retryAfterMs, status });
  }
  if (status === 422 && errorCode === 'unsupported_agent_execution') {
    throw new TurnStartError({ kind: 'unsupported_agent' });
  }
  if (status === 409) {
    if (errorCode !== null && /_already_resolved$/.test(errorCode)) {
      throw new TurnStartError({ kind: 'already_resolved' });
    }
    if (errorCode !== null && /conflicts with an existing turn/i.test(errorCode)) {
      throw new TurnStartError({ kind: 'idempotency_conflict' });
    }
    throw new TurnStartError({
      kind: 'conflict',
      message,
      retryable: body.success && body.data.retryable === true,
      retryAfterMs,
    });
  }
  throw new TurnStartError({ kind: 'rejected', status, message });
}

export type ContinueContract =
  | 'agent.continue.hitl.v1'
  | 'agent.continue.authorization.v1'
  | 'agent.continue.output-limit.v1';

/**
 * POSTs a continuation (`continueChatExecution`): answers a pause and starts
 * the rest of the turn. Resolves with the execution to stream; throws
 * `TurnStartError` (`already_resolved` when someone answered it first) or
 * `ApiError`. Never retried automatically: a continuation is online-only and
 * answered again by the user.
 */
export async function continueTurn(
  contract: ContinueContract,
  body: components['schemas']['ChatContinueRequest'],
  options: { client?: ApiClient; signal?: AbortSignal } = {},
): Promise<StartedTurn> {
  const client = options.client ?? sessionApiClient();
  let result;
  try {
    result = await client.POST('/elitea_core/continue_predict/prompt_lib/{project_id}/{conversation_id}', {
      params: {
        path: { project_id: String(body.project_id), conversation_id: body.conversation_uuid },
        query: { execution_contract: contract },
      },
      body,
      signal: options.signal,
    });
  } catch (error) {
    if (error instanceof ApiError && (error.kind === 'network' || error.kind === 'timeout')) {
      throw new TurnStartError({ kind: 'transient' }, error);
    }
    throw error;
  }
  return readStarted(result);
}

export const REGENERATE_CONTRACT = 'agent.regenerate.v1';
export type ChatRegenerateRequest = components['schemas']['ChatRegenerateRequest'];

/** A plain retry of the newest answer (regenerateChatMessage). */
export interface RegenerationRequest {
  projectId: number;
  conversationUuid: string;
  /** The answer's author participant (the agent, or the model participant); 0 lets the server use the answer's. */
  participantId: number;
  /** The question's group uuid (the send's `question_id`). */
  questionUuid: string;
  /** The answer's group uuid (the send's `response_message_id`). */
  answerUuid: string;
  /**
   * The question's text. Required non-empty by the route; for a plain retry
   * the server answers the question as stored.
   */
  questionText: string;
  /** Set for a plain model conversation: the same `llm_settings` as the send. */
  model?: TurnModel | null;
  /** Client generated, lowercase: the idempotency key (a retry reuses it). */
  regenerationId: string;
}

const UUID_ANY = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** The body every attempt of one regeneration sends (byte-identical, like a send). */
export function prepareRegeneration(request: RegenerationRequest): { request: RegenerationRequest; bodyJson: string } {
  if (!QUESTION_ID_RE.test(request.regenerationId)) throw new TurnInputError('question_id');
  if (!Number.isSafeInteger(request.projectId) || request.projectId <= 0) throw new TurnInputError('invalid');
  if (!Number.isSafeInteger(request.participantId) || request.participantId < 0) throw new TurnInputError('invalid');
  for (const id of [request.conversationUuid, request.questionUuid, request.answerUuid]) {
    if (!UUID_ANY.test(id)) throw new TurnInputError('invalid');
  }
  const model = request.model ?? null;
  const questionText = request.questionText.trim() === '' ? '(regenerate)' : request.questionText;
  const body: ChatRegenerateRequest = {
    payload: {
      user_input: questionText,
      ...(model === null
        ? {}
        : {
            llm_settings: {
              model_name: model.name,
              ...(model.projectId !== null ? { model_project_id: model.projectId } : {}),
              stream: true,
            },
          }),
    },
    project_id: request.projectId,
    participant_id: request.participantId,
    conversation_uuid: request.conversationUuid,
    question_id: request.questionUuid,
    message_id: request.answerUuid,
    stream_id: request.answerUuid,
    regeneration_id: request.regenerationId,
  };
  const bodyJson = JSON.stringify(body);
  if (utf8Bytes(bodyJson) > MAX_TURN_BODY_BYTES) throw new TurnInputError('too_large');
  return { request, bodyJson };
}

/**
 * POSTs a regeneration (`regenerateChatMessage`). Answers like the send: the
 * execution to stream (the same `response_message_id`, rewritten in place).
 * 409 `agent_regeneration_pending` (`retryable: true`, `Retry-After: 1`)
 * while the previous answer is still being finalised: the caller retries
 * with the SAME body.
 */
export async function startRegeneration(
  prepared: { request: RegenerationRequest; bodyJson: string },
  options: { client?: ApiClient; signal?: AbortSignal } = {},
): Promise<StartedTurn> {
  const client = options.client ?? sessionApiClient();
  const { request, bodyJson } = prepared;
  let result;
  try {
    result = await client.POST('/elitea_core/regenerate/prompt_lib/{project_id}/{message_id}', {
      params: {
        path: { project_id: String(request.projectId), message_id: request.answerUuid },
        query: { execution_contract: REGENERATE_CONTRACT },
      },
      body: JSON.parse(bodyJson) as ChatRegenerateRequest,
      bodySerializer: () => bodyJson,
      headers: { 'Content-Type': 'application/json' },
      signal: options.signal,
    });
  } catch (error) {
    if (error instanceof ApiError && (error.kind === 'network' || error.kind === 'timeout')) {
      throw new TurnStartError({ kind: 'transient' }, error);
    }
    throw error;
  }
  return readStarted(result);
}
