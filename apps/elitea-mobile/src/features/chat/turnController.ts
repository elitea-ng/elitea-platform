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
import { editSteering, handOffSteeringWithRetry, listSteering, newSteeringKey, queueSteeringWithRetry, removeSteering } from '@/api/queries/steering';
import { stopTurn, type StopOutcome } from '@/api/queries/stopTurn';
import { backoffDelay, ResumableStream, type ResumableStreamOptions, type StreamStatus } from '@/api/sse/stream';
import type { StreamTransport } from '@/api/sse/transport';
import { projectScope } from '@/lib/abortRegistry';
import { log } from '@/lib/log';
import { parseFrame } from '@/features/chat/frames';
import { INITIAL_TURN, isTurnTerminal, reduceTurn, withSteering, type TurnState } from '@/features/chat/turnReducer';
import { STEERING_ERROR, STEERING_STATE, joinSteeringTexts, leftoverSteering, type SteeringItem } from '@elitea/client-core/stream';
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
  /**
   * The steering messages that THIS device queued against the answer
   * (client contract 1.26). Only these are sent again as the next message
   * when the turn ends before the worker injects them.
   */
  ownSteering?: string[];
  /** The server refused steering for this turn (`steering_not_supported`, a pipeline). */
  steeringUnsupported?: boolean;
  /** A rate-limited steering message goes again at this time (epoch ms); null when none waits. */
  steeringRetryAt?: number | null;
}

