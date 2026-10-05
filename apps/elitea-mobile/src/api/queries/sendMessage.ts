import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { ApiError, parseRetryAfter } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { isSameOriginApiPath } from '@/api/paths';

/**
 * Chat send: `startApplicationTurn` (overlay op until ADR-0025 WP5), with the
 * application contract for an agent and the adhoc contract for a plain model.
 *
 * The body is serialised ONCE and the same string is sent on every retry:
 * the server de-duplicates by `question_id` only for a byte-identical body
 * (same body → the original execution with `created:false`; a different body
 * with the same id → 409).
 */

export type ApplicationTurnRequest = components['schemas']['ApplicationTurnRequest'];

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
}

export interface PreparedTurn {
  request: TurnRequest;
  /** The exact bytes sent on every attempt. */
  bodyJson: string;
}

export class TurnInputError extends Error {
  constructor(readonly reason: 'question_id' | 'too_large' | 'empty' | 'invalid') {
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
  const body: ApplicationTurnRequest = {
    payload: { user_input: request.userInput },
    project_id: request.projectId,
    participant_id: request.participantId,
    conversation_uuid: request.conversationUuid,
    question_id: request.questionId,
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

const StoredBody = z.object({
  payload: z.object({ user_input: z.string() }),
  project_id: z.number(),
  participant_id: z.number(),
  conversation_uuid: z.string(),
  question_id: z.string(),
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
    participantId: parsed.participant_id,
    questionId: parsed.question_id,
    userInput: parsed.payload.user_input,
    model: parsed.llm_settings
      ? { name: parsed.llm_settings.model_name, projectId: parsed.llm_settings.model_project_id ?? null }
      : null,
  };
  // Validates exactly like a fresh send; the stored bytes are what is sent.
  prepareTurn(request);
  return { request, bodyJson };
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
    result = await client.POST('/elitea_core/messages/prompt_lib/{project_id}/{conversation_uuid}', {
      params: {
        path: { project_id: String(request.projectId), conversation_uuid: request.conversationUuid },
        query: { execution_contract: executionContractOf(request) },
      },
      body: JSON.parse(bodyJson) as ApplicationTurnRequest,
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

  const { data, error, response } = result;
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
