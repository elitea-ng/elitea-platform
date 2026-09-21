/**
 * The two UNATTENDED pipeline entry points, driven the way their callers
 * really drive them (#939 group 1).
 *
 * `internal/api/v2/pipelinetriggers` serves two independent facilities per
 * pipeline VERSION — an inbound signed trigger and a cron schedule — plus one
 * route that is not part of the API group at all:
 *
 *     POST /api/v2/pipeline_trigger/{projectID}/{tokenID}
 *
 * That inbound route is mounted on the ROOT mux, ABOVE the Auth group, beside
 * the anonymous shared-chat routes. It is authenticated by a credential the
 * SENDER holds and by nothing else — no session, no cookie, no header the
 * caller chooses. So a fixture that posted at it through `page.request` would
 * be testing a request the real sender can never make: `page.request` carries
 * the persona's session cookie, and a route that quietly started accepting
 * that cookie instead of the secret would still look green.
 *
 * {@link webhookSender} therefore mints an ANONYMOUS context — no
 * `storageState` — and every probe below goes through it. That is the whole
 * reason this module exists rather than a few inline `page.request` calls.
 *
 * ## TWO AUTHENTICATION MODES, AND A TRIGGER IS IN ONE OF THEM
 *
 * A `token` trigger — the default, and every trigger before #970 — accepts the
 * secret in three carriers, in this order (`inbound.go::presentedSecret`):
 * `Authorization: Bearer <secret>`, `X-Elitea-Trigger-Token: <secret>`, then
 * `?token=<secret>`. The stored side is a SHA-256 digest compared in constant
 * time. {@link WebhookCarrier} names exactly those three, and the absence of a
 * fourth is asserted by `pipelines.webhook-trigger.spec.ts` rather than
 * assumed here.
 *
 * An `hmac_sha256` trigger — what `{"type":"github"}` mints — accepts NONE of
 * them. The sender signs the RAW body with the same secret and sends the hex
 * digest in the trigger's `signature_header` (`X-Hub-Signature-256` for the
 * GitHub preset), which is what a repository webhook does: it sends no
 * `Authorization` header and cannot be configured to. {@link sendSignedWebhook}
 * is that sender, and it signs the EXACT bytes it puts on the wire — a signer
 * that re-serialised the body would prove nothing about the route, because the
 * digest would be over a payload the server never saw.
 *
 * ## WHAT AN ACCEPTED CREDENTIAL LOOKS LIKE ON A RUNTIME-LESS STACK
 *
 * `deploy/docker-compose.e2e-standalone.yml` composes no worker, so
 * `AgentStartUseCase` is nil and `admit()` answers `ErrRuntimeUnavailable`
 * BEFORE it authorizes anything (`run.go::admit`, first line). The inbound
 * route turns that into `503` with its own sentence — NOT the single
 * `refusal` string every credential failure shares. So on this stack an
 * accepted credential is observable, and is observably different from a
 * refused one: 401 + "this trigger cannot be used" versus 503 + "this
 * deployment cannot run pipelines". {@link expectCredentialAccepted} asserts
 * that distinction, and accepts the 202 the full stack answers instead, so
 * the same helper is correct in both lanes and is never vacuous in either.
 */
import { createHmac } from 'node:crypto';

import { expect, request as apiRequest, type APIRequestContext, type APIResponse } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { API_BASE } from './api';

/** The header name `inbound.go::TriggerTokenHeader` reads. */
export const TRIGGER_TOKEN_HEADER = 'X-Elitea-Trigger-Token';

/** The query parameter `inbound.go::TriggerTokenQueryParam` reads — the last-resort carrier. */
export const TRIGGER_TOKEN_QUERY_PARAM = 'token';

/**
 * The single sentence every credential refusal shares (`inbound.go::refusal`).
 * Asserting on it is how a test proves it got the REFUSAL and not some other
 * 401 the platform might grow later.
 */
export const TRIGGER_REFUSAL = 'this trigger cannot be used';

/** What a runtime-less deployment says instead — see this file's header. */
export const TRIGGER_NO_RUNTIME = 'this deployment cannot run pipelines';

/** How the secret is presented. `none` sends no credential at all. */
export type WebhookCarrier = 'bearer' | 'header' | 'query' | 'none';

