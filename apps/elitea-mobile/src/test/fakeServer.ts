import { createApiClient, type ApiClient } from '@/api/client';
import { ApiError } from '@/api/errors';
import type { BaseFetch } from '@/api/middleware';

/**
 * A scripted workspace for API-level tests. Each route answers from a queue
 * (the last answer repeats); every request URL is recorded.
 */
export type Answer = { status: number; body?: unknown; headers?: Record<string, string> } | 'network_error';

export interface FakeServer {
  client: ApiClient;
  /** The transport, e.g. to install as `globalThis.fetch` for code that uses the session client. */
  fetch: BaseFetch;
  requests: { method: string; url: URL; body: string | null }[];
  /** Queues answers for requests whose path (without /api/v2) starts with `prefix`. */
  on(method: string, prefix: string, ...answers: Answer[]): void;
}

export const FAKE_ORIGIN = 'https://ai.example.com';

export function fakeServer(): FakeServer {
  const routes: { method: string; prefix: string; answers: Answer[] }[] = [];
  const requests: FakeServer['requests'] = [];
  const fetch: BaseFetch = async (input) => {
    const url = new URL(input.url);
    const body = input.method === 'GET' ? null : await input.clone().text();
    requests.push({ method: input.method, url, body });
    const path = url.pathname.replace(/^\/api\/v2/, '');
    const route = [...routes].reverse().find((r) => r.method === input.method && path.startsWith(r.prefix));
    if (!route) return new Response(JSON.stringify({ error: 'no route' }), { status: 404 });
    const answer = route.answers.length > 1 ? route.answers.shift()! : route.answers[0];
    if (answer === 'network_error') throw new TypeError('Network request failed');
    return new Response(answer.body === undefined ? null : JSON.stringify(answer.body), {
      status: answer.status,
      headers: { 'Content-Type': 'application/json', ...answer.headers },
    });
  };
  return {
    client: createApiClient({ origin: FAKE_ORIGIN, auth: { kind: 'session' }, fetch }),
    fetch,
    requests,
    on(method, prefix, ...answers) {
      routes.push({ method, prefix, answers });
    },
  };
}

export { ApiError };
