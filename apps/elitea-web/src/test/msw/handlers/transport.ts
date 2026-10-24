/**
 * Transport-probe MSW handlers (unit F4). Mock-rule compliance (spec §6.5):
 *  - R-M2: this file is the handlers' home; every response body derives from
 *    a fixture FILE under ../fixtures/transport/ — no inline body literals.
 *  - R-M3: every factory routes its entry through F2's
 *    `registerValidatedHandlers`, so the fixture body is zod-validated at
 *    registration time (the factory call inside each test).
 *  - fixtures carry `recordedAt`; they are `synthetic: true` because
 *    the probed paths (`/__transport__/…`) intentionally exist on no real
 *    endpoint — the §5.4 behaviours under test are transport semantics, not
 *    endpoint contracts.
 *
 * Tests attach these per-scenario via `server.use(...)`; nothing registers
 * globally (stateful gates must not leak between tests, R-M5 covers strays).
 */
import { delay, http, HttpResponse } from 'msw';
import type { RequestHandler } from 'msw';
import { z } from 'zod';
import type { ZodType } from 'zod';

import { registerValidatedHandlers } from '../register';
import author200 from '../fixtures/transport/author.200.json';
import echo200 from '../fixtures/transport/echo.200.json';
import forbidden403 from '../fixtures/transport/forbidden.403.json';
import loginPage200 from '../fixtures/transport/login-page.200.json';
import notFound404 from '../fixtures/transport/not-found.404.json';
import probe200 from '../fixtures/transport/probe.200.json';
import unauthorized401 from '../fixtures/transport/unauthorized.401.json';

export const TRANSPORT_PROBE_PATH = '/api/v2/__transport__/probe';
export const TRANSPORT_ECHO_PATH = '/api/v2/__transport__/echo';
export const AUTHOR_PATH = '/api/v2/social/author/';

/* Colocated minimal schemas (R-M3). */
const probeBody = z.object({ message: z.string() });
const errorBody = z.object({ error: z.string() });
// Mirrors the Go wire struct AuthorResponse (social/handler.go:51-58): every
// field is a string with no omitempty EXCEPT personalization (`any`,
// omitempty). `id` is a string — intToStr'd server-side, never a number.
const authorBody = z.object({
  id: z.string(),
  name: z.string(),
  email: z.string(),
  avatar: z.string(),
  description: z.string(),
  personal_project_id: z.string(),
  personalization: z.unknown().optional(),
});
const echoBody = z.object({ received: z.boolean() });
const loginPageBody = z.string();

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

/** R-M3 seam: validate one entry through F2's registration wrapper. */
function validated(id: string, schema: ZodType, fixture: { recordedAt: string; body: unknown }, handler: RequestHandler): RequestHandler {
  const [first] = registerValidatedHandlers([{ id, handler, schema, fixture }]);
  if (first === undefined) throw new Error(`transport handlers: registration returned nothing for ${id}`);
  return first;
}

/** Mutable auth gate shared between a test and its gated handlers. */
export interface SessionGate {
  authed: boolean;
}

/** 200 probe (fixture body); optionally captures request facts. */
export function probeOk(sink?: CapturedRequest[]): RequestHandler {
  return validated('transport.probe.ok', probeBody, probe200, http.get(`*${TRANSPORT_PROBE_PATH}`, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(probe200.body);
  }));
}

/** Any-method 200 probe — exercises the non-GET sugar methods. */
export function probeAny(sink?: CapturedRequest[]): RequestHandler {
  return validated('transport.probe.any', probeBody, probe200, http.all(`*${TRANSPORT_PROBE_PATH}`, async ({ request }) => {
    await capture(request, sink);
    return HttpResponse.json(probe200.body);
  }));
}

/** JSON content-type over a non-JSON body — the lying-server branch (§3.6). */
export function probeLyingJson(): RequestHandler {
  return validated('transport.probe.lyingJson', loginPageBody, loginPage200, http.get(`*${TRANSPORT_PROBE_PATH}`, () =>
    new HttpResponse(loginPage200.body, { headers: { 'Content-Type': 'application/json' } }),
  ));
}