/** One inbound trigger, as create/rotate and reveal answer it (`triggers.go::triggerView`). */
export interface InboundTrigger {
  readonly configured: boolean;
  /** 32 hex characters. Part of the URL, and the only public handle on the row. */
  readonly tokenId: string;
  /** The inbound path, `/api/v2/pipeline_trigger/{projectID}/{tokenID}` — ROOT-relative, no origin. */
  readonly url: string;
  /** The live credential. Present ONLY on create/rotate and reveal. */
  readonly secret: string;
  /** The same thing as one copy-pasteable URL, secret in the query string. */
  readonly secretUrl: string;
  /** Absent until an inbound call is ADMITTED — `stampTriggerUse` runs on the success path only. */
  readonly lastUsedAt: string | undefined;
  /**
   * When the trigger was revoked, or absent while it is live.
   *
   * A revoke does NOT delete the row: it stamps `revoked_at` and removes the
   * plaintext from the hidden vault (`triggers.go::RevokeTrigger`), so the
   * plain read still answers `configured: true` and the settings tab decides
   * what to draw from `revoked_at`. A test that read `configured` as "still
   * usable" would be asserting the opposite of what the column means.
   */
  readonly revokedAt: string | undefined;
  /** `token` or `hmac_sha256` (#970). Absent on a row written before the mode existed. */
  readonly authMode: string | undefined;
  /** The header a signing trigger reads the digest from. Absent for a bearer trigger. */
  readonly signatureHeader: string | undefined;
}

interface TriggerScope {
  readonly projectId: string;
  readonly versionId: string;
}

function describe(response: APIResponse, body: string): string {
  return `${String(response.status())} ${response.statusText()} ${body.slice(0, 300)}`;
}

function triggerFrom(raw: Record<string, unknown>): InboundTrigger {
  return {
    configured: raw['configured'] === true,
    tokenId: typeof raw['token_id'] === 'string' ? raw['token_id'] : '',
    url: typeof raw['url'] === 'string' ? raw['url'] : '',
    secret: typeof raw['secret'] === 'string' ? raw['secret'] : '',
    secretUrl: typeof raw['secret_url'] === 'string' ? raw['secret_url'] : '',
    lastUsedAt: typeof raw['last_used_at'] === 'string' ? raw['last_used_at'] : undefined,
    revokedAt: typeof raw['revoked_at'] === 'string' ? raw['revoked_at'] : undefined,
    authMode: typeof raw['auth_mode'] === 'string' ? raw['auth_mode'] : undefined,
    signatureHeader: typeof raw['signature_header'] === 'string' ? raw['signature_header'] : undefined,
  };
}

/**
 * Mint (or rotate) the pipeline version's inbound trigger and return it WITH
 * its credential. Carries `models.applications.version.update`, which is the
 * permission the route gates on — the same one editing the pipeline needs.
 */
export async function createInboundTrigger(
  request: APIRequestContext,
  scope: TriggerScope,
  body: Record<string, unknown> = {},
): Promise<InboundTrigger> {
  const url = `${API_BASE}/pipeline_triggers/prompt_lib/${scope.projectId}/${scope.versionId}`;
  const response = await request.post(url, { data: body });
  if (!response.ok()) {
    throw new Error(`createInboundTrigger: POST ${url} -> ${describe(response, await response.text())}`);
  }
  const trigger = triggerFrom((await response.json()) as Record<string, unknown>);
  if (trigger.tokenId === '' || trigger.secret === '' || trigger.url === '') {
    throw new Error(`createInboundTrigger: the create answered without a usable trigger: ${JSON.stringify(trigger)}`);
  }
  return trigger;
}

/**
 * Read the trigger back WITHOUT its credential — the plain read never carries
 * one. This is the server-side read `last_used_at` comes from.
 */
export async function readInboundTrigger(
  request: APIRequestContext,
  scope: TriggerScope,
): Promise<InboundTrigger> {
  const url = `${API_BASE}/pipeline_triggers/prompt_lib/${scope.projectId}/${scope.versionId}`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(`readInboundTrigger: GET ${url} -> ${describe(response, await response.text())}`);
  }
  return triggerFrom((await response.json()) as Record<string, unknown>);
}

/** Revoke the trigger. Best effort — a cleanup step must not fail the test it is cleaning up after. */
export async function revokeInboundTrigger(request: APIRequestContext, scope: TriggerScope): Promise<void> {
  await request.delete(`${API_BASE}/pipeline_triggers/prompt_lib/${scope.projectId}/${scope.versionId}`);
}

/** Options for {@link savePipelineSchedule}. */
export interface SchedulePlan {
  /** Five cron fields. `@daily` and friends are refused by the write path. */
  readonly cron: string;
  /**
   * Whether the schedule may fire. DEFAULTS TO FALSE SERVER-SIDE when the key
   * is omitted (`schedules.go::SaveSchedule`), so a fixture that left it out
   * would save a schedule that can never fire and a test waiting on it would
   * time out with nothing to say why.
   */
  readonly active?: boolean;
  /** The text placed in front of the pipeline. Bounded at 4096 bytes. */
  readonly input?: string;
}

