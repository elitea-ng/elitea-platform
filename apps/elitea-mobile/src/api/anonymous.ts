import { createGuardedFetch, type BaseFetch } from '@/api/middleware';

/**
 * Credential-free requests to a workspace origin: the discovery document,
 * the brand pack and the native token/revoke endpoints. They go through the
 * same guarded fetch as every REST call (origin guard, X-Client-Version,
 * deadline, abort registry, cross-origin redirect refusal, 426 handling), with
 * `Authorization` always stripped.
 */
export interface AnonymousRequestInit {
  method?: 'GET' | 'POST';
  headers?: Record<string, string>;
  body?: string;
  signal?: AbortSignal;
}

export interface AnonymousFetchOptions {
  fetch?: BaseFetch;
  timeoutMs?: number;
}

export function anonymousFetch(
  origin: string,
  url: string,
  init: AnonymousRequestInit = {},
  options: AnonymousFetchOptions = {},
): Promise<Response> {
  const guarded = createGuardedFetch({
    origin,
    auth: { kind: 'anonymous' },
    fetch: options.fetch,
    timeoutMs: options.timeoutMs,
  });
  return guarded(
    new Request(url, {
      method: init.method ?? 'GET',
      headers: init.headers,
      body: init.body,
      signal: init.signal,
    }),
  );
}

/** Reads a JSON body, or undefined when it is absent or not JSON. */
export async function readJson(response: Response): Promise<unknown> {
  try {
    return await response.json();
  } catch {
    return undefined;
  }
}
