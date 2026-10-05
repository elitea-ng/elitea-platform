import { fetch as expoFetch } from 'expo/fetch';

import type { BaseFetch } from '@/api/middleware';

/**
 * A transport that refuses every redirect (SEC-2), for the credential-bearing
 * anonymous POSTs: the native token exchange (`code` + `code_verifier`), the
 * refresh (`refresh_token`) and the revoke (`token`).
 *
 * The global fetch (React Native's networking module) follows a 307/308
 * natively and re-sends the request body to the new location; the guarded
 * fetch's cross-origin check only sees the final URL afterwards, by which time
 * the refresh token has already left. `expo/fetch` with `redirect: 'error'`
 * fails the request at the first 3xx instead (no body is re-sent), and the
 * caller sees a failed request.
 *
 * The `Request` is unpacked here (URL, method, headers, text body) because
 * `expo/fetch` cannot read the Request objects other code builds (plan F11).
 */
export const noRedirectFetch: BaseFetch = async (input, init) => {
  const headers: Record<string, string> = {};
  new Headers(init?.headers ?? input.headers).forEach((value, key) => {
    headers[key] = value;
  });
  const method = input.method;
  const body = method === 'GET' || method === 'HEAD' ? undefined : await input.text();
  const response = await expoFetch(input.url, {
    method,
    headers,
    body,
    signal: init?.signal ?? undefined,
    redirect: 'error',
  });
  return response as unknown as Response;
};