/**
 * Save the pipeline version's cron schedule. The CALLER becomes its author,
 * and the author's project permission is re-resolved at every fire — so the
 * persona that saves it is the identity a scheduled run executes as.
 *
 * ## THIS IS THE "FIRE NOW" CAPABILITY, AND IT NEEDS NO PRODUCT CHANGE
 *
 * A test does not have to wait for a cron expression to come round.
 * `schedulerun.go::timeToRun` opens with
 *
 *     if schedule.LastRun == nil { return true }
 *
 * so a schedule that has NEVER run is due on the very next tick whatever its
 * expression says, and the tick itself is registered at `* * * * *`
 * (`job.go::ScheduleJobCadence`). An `active: true` schedule therefore fires
 * within ONE tick — at most ~60 s — of being saved, deterministically and
 * without an e2e-only endpoint, an env-gated clock, or a one-minute cron
 * expression whose minute boundary the test would have to race.
 *
 * The tick is only registered where a runtime is composed
 * (`cmd/elitea-main/main.go`, inside the `publicRoutes.AgentStart != nil`
 * block), so this fires on the chat-stream stack and never on the journeys
 * stack — which is why the schedule FIRE cases live in the streaming lane.
 */
export async function savePipelineSchedule(
  request: APIRequestContext,
  scope: TriggerScope,
  plan: SchedulePlan,
): Promise<Record<string, unknown>> {
  const url = `${API_BASE}/pipeline_schedules/prompt_lib/${scope.projectId}/${scope.versionId}`;
  const response = await request.put(url, {
    data: {
      cron: plan.cron,
      active: plan.active ?? true,
      ...(plan.input === undefined ? {} : { input: plan.input }),
    },
  });
  if (!response.ok()) {
    throw new Error(`savePipelineSchedule: PUT ${url} -> ${describe(response, await response.text())}`);
  }
  return (await response.json()) as Record<string, unknown>;
}

/** Read the schedule back — `last_run`/`last_result` are how a fire is observed. */
export async function readPipelineSchedule(
  request: APIRequestContext,
  scope: TriggerScope,
): Promise<Record<string, unknown>> {
  const url = `${API_BASE}/pipeline_schedules/prompt_lib/${scope.projectId}/${scope.versionId}`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(`readPipelineSchedule: GET ${url} -> ${describe(response, await response.text())}`);
  }
  return (await response.json()) as Record<string, unknown>;
}

/** Delete the schedule. Best effort, for the same reason as {@link revokeInboundTrigger}. */
export async function deletePipelineSchedule(request: APIRequestContext, scope: TriggerScope): Promise<void> {
  await request.delete(`${API_BASE}/pipeline_schedules/prompt_lib/${scope.projectId}/${scope.versionId}`);
}

/**
 * An ANONYMOUS request context — no persona, no cookie, no storage state.
 *
 * This is the capability the webhook cases were deferred for: it is the only
 * way to send what a third-party webhook sender actually sends. Dispose it in
 * `finally`.
 */
export async function webhookSender(): Promise<APIRequestContext> {
  return apiRequest.newContext({ baseURL: BASE_URL });
}

export interface WebhookProbe {
  /** Defaults to `bearer`. */
  readonly carrier?: WebhookCarrier;
  /** The credential to present. Ignored when `carrier` is `none`. */
  readonly secret?: string;
  /** The raw request body. Defaults to no body at all, which the route treats as an empty input. */
  readonly body?: string;
  /** Defaults to `POST` — the only method the route registers. */
  readonly method?: 'POST' | 'GET' | 'PUT' | 'DELETE' | 'PATCH';
}

/**
 * Send one request at the trigger's inbound URL, exactly as a webhook sender
 * would: an anonymous context, a raw body, and the credential in one of the
 * three carriers the handler reads.
 */
export async function sendWebhook(
  sender: APIRequestContext,
  trigger: Pick<InboundTrigger, 'url'>,
  probe: WebhookProbe = {},
): Promise<APIResponse> {
  const carrier = probe.carrier ?? 'bearer';
  const secret = probe.secret ?? '';
  const headers: Record<string, string> = { 'content-type': 'application/json' };
  if (carrier === 'bearer') headers['Authorization'] = `Bearer ${secret}`;
  if (carrier === 'header') headers[TRIGGER_TOKEN_HEADER] = secret;
  const url =
    carrier === 'query'
      ? `${BASE_URL}${trigger.url}?${TRIGGER_TOKEN_QUERY_PARAM}=${encodeURIComponent(secret)}`
      : `${BASE_URL}${trigger.url}`;
  const options = { headers, ...(probe.body === undefined ? {} : { data: probe.body }) };
  switch (probe.method ?? 'POST') {
    case 'GET':
      return sender.get(url, options);
    case 'PUT':
      return sender.put(url, options);
    case 'DELETE':
      return sender.delete(url, options);
    case 'PATCH':
      return sender.patch(url, options);
    default:
      return sender.post(url, options);
  }
}