/** What a message sent while the answer runs came to (client contract 1.26). */
export type SteerOutcome =
  /** Queued against the running answer; the worker reads it at the next safe point. */
  | 'queued'
  /** The turn ended first; the text goes as the next message once it settles. */
  | 'after_turn'
  /** This turn cannot take messages while it runs (a pipeline). The text is not sent. */
  | 'not_supported'
  /** The turn already has the most waiting messages. The text is not sent. */
  | 'queue_full'
  /** The server refused too many messages in a short time, also after the retries. The text is not sent. */
  | 'rate_limited'
  /** The answer is not admitted yet, or the request failed. The text is not sent. */
  | 'failed';

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
  /**
   * The turn ended with steering messages of this device that it did not
   * read (client contract 1.26), and the hand-off returned them (or a text
   * came too late for the turn): send `text` (the texts joined with a blank
   * line) as ONE normal next message.
   */
  onLeftover?: (text: string) => void;
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
/** The waits before the steering list is read again while own messages still show `pending` at the turn end. */
const SETTLE_RETRY_MS = [1_500, 4_000] as const;

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
      ownSteering: [],
      steeringUnsupported: false,
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
    this.steeringSettled = false;
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
    void this.settleSteering();
    return 'stopped';
  }

  /**
   * The answer streams and is admitted, so a message can steer it now
   * (client contract 1.26), unless the server refused steering for this turn.
   */
  get canSteer(): boolean {
    return (
      this.isActive &&
      this.state.turn.phase === 'streaming' &&
      (this.state.execution?.responseMessageId ?? null) !== null &&
      this.state.steeringUnsupported !== true
    );
  }

  /**
   * Sends `text` as a steering message of the running answer. A network
   * failure is tried once more with the SAME `Idempotency-Key` (the server
   * then answers the first message again). A `rate_limited` refusal is tried
   * again after its wait, with the same key; `steeringRetryAt` shows the
   * wait meanwhile.
   */
  async steer(text: string): Promise<SteerOutcome> {
    const responseMessageId = this.state.execution?.responseMessageId ?? null;
    if (!this.canSteer || responseMessageId === null) return 'failed';
    const projectId = this.prepared.request.projectId;
    const key = newSteeringKey();
    const sleep = this.deps.sleep ?? defaultSleep;
    const result = await queueSteeringWithRetry(projectId, responseMessageId, text, key, {
      client: this.deps.client,
      sleep: (ms) => sleep(ms, new AbortController().signal),
      onWait: (ms) => this.update({ steeringRetryAt: Date.now() + ms }),
    });
    if (this.state.steeringRetryAt != null) this.update({ steeringRetryAt: null });
    if (result.ok) {
      const item = result.item;
      if (item) this.update({ ownSteering: [...(this.state.ownSteering ?? []), item.id], turn: withSteering(this.state.turn, [item]) });
      // The turn settled while the POST was on its way: the end-of-turn rule must still see this message.
      if (item && this.steeringSettled) void this.settleSteering(true);
      return 'queued';
    }
    switch (result.code) {
      case STEERING_ERROR.turnNotRunning:
        this.queueAfterTurn(text);
        return 'after_turn';
      case STEERING_ERROR.notSupported:
        this.update({ steeringUnsupported: true });
        return 'not_supported';
      case STEERING_ERROR.queueFull:
        return 'queue_full';
      case STEERING_ERROR.rateLimited:
        return 'rate_limited';
      default:
        return 'failed';
    }
  }

  /** Changes the text of a pending steering message. Resolves to an error text, or null. */
  async editSteering(id: string, text: string): Promise<string | null> {
    const responseMessageId = this.state.execution?.responseMessageId ?? null;
    if (responseMessageId === null) return null;
    const result = await editSteering(this.prepared.request.projectId, responseMessageId, id, text, this.deps.client);
    if (result.ok) {
      if (result.item) this.update({ turn: withSteering(this.state.turn, [result.item]) });
      return null;
    }
    // `steering_not_pending`: the turn read it first; the list shows it as delivered.
    if (result.code === STEERING_ERROR.notPending) {
      await this.refreshSteering();
      return 'The agent already read this message.';
    }
    return "Couldn't change the message. Try again.";
  }

  /** Removes a pending steering message. Resolves to an error text, or null. */
  async removeSteering(id: string): Promise<string | null> {
    const responseMessageId = this.state.execution?.responseMessageId ?? null;
    const item = this.state.turn.steering.find((candidate) => candidate.id === id);
    if (responseMessageId === null || item === undefined) return null;
    const result = await removeSteering(this.prepared.request.projectId, responseMessageId, id, this.deps.client);
    if (result.ok) {
      this.update({
        turn: withSteering(this.state.turn, [{ ...item, state: STEERING_STATE.Removed }]),
        ownSteering: (this.state.ownSteering ?? []).filter((own) => own !== id),
      });
      return null;
    }
    if (result.code === STEERING_ERROR.notPending) {
      await this.refreshSteering();
      return 'The agent already read this message.';
    }
    return "Couldn't remove the message. Try again.";
  }

  /** Reads the answer's steering list again (after a lost race with the injection). */
  private async refreshSteering(): Promise<SteeringItem[]> {
    const responseMessageId = this.state.execution?.responseMessageId ?? null;
    if (responseMessageId === null) return this.state.turn.steering;
    try {
      const items = await listSteering(this.prepared.request.projectId, responseMessageId, this.deps.client);
      this.update({ turn: withSteering(this.state.turn, items) });
    } catch (error) {
      log.info('could not read the steering list', { error });
    }
    return this.state.turn.steering;
  }

  /** The texts that wait for the turn end (`turn_not_running`). */
  private readonly afterTurn: string[] = [];
  /** The end-of-turn rule ran (the turn is over). */
  private steeringSettled = false;

  /** Sends `text` as the next message once this turn is over (now, when it is over already). */
  private queueAfterTurn(text: string): void {
    if (this.steeringSettled || !this.isActive) {
      this.deps.onLeftover?.(text);
      return;
    }
    this.afterTurn.push(text);
  }

  /** The Idempotency-Key of the hand-off that is not done yet; a retry sends it again. */
  private handoffKey: string | null = null;

  /**
   * The end-of-turn rule (client contract 1.26). Reads the server list (it
   * reads again a little later while own messages still show `pending`: the
   * server marks them `turn_ended` a moment after the terminal frame). When
   * own messages are `turn_ended`, calls the hand-off, which takes them in
   * one step. ONLY the texts that the hand-off returns, plus the texts that
   * came too late (`turn_not_running`), become ONE next message, joined with
   * a blank line. An empty hand-off means that the server took them first
   * (it then starts the next turn itself): nothing of them is sent. Runs
   * after a detach too: the next message must not be lost when the thread
   * closes.
   */
  private async settleSteering(again = false): Promise<void> {
    if (this.steeringSettled && !again) return;
    this.steeringSettled = true;
    const own = new Set(this.state.ownSteering ?? []);
    const late = this.afterTurn.splice(0);
    const responseMessageId = this.state.execution?.responseMessageId ?? null;
    const items = own.size > 0 ? await this.settledSteeringList(own) : this.state.turn.steering;
    const leftovers = leftoverSteering(items, own, false);
    let handed: SteeringItem[] = [];
    if (responseMessageId !== null && (leftovers.length > 0 || this.handoffKey !== null)) {
      const key = (this.handoffKey ??= newSteeringKey());
      const sleep = this.deps.sleep ?? defaultSleep;
      try {
        handed = await handOffSteeringWithRetry(this.prepared.request.projectId, responseMessageId, key, {
          client: this.deps.client,
          sleep: (ms) => sleep(ms, new AbortController().signal),
        });
        // The answer is used: the next hand-off needs a new key.
        this.handoffKey = null;
        this.update({ turn: withSteering(this.state.turn, handed) });
      } catch (error) {
        // The key stays: the next settle sends it again and gets the same messages.
        log.info('could not hand off the steering messages', { error });
      }
    }
    const text = joinSteeringTexts([...handed, ...late.map((content) => ({ content }))]);
    this.update({ ownSteering: (this.state.ownSteering ?? []).filter((id) => !own.has(id)) });
    if (text !== '') this.deps.onLeftover?.(text);
  }

  /** The server list, read again after each wait of `SETTLE_RETRY_MS` while an own message is still `pending`. */
  private async settledSteeringList(own: ReadonlySet<string>): Promise<SteeringItem[]> {
    const sleep = this.deps.sleep ?? defaultSleep;
    let items = await this.refreshSteering();
    for (const wait of SETTLE_RETRY_MS) {
      if (!items.some((item) => own.has(item.id) && item.state === STEERING_STATE.Pending)) break;
      await sleep(wait, new AbortController().signal);
      items = await this.refreshSteering();
    }
    return items;
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
    void this.settleSteering();
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