/** 204 No Content — empty-body branch. */
export function probeNoContent(): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, () => new HttpResponse(null, { status: 204 }));
}

/** Plain 404 — §3.6 Result-discipline probe. */
export function probeNotFound(): RequestHandler {
  return validated('transport.probe.notFound', errorBody, notFound404, http.get(`*${TRANSPORT_PROBE_PATH}`, () =>
    HttpResponse.json(notFound404.body, { status: 404 }),
  ));
}

/** Network-level failure (no response body — nothing to derive, R-M2 n/a). */
export function probeNetworkError(): RequestHandler {
  return http.get(`*${TRANSPORT_PROBE_PATH}`, () => HttpResponse.error());
}

/** Never responds — abort-mid-flight probe (§5.4 behaviour 8). */
export function probeNeverending(): RequestHandler {
  return validated('transport.probe.neverending', probeBody, probe200, http.get(`*${TRANSPORT_PROBE_PATH}`, async () => {
    await delay('infinite');
    return HttpResponse.json(probe200.body);
  }));
}

/** 401/403 until `gate.authed`, then the 200 probe body (§5.4 behaviour 2/3). */
export function probeAuthGated(gate: SessionGate, deniedStatus: 401 | 403 = 401, sink?: CapturedRequest[]): RequestHandler {
  const denied = deniedStatus === 401 ? unauthorized401 : forbidden403;
  return validated(`transport.probe.authGated.${deniedStatus}`, errorBody, denied, http.get(`*${TRANSPORT_PROBE_PATH}`, async ({ request }) => {
    await capture(request, sink);
    if (!gate.authed) return HttpResponse.json(denied.body, { status: deniedStatus });
    return HttpResponse.json(probe200.body);
  }));
}

/** 401 on the first call, then a network-level failure — retry-crash probe. */
export function probeUnauthorizedThenNetworkError(): RequestHandler {
  let first = true;
  return validated('transport.probe.401ThenNetError', errorBody, unauthorized401, http.get(`*${TRANSPORT_PROBE_PATH}`, () => {
    if (first) {
      first = false;
      return HttpResponse.json(unauthorized401.body, { status: 401 });
    }
    return HttpResponse.error();
  }));
}

/** Redirects to `loginUrl` until `gate.authed` — the SECONDARY sniff signal. */
export function probeRedirectGated(gate: SessionGate, loginUrl: string): RequestHandler {
  return validated('transport.probe.redirectGated', probeBody, probe200, http.get(`*${TRANSPORT_PROBE_PATH}`, () => {
    if (!gate.authed) return HttpResponse.redirect(loginUrl, 302);
    return HttpResponse.json(probe200.body);
  }));
}

/** Serves the synthetic login page at an arbitrary absolute-path pattern. */
export function loginPage(pathPattern: string): RequestHandler {
  return validated('transport.loginPage', loginPageBody, loginPage200, http.get(`*${pathPattern}`, () =>
    HttpResponse.html(loginPage200.body),
  ));
}

/** POST echo, 401-gated — the byte-identical-replay probe captures every attempt's body. */
export function echoAuthGated(gate: SessionGate, sink: CapturedRequest[]): RequestHandler {
  return validated('transport.echo.authGated', echoBody, echo200, http.post(`*${TRANSPORT_ECHO_PATH}`, async ({ request }) => {
    await capture(request, sink);
    if (!gate.authed) return HttpResponse.json(unauthorized401.body, { status: 401 });
    return HttpResponse.json(echo200.body);
  }));
}

/** auth/me-class endpoint (§5.4 behaviour 5): 200 author when authed, real-shape 401 otherwise. */
export function authorGated(gate: SessionGate, sink?: CapturedRequest[]): RequestHandler {
  return validated('transport.author.gated', authorBody, author200, http.get(`*${AUTHOR_PATH}`, async ({ request }) => {
    await capture(request, sink);
    if (!gate.authed) return HttpResponse.json(unauthorized401.body, { status: 401 });
    return HttpResponse.json(author200.body);
  }));
}
