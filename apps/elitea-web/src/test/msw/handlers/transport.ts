/**
 * Transport-probe MSW handlers (unit F4). Mock rules (spec §6.5):
 *  - R-M2: a body is either a generated contract mock (`get<Op>ResponseMock()`,
 *    whose value is the operation's example in v2.yaml) or a typed constant
 *    in this file. The probe paths (`/__transport__/…`) exist on no real
 *    endpoint on purpose — the §5.4 behaviours under test are transport
 *    semantics, not endpoint contracts — so their bodies are constants here.
 *  - The one real route, GET /api/v2/social/author, answers the contract's
 *    own example (`getGetCurrentAuthorResponseMock`).
 *
 * Tests attach these per-scenario via `server.use(...)`; nothing registers
 * globally (stateful gates must not leak between tests, R-M5 covers strays).
 */
import { delay, http, HttpResponse } from 'msw';
import type { RequestHandler } from 'msw';

import type { ErrorResponse } from '../../../shared/api/generated/model';
import { getGetCurrentAuthorResponseMock } from '../../../shared/api/generated/social/social.msw';

export const TRANSPORT_PROBE_PATH = '/api/v2/__transport__/probe';
export const TRANSPORT_ECHO_PATH = '/api/v2/__transport__/echo';
export const AUTHOR_PATH = '/api/v2/social/author/';

/* Probe bodies. The error bodies use the flat apierr shape; http.ts keeps a
 * failure body as `unknown` and never parses it. */
const PROBE_OK: { message: string } = { message: 'transport probe ok' };
const ECHO_OK: { received: boolean } = { received: true };
const UNAUTHORIZED: ErrorResponse = { error: 'missing authorization header' };
const FORBIDDEN: ErrorResponse = { error: 'forbidden' };
const NOT_FOUND: ErrorResponse = { error: 'not found' };
/** Stand-in for the edge-auth login page a session-expiry redirect lands on. */
const LOGIN_PAGE_HTML = '<html><body>edge-auth login page</body></html>';

/** Request facts captured for assertions (headers/bytes, §5.4 probes). */
export interface CapturedRequest {
  url: string;
  method: string;
  traceparent: string | null;
  authorization: string | null;
  cacheControl: string | null;
  contentType: string | null;
  bodyText: string | null;
}

async function capture(request: Request, sink?: CapturedRequest[]): Promise<void> {
  if (sink === undefined) return;
  const bodyText = request.method === 'GET' || request.method === 'HEAD' ? null : await request.clone().text();
  sink.push({
    url: request.url,
    method: request.method,
    traceparent: request.headers.get('traceparent'),
    authorization: request.headers.get('authorization'),
    cacheControl: request.headers.get('cache-control'),
    contentType: request.headers.get('content-type'),
    bodyText,
  });
}

/** Mutable auth gate shared between a test and its gated handlers. */
export interface SessionGate {
  authed: boolean;
}

/** 200 probe; optionally captures request facts. */
export function probeOk(sink?: CapturedRequest[]): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(PROBE_OK);
  });
}

/** Any-method 200 probe — exercises the non-GET sugar methods. */
export function probeAny(sink?: CapturedRequest[]): RequestHandler {
  return http.all(`*${TRANSPORT_PROBE_PATH}`, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(PROBE_OK);
  });
}

/** JSON content-type over a non-JSON body — the lying-server branch (§3.6). */
export function probeLyingJson(): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, () =>
    new HttpResponse(LOGIN_PAGE_HTML, { headers: { 'Content-Type': 'application/json' } }),
  );
}

/** 204 No Content — empty-body branch. */
export function probeNoContent(): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, () => new HttpResponse(null, { status: 204 }));
}

/** Plain 404 — §3.6 Result-discipline probe. */
export function probeNotFound(): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, () => HttpResponse.json(NOT_FOUND, { status: 404 }));
}

/** Network-level failure (no response body, R-M2 n/a). */
export function probeNetworkError(): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, () => HttpResponse.error());
}

/** Never responds — abort-mid-flight probe (§5.4 behaviour 8). */
export function probeNeverending(): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, async () => {
    await delay('infinite');
    return HttpResponse.json(PROBE_OK);
  });
}

/** 401/403 until `gate.authed`, then the 200 probe body (§5.4 behaviour 2/3). */
export function probeAuthGated(gate: SessionGate, deniedStatus: 401 | 403 = 401, sink?: CapturedRequest[]): RequestHandler {
  const denied = deniedStatus === 401 ? UNAUTHORIZED : FORBIDDEN;
  return http.get(`*${TRANSPORT_PROBE_PATH}`, async ({ request }) => {
    await capture(request, sink);
    if (!gate.authed) return HttpResponse.json(denied, { status: deniedStatus });
    return HttpResponse.json(PROBE_OK);
  });
}

/** 401 on the first call, then a network-level failure — retry-crash probe. */
export function probeUnauthorizedThenNetworkError(): RequestHandler {
  let first = true;
  return http.get(`*${TRANSPORT_PROBE_PATH}`, () => {
    if (first) {
      first = false;
      return HttpResponse.json(UNAUTHORIZED, { status: 401 });
    }
    return HttpResponse.error();
  });
}

/** Redirects to `loginUrl` until `gate.authed` — the SECONDARY sniff signal. */
export function probeRedirectGated(gate: SessionGate, loginUrl: string): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, () => {
    if (!gate.authed) return HttpResponse.redirect(loginUrl, 302);
    return HttpResponse.json(PROBE_OK);
  });
}

/** Serves the synthetic login page at an arbitrary absolute-path pattern. */
export function loginPage(pathPattern: string): RequestHandler {
  return http.get(`*${pathPattern}`, () => HttpResponse.html(LOGIN_PAGE_HTML));
}

/** POST echo, 401-gated — the byte-identical-replay probe captures every attempt's body. */
export function echoAuthGated(gate: SessionGate, sink: CapturedRequest[]): RequestHandler {
  return http.post(`*${TRANSPORT_ECHO_PATH}`, async ({ request }) => {
    await capture(request, sink);
    if (!gate.authed) return HttpResponse.json(UNAUTHORIZED, { status: 401 });
    return HttpResponse.json(ECHO_OK);
  });
}

/** auth/me-class endpoint (§5.4 behaviour 5): the contract's example author when authed, a 401 otherwise. */
export function authorGated(gate: SessionGate, sink?: CapturedRequest[]): RequestHandler {
  return http.get(`*${AUTHOR_PATH}`, async ({ request }) => {
    await capture(request, sink);
    if (!gate.authed) return HttpResponse.json(UNAUTHORIZED, { status: 401 });
    return HttpResponse.json(getGetCurrentAuthorResponseMock());
  });
}