/** The header the GitHub preset signs into (`authmode.go::GitHubSignatureHeader`). */
export const GITHUB_SIGNATURE_HEADER = 'X-Hub-Signature-256';

/** Options for {@link sendSignedWebhook}. */
export interface SignedWebhookProbe {
  /** The secret the trigger was minted with — the HMAC key. */
  readonly secret: string;
  /** The raw body. Signed and sent VERBATIM; nothing re-serialises it. */
  readonly body: string;
  /** Defaults to the GitHub header. */
  readonly header?: string;
  /**
   * Send this instead of the computed digest. For the tampered-signature case;
   * leave it out to send a correct one.
   */
  readonly signature?: string;
}

/**
 * Send one request the way a GitHub repository webhook sends it: no
 * `Authorization` header at all, the raw body, and
 * `X-Hub-Signature-256: sha256=<hex>` over exactly those bytes.
 *
 * The digest is computed HERE rather than asked of the server, which is the
 * whole point: a signature the server produced would prove nothing about
 * whether it verifies one.
 */
export async function sendSignedWebhook(
  sender: APIRequestContext,
  trigger: Pick<InboundTrigger, 'url'>,
  probe: SignedWebhookProbe,
): Promise<APIResponse> {
  const digest = probe.signature ?? `sha256=${createHmac('sha256', probe.secret).update(probe.body).digest('hex')}`;
  return sender.post(`${BASE_URL}${trigger.url}`, {
    headers: { 'content-type': 'application/json', [probe.header ?? GITHUB_SIGNATURE_HEADER]: digest },
    data: probe.body,
  });
}

/**
 * Assert that a request was REFUSED for its credential: the one status and the
 * one sentence every credential failure shares.
 *
 * Reading the body matters. A 401 from the surrounding auth middleware — the
 * failure mode if this route were ever mounted INSIDE the Auth group — is also
 * a 401, and would pass a status-only check while proving the opposite of what
 * the case asks.
 */
export async function expectCredentialRefused(response: APIResponse, what: string): Promise<void> {
  const body = await response.text();
  expect(response.status(), `${what}: expected 401 for a bad credential, got ${describe(response, body)}`).toBe(401);
  expect(body, `${what}: the refusal must be the shared one, not a reason that identifies the cause`).toContain(
    TRIGGER_REFUSAL,
  );
}

/**
 * Assert that a request's credential was ACCEPTED — the run was admitted (202
 * on a stack with a runtime) or refused for the RUNTIME (503 with its own
 * sentence, on the journeys stack, which composes no worker).
 *
 * The two are the same statement about authentication and different statements
 * about the deployment, which is why one helper covers both lanes.
 */
export async function expectCredentialAccepted(response: APIResponse, what: string): Promise<void> {
  const body = await response.text();
  expect(
    [202, 503],
    `${what}: an accepted credential answers 202 (admitted) or 503 (no runtime), got ${describe(response, body)}`,
  ).toContain(response.status());
  if (response.status() === 503) {
    expect(body, `${what}: a 503 here must be the runtime's absence, not a credential refusal`).toContain(
      TRIGGER_NO_RUNTIME,
    );
  }
  expect(body, `${what}: an accepted credential must never carry the credential refusal`).not.toContain(
    TRIGGER_REFUSAL,
  );
}

/**
 * The conversations a trigger run would appear as, by name.
 *
 * A run started by either entry point creates one conversation named
 * `"<Origin>: <pipeline name>"` (`run.go::runConversationName`, origins
 * `Webhook` and `Schedule`), and the pipeline's run history is exactly the
 * list of conversations that used it. A uniquely named pipeline therefore
 * makes this count exact without a scratch project, even with four workers on
 * the shared project.
 */
export async function countTriggerRunConversations(
  request: APIRequestContext,
  projectId: string,
  pipelineName: string,
): Promise<number> {
  const url = `${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}?limit=100`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(`countTriggerRunConversations: GET ${url} -> ${describe(response, await response.text())}`);
  }
  const body = (await response.json()) as { rows?: unknown; items?: unknown };
  const rows = Array.isArray(body.rows) ? body.rows : Array.isArray(body.items) ? body.items : [];
  return rows.filter((row) => {
    const name = (row as { name?: unknown }).name;
    return typeof name === 'string' && (name === `Webhook: ${pipelineName}` || name === `Schedule: ${pipelineName}`);
  }).length;
}
