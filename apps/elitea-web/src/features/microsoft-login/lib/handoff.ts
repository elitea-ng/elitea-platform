/**
 * What the callback page tells the opener (ADR-0057 decision 6, S1c).
 *
 * The page posts `{type, state, handoff | error}` twice: with `postMessage` to
 * `window.opener`, targeted at the deployment origin, and on a same-origin
 * BroadcastChannel, because an identity page may sever `window.opener`. A message is
 * acted on only when it comes from this origin, has the right type, and carries the
 * `state` of the sign-in this window started: a message for another sign-in, or one a
 * different page posted, is ignored.
 */

/** The `type` of the callback page's message. */
export const MESSAGE_TYPE = 'elitea:microsoft-login';

export type HandoffOutcome =
  | { readonly kind: 'handoff'; readonly handoff: string }
  | { readonly kind: 'error'; readonly error: string };

/** The outcome a message carries for `state`, or `undefined` when it is not for this sign-in. */
export function outcomeOf(data: unknown, state: string): HandoffOutcome | undefined {
  if (typeof data !== 'object' || data === null) return undefined;
  const message = data as Readonly<Record<string, unknown>>;
  if (message['type'] !== MESSAGE_TYPE || message['state'] !== state) return undefined;
  const handoff = message['handoff'];
  if (typeof handoff === 'string' && handoff !== '') return { kind: 'handoff', handoff };
  const error = message['error'];
  if (typeof error === 'string' && error !== '') return { kind: 'error', error };
  return undefined;
}

export interface HandoffWait {
  /** Settles with the first outcome for `state`; rejects with an `expired` error when the sign-in's time is up and a `cancelled` one when it is aborted. */
  readonly outcome: Promise<HandoffOutcome>;
  /** Stops listening; the promise then stays pending. Safe to call twice. */
  readonly stop: () => void;
}

interface HandoffWaitParams {
  readonly state: string;
  readonly channelName: string;
  /** Milliseconds until the sign-in can no longer finish. */
  readonly timeoutMs: number;
  /** The popup this window opened. A `message` event must come from it while it is still reachable. */
  readonly popup: Window | null;
  /** Aborting stops the wait and rejects it with a `cancelled` error. */
  readonly signal?: AbortSignal;
}

function openChannel(name: string): BroadcastChannel | undefined {
  try {
    return new BroadcastChannel(name);
  } catch {
    // No BroadcastChannel (an old webview): postMessage alone then carries the handoff.
    return undefined;
  }
}

/** Listen for the callback page's message for `state`, on both routes. */
export function waitForHandoff({ state, channelName, timeoutMs, popup, signal }: HandoffWaitParams): HandoffWait {
  let release: () => void = () => undefined;
  const outcome = new Promise<HandoffOutcome>((resolve, reject) => {
    const channel = openChannel(channelName);
    const timer = window.setTimeout(() => {
      release();
      reject(new Error('expired'));
    }, timeoutMs);
    const settle = (data: unknown): void => {
      const found = outcomeOf(data, state);
      if (found === undefined) return;
      release();
      resolve(found);
    };
    const onMessage = (event: MessageEvent): void => {
      // Same origin only, and from the popup when this window still holds it.
      if (event.origin !== window.location.origin) return;
      if (popup !== null && event.source !== null && event.source !== popup) return;
      settle(event.data);
    };
    window.addEventListener('message', onMessage);
    signal?.addEventListener('abort', () => {
      release();
      reject(new Error('cancelled'));
    });
    if (channel !== undefined) {
      channel.onmessage = (event: MessageEvent) => {
        settle(event.data);
      };
    }
    release = () => {
      window.clearTimeout(timer);
      window.removeEventListener('message', onMessage);
      channel?.close();
    };
  });
  return {
    outcome,
    stop: () => {
      release();
    },
  };
}
