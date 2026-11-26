/**
 * One send at a time, decided synchronously (#3077).
 *
 * A composer's "sending" React state changes a render later, so Enter pressed
 * three times in one tick reaches the send handler three times with the same
 * "not sending" state. The gate is a plain flag, checked and set before the
 * first `await`: while a submit is in flight, a second one answers
 * `undefined` at once and runs nothing. The input itself stays open (the
 * person can keep typing); only a second submit is ignored.
 *
 * `SubmitIds` is the other half: a client id per message, kept while the
 * message has not gone (a refused or failed send is retried with the same
 * id) and renewed once it has. The id is what the receivers make their
 * create and start calls idempotent on, so even a duplicate that gets past
 * a gate (a second window, a double IPC call) lands on the same thread.
 */

/** A synchronous in-flight guard for one composer. */
export interface SubmitGate {
  /** Runs `submit` unless one is in flight; `undefined` (nothing ran) when one is. */
  run: <T>(submit: () => Promise<T>) => Promise<T | undefined>;
  /** A submit is in flight now. */
  readonly pending: boolean;
}

export function createSubmitGate(): SubmitGate {
  let pending = false;
  return {
    run: async <T>(submit: () => Promise<T>): Promise<T | undefined> => {
      if (pending) return undefined;
      pending = true;
      try {
        return await submit();
      } finally {
        pending = false;
      }
    },
    get pending() {
      return pending;
    },
  };
}

/** The client id of the message being sent: the same until it is sent (`sent`), then a new one. */
export interface SubmitIds {
  /** The current message's id (made on first use). */
  current: () => string;
  /** The message was sent: the next one gets a new id. */
  sent: () => void;
}

/** `make`: a lowercase canonical UUID (the default, `crypto.randomUUID`); tests pass their own. */
export function createSubmitIds(make: () => string = () => globalThis.crypto.randomUUID()): SubmitIds {
  let id: string | undefined;
  return {
    current: () => {
      id ??= make();
      return id;
    },
    sent: () => {
      id = undefined;
    },
  };
}
