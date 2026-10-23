import type { StreamRequestInit, StreamResponse, StreamTransport } from '@/api/sse/transport';

/**
 * A scriptable stand-in for the expo/fetch stream transport. Each call opens a
 * `FakeConnection` the test drives: push bytes, end the body, or answer with a
 * status. Aborting the request's signal makes the body iterator throw, as the
 * real transport does.
 */
export interface FakeConnection {
  url: string;
  headers: Record<string, string>;
  signal: AbortSignal;
  push(text: string | Uint8Array): void;
  end(): void;
}

export interface ScriptedAnswer {
  status?: number;
  headers?: Record<string, string>;
  /** Body text for a non-200 answer. */
  body?: string;
  /** Throw instead of answering (network failure). */
  fail?: boolean;
  /** Final URL to report. */
  url?: string;
}

export function fakeStreamTransport(script: (index: number) => ScriptedAnswer = () => ({})) {
  const connections: FakeConnection[] = [];
  const transport: jest.MockedFunction<StreamTransport> = jest.fn(
    async (url: string, init: StreamRequestInit): Promise<StreamResponse> => {
      const index = connections.length;
      const items: Uint8Array[] = [];
      let ended = false;
      let failure: unknown = null;
      let notify: (() => void) | null = null;
      const poke = () => {
        const n = notify;
        notify = null;
        n?.();
      };
      init.signal.addEventListener('abort', () => {
        failure = init.signal.reason ?? new Error('aborted');
        poke();
      });
      const connection: FakeConnection = {
        url,
        headers: init.headers,
        signal: init.signal,
        push: (chunk) => {
          items.push(typeof chunk === 'string' ? new TextEncoder().encode(chunk) : chunk);
          poke();
        },
        end: () => {
          ended = true;
          poke();
        },
      };
      connections.push(connection);
      const answer = script(index);
      if (answer.fail) throw new TypeError('Network request failed');
      if (init.signal.aborted) throw init.signal.reason ?? new Error('aborted');
      const status = answer.status ?? 200;
      const headerMap = new Map(Object.entries(answer.headers ?? {}).map(([k, v]) => [k.toLowerCase(), v]));
      if (status !== 200) {
        const text = answer.body ?? '';
        if (text) connection.push(text);
        connection.end();
      }
      async function* body(): AsyncGenerator<Uint8Array> {
        while (true) {
          if (failure) throw failure;
          const next = items.shift();
          if (next) {
            yield next;
            continue;
          }
          if (ended) return;
          await new Promise<void>((resolve) => {
            notify = resolve;
          });
        }
      }
      return {
        status,
        headers: { get: (name: string) => headerMap.get(name.toLowerCase()) ?? null },
        url: answer.url,
        body: body(),
      };
    },
  );
  return { transport, connections };
}
