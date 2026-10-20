import { randomUUID } from 'expo-crypto';

import type { ApiClient } from '@/api/client';
import { isApiError } from '@/api/errors';
import { resolveApiPath } from '@/api/paths';
import {
  QUESTION_ID_RE,
  TurnStartError,
  continueTurn,
  prepareRegeneration,
  prepareTurn,
  startRegeneration,
  startTurn,
  type RegenerationRequest,
  type ContinueContract,
  type PreparedTurn,
  type StartedTurn,
  type TurnRequest,
} from '@/api/queries/sendMessage';
import { stopTurn, type StopOutcome } from '@/api/queries/stopTurn';
import { backoffDelay, ResumableStream, type ResumableStreamOptions, type StreamStatus } from '@/api/sse/stream';
import type { StreamTransport } from '@/api/sse/transport';
import { projectScope } from '@/lib/abortRegistry';
import { log } from '@/lib/log';
import { parseFrame } from '@/features/chat/frames';
import { INITIAL_TURN, isTurnTerminal, reduceTurn, type TurnState } from '@/features/chat/turnReducer';
import { registerWipeHook } from '@/security/endSession';
import { getSession } from '@/state/session';

/**
 * One chat turn from send to settle (plan-app §4 "Send and stream"):
 *
 *  1. The body is built once with a client `question_id` (lowercase UUID)
 *     and kept in memory; every attempt re-sends the same bytes.
 *  2. Network errors, timeouts, 5xx and retryable 409s are re-POSTed with
 *     backoff (`Retry-After` wins). The server replays an identical send
 *     instead of starting a second execution. When the workspace is
 *     unreachable (no answer, or a proxy's 502/503/504) or the in-place
 *     retries run out, the send is handed to the outbox instead.
 *  3. The server's `events_url` must be a same-origin API path; it is then
 *     streamed with `ResumableStream` and folded by `turnReducer`.
 *  4. On the terminal frame the stream is closed (the server never closes
 *     it) and the persisted conversation is refetched.
 *  5. Reattach (screen remount, failed stream, user retry): re-POST the
 *     stored body, which returns the original execution, and replay its
 *     stream from the beginning.
 */

export const MAX_SEND_ATTEMPTS = 6;

export type TurnFailureSource = 'send' | 'stream' | 'turn';

export interface LiveTurn {
  questionId: string;
  userText: string;
  turn: TurnState;
  execution: StartedTurn | null;
  streamStatus: StreamStatus | null;
  /** The stream gave up reconnecting; the UI offers "Reconnect". */
  canReconnect: boolean;
  /** Where a failure happened, when `turn.phase === 'failed'`. */
  failure: TurnFailureSource | null;
  /** The persisted conversation was refetched after the turn ended. */
  settled: boolean;
  /**
   * The workspace did not answer the send, and the turn was handed to the
   * outbox (`TurnDeps.handOff`); this controller does no more network work.
   */
  queued: boolean;
  /** This turn continues a paused answer (no user message of its own). */
  continuation: boolean;
  /** The names of the files the message carries. */
  attachmentNames: string[];
  /**
   * A regeneration of a persisted answer: it is rewritten in place, so the
   * thread hides the old text while this turn is live, and after it settles
   * checks that the answer's `generation` is this `regenerationId`.
   */
  regeneration: { answerUuid: string; regenerationId: string } | null;
}

/**
 * The persisted answer a live regeneration rewrites, which the thread hides
 * while the new one streams; null when nothing should be hidden. A
 * regeneration that failed while SENDING with nothing written (a refused
 * 400/409/422, the send attempts used up) never touched the answer on the
 * server, so the original shows again, next to the failed bubble, instead of
 * staying hidden until the user taps Dismiss.
 */
export function answerHiddenByRegeneration(live: LiveTurn | null): string | null {
  if (!live || live.settled || !live.regeneration) return null;
  if (live.turn.phase === 'failed' && live.failure === 'send' && live.turn.content === '') return null;
  return live.regeneration.answerUuid;
}

