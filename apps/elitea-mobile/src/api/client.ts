import createClient, { type Client } from 'openapi-fetch';

import { ApiError } from '@/api/errors';
import type { paths } from '@/api/generated/elitea.gen';
import { createGuardedFetch, type AuthMode, type BaseFetch } from '@/api/middleware';
import { apiBaseUrl } from '@/api/paths';
import { getSession } from '@/state/session';

export type ApiClient = Client<paths>;
export type { paths } from '@/api/generated/elitea.gen';

export interface ApiClientOptions {
  origin: string;
  auth: AuthMode;
  /** Injectable transport (tests); defaults to the global fetch. */
  fetch?: BaseFetch;
  timeoutMs?: number;
}

/** A typed client for `origin + /api/v2`, with every call going through the guarded fetch. */
export function createApiClient(options: ApiClientOptions): ApiClient {
  return createClient<paths>({
    baseUrl: apiBaseUrl(options.origin),
    fetch: createGuardedFetch(options),
  });
}

let cached: { origin: string; client: ApiClient } | null = null;

/** The signed-in session's client (memoised per workspace origin). */
export function sessionApiClient(): ApiClient {
  const origin = getSession().serverOrigin;
  if (origin === null) {
    throw new ApiError('session_invalid', 'no workspace selected', { reason: 'no_credential' });
  }
  if (cached?.origin !== origin) {
    cached = { origin, client: createApiClient({ origin, auth: { kind: 'session' } }) };
  }
  return cached.client;
}