export interface TurnDeps {
  client?: ApiClient;
  transport?: StreamTransport;
  appState?: ResumableStreamOptions['appState'];
  /** Resolves after `ms`, or early when `signal` aborts. */
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
  random?: () => number;
  /** Called once the turn ended (refetch the conversation). */
  onSettle?: () => Promise<void> | void;
  /** Frames were lost (replay reset): refill from REST. */
  onRefreshNeeded?: () => void;
  maxSendAttempts?: number;
  /**
   * The workspace cannot be reached even though the device may be online: a
   * send attempt got no answer at all (network error, request deadline), or a
   * proxy in front of it answered 502/503/504, or the in-place retries ran out
   * on a transient failure. Store the prepared turn (same question_id, same
   * bytes) in the outbox, not before `retryAfterMs` when the server said so,
   * and resolve true; the controller then stops. Resolve false to keep
   * retrying in place (e.g. offline storage is off).
   */
  handOff?: (prepared: PreparedTurn, hint: HandOffHint) => Promise<boolean>;
  /** Starts the execution instead of a chat send (a continuation). */
  start?: (signal: AbortSignal) => Promise<StartedTurn>;
  /** Set by `TurnController.regeneration`. */
  regeneration?: { answerUuid: string; regenerationId: string };
  /** The server stop (client contract 1.3); injectable for tests. */
  stopOnServer?: (projectId: number, responseMessageId: string) => Promise<StopOutcome>;
  /** The pause was answered elsewhere (409 `*_already_resolved`). */
  onAlreadyResolved?: () => void;
}

export interface HandOffHint {
  /** The server's `Retry-After`, when it sent one. */
  retryAfterMs?: number;
}

export const FAILURE_TEXT = {
  unreachable: "Couldn't reach the workspace. Check your connection and try again.",
  unsupported: 'This agent needs the web app.',
  conflict: 'This message could not be sent. Start a new message.',
  rejected: 'The workspace refused this message.',
  badResponse: 'The workspace sent a reply this app cannot follow.',
  stream: 'Lost the connection to the reply.',
  signedOut: 'You were signed out.',
  answeredElsewhere: 'Answered elsewhere.',
} as const;

function defaultSleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const timer = setTimeout(done, ms);
    function done() {
      clearTimeout(timer);
      signal.removeEventListener('abort', done);
      resolve();
    }
    signal.addEventListener('abort', done);
  });
}

/** Answers from a gateway in front of a workspace that is down (DEF-R3). */
const GATEWAY_STATUSES: ReadonlySet<number> = new Set([502, 503, 504]);

/**
 * The workspace itself cannot be reached: the send got no HTTP answer at all
 * (a network error or the request deadline, DEF-5), or a reachable proxy
 * answered 502/503/504 for it (DEF-R3). Either way retrying in place for a
 * few seconds rarely helps; the outbox retries with backoff until it is back.
 */
function isUnreachable(error: unknown): boolean {
  if (!(error instanceof TurnStartError) || error.failure.kind !== 'transient') return false;
  if (error.failure.status !== undefined) return GATEWAY_STATUSES.has(error.failure.status);
  const cause = error.cause;
  return isApiError(cause) && (cause.kind === 'network' || cause.kind === 'timeout');
}

function retryAfterOf(error: unknown): number | undefined {
  return error instanceof TurnStartError && (error.failure.kind === 'transient' || error.failure.kind === 'conflict')
    ? error.failure.retryAfterMs
    : undefined;
}

/** A fresh lowercase `question_id`. */
export function newQuestionId(uuid: () => string = randomUUID): string {
  const id = uuid().toLowerCase();
  if (!QUESTION_ID_RE.test(id)) throw new Error('generated question_id is not a canonical UUID');
  return id;
}

export class TurnController {
  private state: LiveTurn;
  private readonly listeners = new Set<() => void>();
  private stream: ResumableStream | null = null;
  private sendAbort: AbortController | null = null;
  private generation = 0;
  /**
   * A continuation's execution once admitted. Re-attaching replays its stream
   * instead of POSTing again: unlike a send (deduplicated by question_id), a
   * second continuation of the same pause is refused as already resolved.
   */
  private admitted: StartedTurn | null = null;
  /**
   * A continuation's POST while it is in flight. Detaching never aborts it
   * (the server may already have admitted it), and a re-attach waits for the
   * same request instead of POSTing the answer a second time.
   */
  private continuationPost: Promise<StartedTurn> | null = null;

  constructor(
    private readonly prepared: PreparedTurn,
    private readonly deps: TurnDeps = {},
  ) {
    this.state = {
      questionId: prepared.request.questionId,
      userText: prepared.request.userInput,
      turn: INITIAL_TURN,
      execution: null,
      streamStatus: null,
      canReconnect: false,
      failure: null,
      settled: false,
      queued: false,
      continuation: deps.start !== undefined,
      attachmentNames: (prepared.request.attachments ?? []).map((a) => a.name),
      regeneration: deps.regeneration ?? null,
    };
  }

  /**
   * A continuation of a paused answer: POSTs `body` once (online only, never
   * to the outbox) and streams the rest of the turn like a send.
   */
  static continuation(
    target: { projectId: number; conversationUuid: string },
    contract: ContinueContract,
    body: Parameters<typeof continueTurn>[1],
    deps: Omit<TurnDeps, 'start' | 'handOff' | 'maxSendAttempts'> = {},
  ): TurnController {
    const prepared: PreparedTurn = {
      request: {
        projectId: target.projectId,
        conversationUuid: target.conversationUuid,
        participantId: 0,
        questionId: newQuestionId(),
        userInput: '',
      },
      bodyJson: JSON.stringify(body),
    };
    return new TurnController(prepared, {
      ...deps,
      maxSendAttempts: 1,
      start: (signal) => continueTurn(contract, body, { client: deps.client, signal }),
    });
  }

  /**
   * Regenerates the newest answer (regenerateChatMessage): one body, sent
   * again unchanged on a retry (the `regeneration_id` makes it idempotent;
   * 409 `agent_regeneration_pending` is retried after its Retry-After), then
   * the answer's stream like a send. Never queued: online only.
   */
  static regeneration(
    request: Omit<RegenerationRequest, 'regenerationId'> & { regenerationId?: string },
    deps: Omit<TurnDeps, 'start' | 'handOff' | 'regeneration'> = {},
  ): TurnController {
    const regenerationId = request.regenerationId ?? newQuestionId();
    const prepared = prepareRegeneration({ ...request, regenerationId });
    const turn: PreparedTurn = {
      request: {
        projectId: request.projectId,
        conversationUuid: request.conversationUuid,
        participantId: request.participantId,
        questionId: regenerationId,
        userInput: '',
        model: request.model ?? null,
      },
      bodyJson: prepared.bodyJson,
    };
    return new TurnController(turn, {
      ...deps,
      regeneration: { answerUuid: request.answerUuid, regenerationId },
      start: (signal) => startRegeneration(prepared, { client: deps.client, signal }),
    });
  }

  static create(request: Omit<TurnRequest, 'questionId'> & { questionId?: string }, deps?: TurnDeps): TurnController {
    const questionId = request.questionId ?? newQuestionId();
    return new TurnController(prepareTurn({ ...request, questionId }), deps);
  }

  get request(): TurnRequest {
    return this.prepared.request;
  }

  /** The exact body every attempt sends. */
  get bodyJson(): string {
    return this.prepared.bodyJson;
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = (): LiveTurn => this.state;

  private update(patch: Partial<LiveTurn>) {
    this.state = { ...this.state, ...patch };
    for (const listener of [...this.listeners]) listener();
  }

  /** True while the turn still needs the network (not settled, not failed, not queued). */
  get isActive(): boolean {
    if (this.state.queued) return false;
    const phase = this.state.turn.phase;
    return phase === 'sending' || phase === 'streaming';
  }

  /** Sends (first time) or re-sends the stored body and streams the execution. */
  start(): Promise<void> {
    this.stopNetwork();
    const generation = ++this.generation;
    this.update({
      turn: { ...INITIAL_TURN },
      execution: null,
      streamStatus: null,
      canReconnect: false,
      failure: null,
      settled: false,
      queued: false,
    });
    return this.run(generation);
  }

  /** Re-attach after a remount or a failure: same body, same question_id. */
  reattach(): Promise<void> {
    return this.start();
  }

  /**
   * Stops the run on the server (client contract 1.3). Possible once the
   * send is admitted (the answer's `response_message_id` is known) and while
   * the turn streams. After a 204 the stream is closed and the transcript
   * read again (partial output is kept); the turn settles as stopped.
   * 'not_running': the answer had already ended; its own stream settles it.
   * Throws StopError when the stop could not be recorded.
   */
  async stop(): Promise<StopOutcome | 'not_ready'> {
    const responseMessageId = this.state.execution?.responseMessageId ?? null;
    if (!this.isActive || this.state.turn.phase !== 'streaming' || responseMessageId === null) return 'not_ready';
    const stopOnServer = this.deps.stopOnServer ?? ((projectId: number, id: string) => stopTurn(projectId, id, this.deps.client));
    const outcome = await stopOnServer(this.prepared.request.projectId, responseMessageId);
    if (outcome !== 'stopped') return outcome;
    // The stream would still deliver the CANCELLED frame; it is not needed.
    const generation = ++this.generation;
    this.stopNetwork();
    this.update({ turn: { ...this.state.turn, phase: 'done', status: null, error: null, stopped: true }, canReconnect: false });
    try {
      await this.deps.onSettle?.();
    } catch (error) {
      log.warn('refetch after the stop failed', { error });
    }
    if (generation === this.generation) this.update({ settled: true });
    return 'stopped';
  }

  /** After the stream gave up: try again now. */
  reconnect(): void {
    this.stream?.reconnect();
  }

  /** Stops network work (screen gone). The turn can be re-attached later. */
  detach(): void {
    this.generation++;
    this.stopNetwork();
  }

  private stopNetwork() {
    this.sendAbort?.abort(new Error('turn_detached'));
    this.sendAbort = null;
    this.stream?.close();
    this.stream = null;
  }

  private fail(source: TurnFailureSource, message: string, retryable: boolean) {
    this.update({
      failure: source,
      turn: { ...this.state.turn, phase: 'failed', status: null, error: { message, retryable } },
    });
  }

  private async run(generation: number): Promise<void> {
    const execution = this.state.continuation && this.admitted !== null ? this.admitted : await this.send(generation);
    if (execution === null || generation !== this.generation) return;
    if (this.state.continuation) this.admitted = execution;
    this.update({ execution });
    await this.streamExecution(execution, generation);
  }

  private async send(generation: number): Promise<StartedTurn | null> {
    const max = this.deps.maxSendAttempts ?? MAX_SEND_ATTEMPTS;
    const sleep = this.deps.sleep ?? defaultSleep;
    const projectId = this.prepared.request.projectId;
    for (let attempt = 0; ; attempt++) {
      if (generation !== this.generation) return null;
      const abort = new AbortController();
      this.sendAbort = abort;
      try {
        return this.deps.start
          ? await this.postContinuation(this.deps.start)
          : await startTurn(this.prepared, { client: this.deps.client, signal: abort.signal });
      } catch (error) {
        if (generation !== this.generation) return null;
        if (isUnreachable(error) && (await this.tryHandOff(generation, error))) return null;
        if (generation !== this.generation) return null;
        let retryAfterMs: number | undefined;
        if (error instanceof TurnStartError) {
          const f = error.failure;
          switch (f.kind) {
            case 'transient':
              retryAfterMs = f.retryAfterMs;
              break;
            case 'conflict':
              if (!f.retryable) {
                this.fail('send', f.message ?? FAILURE_TEXT.rejected, false);
                return null;
              }
              retryAfterMs = f.retryAfterMs;
              break;
            case 'idempotency_conflict':
              // We never change the body for a question_id, so this is a bug.
              log.error('send rejected as an idempotency conflict', { projectId });
              this.fail('send', FAILURE_TEXT.conflict, false);
              return null;
            case 'unsupported_agent':
              this.fail('send', FAILURE_TEXT.unsupported, false);
              return null;
            case 'already_resolved':
              this.fail('send', FAILURE_TEXT.answeredElsewhere, false);
              this.deps.onAlreadyResolved?.();
              return null;
            case 'rejected':
              this.fail('send', f.message ?? FAILURE_TEXT.rejected, false);
              return null;
            case 'bad_response':
              this.fail('send', FAILURE_TEXT.badResponse, false);
              return null;
          }
        } else if (isApiError(error)) {
          // 401/426/origin/sign-out: the session layer already reacted.
          this.fail('send', FAILURE_TEXT.signedOut, false);
          return null;
        } else {
          log.warn('unexpected send failure', { error });
        }
        if (attempt + 1 >= max) {
          // Still failing after every in-place retry (500, 429, a retryable
          // 409): the outbox keeps trying, with the same question_id and bytes.
          if (await this.tryHandOff(generation, error)) return null;
          if (generation !== this.generation) return null;
          this.fail('send', FAILURE_TEXT.unreachable, true);
          return null;
        }
        const delay = retryAfterMs ?? Math.max(250, backoffDelay(attempt + 1, this.deps.random));
        await sleep(delay, abort.signal);
      } finally {
        if (this.sendAbort === abort) this.sendAbort = null;
      }
    }
  }

  /**
   * POSTs a continuation once and records its admission whichever attachment
   * is current when the answer arrives. Only a POST that failed is sent again.
   */
  private postContinuation(start: NonNullable<TurnDeps['start']>): Promise<StartedTurn> {
    if (this.continuationPost === null) {
      const post = start(new AbortController().signal).then((execution) => {
        this.admitted = execution;
        return execution;
      });
      this.continuationPost = post;
      post.then(
        () => {},
        () => {
          if (this.continuationPost === post) this.continuationPost = null;
        },
      );
    }
    return this.continuationPost;
  }

  /** Gives the turn to the outbox; true when it is stored there and this turn stops. */
  private async tryHandOff(generation: number, error: unknown): Promise<boolean> {
    if (!this.deps.handOff) return false;
    let queued = false;
    try {
      queued = (await this.deps.handOff(this.prepared, { retryAfterMs: retryAfterOf(error) })) === true;
    } catch (error) {
      log.warn('could not hand the send to the outbox', { error });
    }
    if (!queued) return false;
    if (generation === this.generation) this.update({ queued: true });
    return true;
  }

  private async streamExecution(execution: StartedTurn, generation: number): Promise<void> {
    const origin = getSession().serverOrigin;
    const url = origin === null ? null : resolveApiPath(origin, execution.eventsPath);
    if (url === null) {
      this.fail('send', FAILURE_TEXT.badResponse, false);
      return;
    }
    this.update({ turn: { ...this.state.turn, phase: 'streaming' } });

    let ended = false;
    const stream = new ResumableStream({
      url,
      scope: projectScope(this.prepared.request.projectId),
      transport: this.deps.transport,
      appState: this.deps.appState,
      random: this.deps.random,
      onEvent: (event) => {
        if (generation !== this.generation) return 'stop';
        const frame = parseFrame(event);
        const before = this.state.turn;
        const turn = reduceTurn(before, frame);
        if (turn !== before) this.update({ turn });
        if (turn.refreshRequests > before.refreshRequests) this.deps.onRefreshNeeded?.();
        if (isTurnTerminal(frame)) {
          ended = true;
          return 'stop';
        }
        return undefined;
      },
      onStatus: (info) => {
        if (generation !== this.generation) return;
        this.update({ streamStatus: info.status, canReconnect: info.status === 'gave_up' });
        if (info.status === 'closed' && info.error && !ended) {
          this.fail('stream', info.error.kind === 'session_invalid' ? FAILURE_TEXT.signedOut : FAILURE_TEXT.stream, info.error.kind !== 'session_invalid');
        }
      },
    });
    this.stream = stream;
    await stream.start();
    if (this.stream === stream) this.stream = null;
    if (!ended || generation !== this.generation) return;

    try {
      await this.deps.onSettle?.();
    } catch (error) {
      log.warn('refetch after the turn failed', { error });
    }
    if (generation !== this.generation) return;
    const phase = this.state.turn.phase;
    if (phase === 'done' || phase === 'paused') this.update({ settled: true });
  }
}

/**
 * Live turns by conversation, in memory only (the B4 outbox persists them).
 * A remounted thread finds its unfinished turn here and re-attaches.
 */
const liveTurns = new Map<string, TurnController>();
const registryListeners = new Set<() => void>();

function notifyRegistry() {
  for (const listener of [...registryListeners]) listener();
}

export function turnKey(projectId: number, conversationUuid: string): string {
  return `${projectId}:${conversationUuid}`;
}

export function subscribeLiveTurns(listener: () => void): () => void {
  registryListeners.add(listener);
  return () => {
    registryListeners.delete(listener);
  };
}

/** Every live turn with its key (the widget counts and the Live Activity read these). */
export function listLiveTurns(): [string, TurnController][] {
  return [...liveTurns.entries()];
}

export function getLiveTurn(key: string): TurnController | undefined {
  return liveTurns.get(key);
}

export function setLiveTurn(key: string, controller: TurnController | null): void {
  const previous = liveTurns.get(key);
  if (previous === (controller ?? undefined)) return;
  if (previous) previous.detach();
  if (controller) liveTurns.set(key, controller);
  else liveTurns.delete(key);
  notifyRegistry();
}

export function clearLiveTurns(): void {
  for (const controller of liveTurns.values()) controller.detach();
  liveTurns.clear();
  notifyRegistry();
}

registerWipeHook('live_turns', clearLiveTurns);
