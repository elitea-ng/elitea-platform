/**
 * THE INBOUND PIPELINE TRIGGER, DRIVEN BY A REAL WEBHOOK SENDER (#939 group 1).
 *
 * `pipelines.entrypoint.spec.ts` proves the editor SURFACE — the dropdown, the
 * two facilities, the reveal, the delete. It never sends the request the
 * credential exists for. This file does, through `e2e/fixtures/
 * pipelineTriggers.ts`'s anonymous sender, which is the capability #939 group 1
 * was deferred for: `POST /api/v2/pipeline_trigger/{projectID}/{tokenID}` is
 * mounted on the ROOT mux ABOVE the Auth group, so a probe sent through
 * `page.request` — which carries the persona's session cookie — would be
 * testing a request no real sender can make, and a route that started
 * accepting the cookie instead of the secret would still look green.
 *
 * ── WHAT THIS STACK CAN AND CANNOT SAY ──────────────────────────────────
 *
 * `deploy/docker-compose.e2e-standalone.yml` composes no worker, so
 * `AgentStartUseCase` is nil and `admit()` answers `ErrRuntimeUnavailable` on
 * its FIRST line, before it authorizes anything. That is not a hole in these
 * assertions — it is what makes an accepted credential OBSERVABLE here:
 *
 *   refused credential → 401 + "this trigger cannot be used"
 *   accepted credential → 503 + "this deployment cannot run pipelines"
 *
 * Two different statuses and two different sentences, so nothing below passes
 * vacuously. `expectCredentialAccepted` also admits the 202 a stack WITH a
 * runtime answers, so the same file is correct in the chat-stream lane, where
 * the run-history half of ELITEA-0881/0884/0873 belongs (those cases need a
 * turn to actually happen and are tracked as streaming work under #939).
 *
 * ── TWO SCHEMES, AND A TRIGGER IS IN ONE OF THEM ────────────────────────
 *
 * A `token` trigger takes the bearer secret in three carriers; an
 * `hmac_sha256` trigger takes a signature over the raw body and NONE of the
 * three (#970). The last test in this file is the GitHub case, sent the way a
 * repository webhook sends it.
 */
import { createHmac } from 'node:crypto';

import { expect, test } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../../fixtures/pipelines';
import {
  countTriggerRunConversations,
  createInboundTrigger,
  deletePipelineSchedule,
  expectCredentialAccepted,
  expectCredentialRefused,
  readInboundTrigger,
  readPipelineSchedule,
  revokeInboundTrigger,
  savePipelineSchedule,
  sendSignedWebhook,
  sendWebhook,
  GITHUB_SIGNATURE_HEADER,
  TRIGGER_REFUSAL,
  TRIGGER_TOKEN_HEADER,
  webhookSender,
  type InboundTrigger,
} from '../../fixtures/pipelineTriggers';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline === undefined) continue;
    await revokeInboundTrigger(page.request, { projectId: pipeline.projectId, versionId: pipeline.versionId });
    await deletePipeline(page.request, pipeline);
  }
});

/**
 * A pipeline with a live inbound trigger, named uniquely so the run-history
 * read below is exact on the shared project even with four workers running.
 */
async function triggeredPipeline(
  request: Parameters<typeof createPipelineThroughApi>[0],
  label: string,
): Promise<{ pipeline: CreatedPipeline; name: string; trigger: InboundTrigger }> {
  const name = `${AUTOTEST_PREFIX}wh-${label}-${Date.now() % 1e9}-${Math.floor(Math.random() * 1e4)}`;
  const pipeline = await createPipelineThroughApi(request, name);
  created.push(pipeline);
  const trigger = await createInboundTrigger(request, {
    projectId: pipeline.projectId,
    versionId: pipeline.versionId,
  });
  return { pipeline, name, trigger };
}

/*
 * onetest: ELITEA-0880 — a webhook sent with no credential, or with the wrong
 * one, is refused with the single shared refusal; GET/PUT/DELETE at the same
 * URL are refused as methods; and none of the refused calls leaves a run in
 * the pipeline's history or stamps the trigger as used.
 */
test('the inbound trigger refuses a missing or wrong credential, and refuses every method but POST', async ({ page }) => {
  test.setTimeout(90_000);
  const { pipeline, name, trigger } = await triggeredPipeline(page.request, 'refuse');
  const before = await countTriggerRunConversations(page.request, pipeline.projectId, name);
  const sender = await webhookSender();
  try {
    // ── no credential at all, in any of the three carriers ────────────────
    await expectCredentialRefused(
      await sendWebhook(sender, trigger, { carrier: 'none', body: '{"message":"no token"}' }),
      'POST with no credential',
    );

    // ── the wrong secret, in each carrier the handler reads ───────────────
    // All three, because `presentedSecret` reads them in order and stops at
    // the first non-empty one: a bug that ignored the header would be hidden
    // by a probe that only ever used the bearer form.
    for (const carrier of ['bearer', 'header', 'query'] as const) {
      await expectCredentialRefused(
        await sendWebhook(sender, trigger, { carrier, secret: 'wrong-secret-value', body: '{}' }),
        `POST with a wrong secret in the ${carrier} carrier`,
      );
    }

    // ── a credential that is well-formed but belongs to nobody ────────────
    await expectCredentialRefused(
      await sendWebhook(sender, trigger, { secret: trigger.secret.slice(0, -1) + 'x', body: '{}' }),
      'POST with a one-character-off secret',
    );

    // ── every other method ────────────────────────────────────────────────
    // MEASURED, and not what the source case predicts. Only POST is registered
    // on the ROOT mux at this pattern; GET/PUT/DELETE/PATCH fall through to the
    // authenticated `/api/v2` group and are answered by the auth middleware —
    // 401 `{"code":"no_credential"}`, not chi's 405. The case asks for 405 (or
    // 503); what matters to it is that a non-POST method is REFUSED and starts
    // nothing, and that is exactly what happens, so this asserts the refusal
    // the platform really makes rather than a status it does not.
    //
    // The body is the discriminator and is why this is not a status-only
    // check: the trigger handler's own refusal sentence appearing here would
    // mean the handler RAN on a GET — the failure worth catching.
    for (const method of ['GET', 'PUT', 'DELETE', 'PATCH'] as const) {
      const response = await sendWebhook(sender, trigger, { method, secret: trigger.secret });
      const body = await response.text();
      expect([401, 405], `${method} at the inbound URL must be refused, got ${String(response.status())}`).toContain(
        response.status(),
      );
      expect(body, `${method} must never reach the trigger handler`).not.toContain(TRIGGER_REFUSAL);
    }

    // ── nothing above started anything ────────────────────────────────────
    // Server-side, twice over: the trigger row's own `last_used_at` is stamped
    // only on the admitted path (`inbound.go::stampTriggerUse`), and a run
    // would appear as a conversation named "Webhook: <pipeline>"
    // (`run.go::runConversationName`).
    const afterRefusals = await readInboundTrigger(page.request, {
      projectId: pipeline.projectId,
      versionId: pipeline.versionId,
    });
    expect(afterRefusals.configured, 'the trigger is still configured after the refusals').toBe(true);
    expect(afterRefusals.lastUsedAt, 'a refused call must never stamp the trigger as used').toBeUndefined();
    expect(
      await countTriggerRunConversations(page.request, pipeline.projectId, name),
      'no refused request may leave a run in the pipeline run history',
    ).toBe(before);
  } finally {
    await sender.dispose();
  }
});

/*
 * onetest: ELITEA-0881 — the correct secret in any of the three carriers is
 * ACCEPTED (this deployment then refuses for its missing runtime, with its own
 * sentence, not the credential refusal); an empty body and a 10 KB body are
 * both accepted; a body past the 64 KiB ceiling is refused as too large, not
 * as a bad credential.
 */
test('the inbound trigger accepts the minted secret in every carrier, and takes an empty, large or absent body', async ({ page }) => {
  test.setTimeout(90_000);
  const { pipeline, trigger } = await triggeredPipeline(page.request, 'accept');
  const sender = await webhookSender();
  try {
    for (const carrier of ['bearer', 'header', 'query'] as const) {
      await expectCredentialAccepted(
        await sendWebhook(sender, trigger, { carrier, secret: trigger.secret, body: '{"message":"webhook test input"}' }),
        `POST with the minted secret in the ${carrier} carrier`,
      );
    }

    // No body at all, and an empty one. `readInboundBody` treats both as an
    // empty input rather than an error: a webhook that only says "something
    // happened" carries nothing this service should require.
    await expectCredentialAccepted(
      await sendWebhook(sender, trigger, { secret: trigger.secret }),
      'POST with no body',
    );
    await expectCredentialAccepted(
      await sendWebhook(sender, trigger, { secret: trigger.secret, body: '' }),
      'POST with an empty body',
    );

    // A payload the sender's format owns, not this service's: an unparseable
    // body is accepted as an empty input rather than refused.
    await expectCredentialAccepted(
      await sendWebhook(sender, trigger, { secret: trigger.secret, body: 'not json at all' }),
      'POST with an unparseable body',
    );

    // 10 KB, the case's own "large payload". Well inside the 64 KiB ceiling.
    const large = JSON.stringify({ message: 'x'.repeat(10 * 1024) });
    await expectCredentialAccepted(
      await sendWebhook(sender, trigger, { secret: trigger.secret, body: large }),
      'POST with a 10 KB payload',
    );

    // Past the ceiling. The refusal is about the BODY — a different status and
    // a different sentence from the credential refusal, which is the point:
    // a service that answered 401 here would teach senders to rotate a secret
    // that is perfectly good.
    const oversized = JSON.stringify({ message: 'x'.repeat(70 * 1024) });
    const refusedBody = await sendWebhook(sender, trigger, { secret: trigger.secret, body: oversized });
    expect(refusedBody.status(), 'a body past the ceiling is refused as too large').toBe(413);
    expect(await refusedBody.text()).toContain('too large');

    // The header carrier is the preferred one, and its name is part of the
    // contract a sender is configured against.
    expect(TRIGGER_TOKEN_HEADER).toBe('X-Elitea-Trigger-Token');

    // The URL is the stored row's handle and carries no project-selecting
    // input: the path names WHERE to look, and what runs comes from the row.
    expect(trigger.url).toBe(`/api/v2/pipeline_trigger/${pipeline.projectId}/${trigger.tokenId}`);
  } finally {
    await sender.dispose();
  }
});

/*
 * onetest: ELITEA-0880 (revocation half) — a revoked trigger's secret stops
 * working immediately, and is refused with the same shared refusal as a wrong
 * one, so a caller cannot tell revoked from unknown.
 */
test('a revoked trigger refuses the credential it used to accept', async ({ page }) => {
  test.setTimeout(90_000);
  const { pipeline, trigger } = await triggeredPipeline(page.request, 'revoke');
  const sender = await webhookSender();
  try {
    await expectCredentialAccepted(
      await sendWebhook(sender, trigger, { secret: trigger.secret, body: '{}' }),
      'POST before the revoke',
    );
    await revokeInboundTrigger(page.request, { projectId: pipeline.projectId, versionId: pipeline.versionId });
    await expectCredentialRefused(
      await sendWebhook(sender, trigger, { secret: trigger.secret, body: '{}' }),
      'POST after the revoke',
    );
    const afterRevoke = await readInboundTrigger(page.request, {
      projectId: pipeline.projectId,
      versionId: pipeline.versionId,
    });
    // The row is KEPT and stamped; only the plaintext is destroyed. So the
    // read still says `configured`, and `revoked_at` is what changed — the
    // column the settings tab draws the revoked state from.
    expect(afterRevoke.revokedAt, 'the revoke stamps revoked_at').toBeDefined();
  } finally {
    await sender.dispose();
  }
});

/*
 * onetest: ELITEA-0879 — a GitHub-type webhook trigger, signed the way GitHub
 * signs: `X-Hub-Signature-256: sha256=<HMAC-SHA256(secret, body)>`, at the URL
 * ending in `/github` this service hands out, with a tampered signature and a
 * missing header both refused.
 *
 * The SIGNATURE is computed in this test and never asked of the server, and
 * the body is signed and sent as the same string — a re-serialised payload
 * would be signed over bytes the server never receives, and the case would
 * pass or fail for a reason that has nothing to do with the route.
 *
 * The accepted call is asserted with the same `expectCredentialAccepted` every
 * other case in this file uses, for the reason the file header states: this
 * stack composes no worker, so an accepted credential answers 503 with the
 * RUNTIME's sentence rather than 202. A bare `status < 300` here would fail on
 * a correct implementation and pass on none.
 */
test('a GitHub-type webhook trigger validates an X-Hub-Signature-256 HMAC', async ({ page }) => {
  test.setTimeout(90_000);
  const name = `${AUTOTEST_PREFIX}wh-github-${Date.now() % 1e9}-${Math.floor(Math.random() * 1e4)}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const scope = { projectId: pipeline.projectId, versionId: pipeline.versionId };
  const trigger = await createInboundTrigger(page.request, scope, { type: 'github' });
  expect(trigger.url, 'a GitHub-type trigger is exposed at a /github URL').toMatch(/\/github$/);
  expect(trigger.authMode, 'the stored row carries the signature mode').toBe('hmac_sha256');
  expect(trigger.signatureHeader, 'the row names the header the sender must sign into').toBe(GITHUB_SIGNATURE_HEADER);

  const body = '{"ref": "refs/heads/main", "commits": []}';

  const sender = await webhookSender();
  try {
    await expectCredentialAccepted(
      await sendSignedWebhook(sender, trigger, { secret: trigger.secret, body }),
      'a correctly signed GitHub webhook',
    );

    await expectCredentialRefused(
      await sendSignedWebhook(sender, trigger, { secret: trigger.secret, body, signature: 'sha256=aaabbbcccddd000' }),
      'a tampered signature',
    );

    // The signature is the trigger's OWN and the body is not the one it was
    // computed over — the case a digest comparison cannot catch, and the
    // reason the handler keeps the raw bytes.
    await expectCredentialRefused(
      await sender.post(`${trigger.url}`, {
        headers: {
          'content-type': 'application/json',
          [GITHUB_SIGNATURE_HEADER]: `sha256=${createHmac('sha256', trigger.secret).update(body).digest('hex')}`,
        },
        data: '{"ref": "refs/heads/main", "commits": [], "tampered": true}',
      }),
      'the right signature over a tampered body',
    );

    await expectCredentialRefused(
      await sender.post(`${trigger.url}`, { headers: { 'content-type': 'application/json' }, data: body }),
      'a webhook with no signature header',
    );

    // The mode is not decorative: the bearer secret, which is the same string,
    // must not be a second way in. Otherwise a URL in a proxy log plus the
    // secret pasted into the provider's form would still start runs.
    await expectCredentialRefused(
      await sendWebhook(sender, trigger, { secret: trigger.secret, body }),
      'the bearer secret against a signing trigger',
    );
  } finally {
    await sender.dispose();
  }
});

/*
 * The trigger's URL is derived from the STORED row, and the project segment
 * only says WHERE TO LOOK — rule 1 of the package's auth story. A well-formed
 * token id that names no row gets the SAME refusal as a wrong secret, so the
 * response is no oracle for which triggers a deployment holds.
 *
 * onetest: ELITEA-0880 (tenant-selection half).
 */
test('an unknown token id is refused with the same sentence as a wrong secret', async ({ page }) => {
  test.setTimeout(90_000);
  const { trigger } = await triggeredPipeline(page.request, 'tenant');
  const sender = await webhookSender();
  try {
    // Same length, same alphabet, same project — only the row is missing.
    const unknown = trigger.url.replace(trigger.tokenId, 'f'.repeat(trigger.tokenId.length));
    expect(unknown, 'the probe must actually differ from the live URL').not.toBe(trigger.url);
    await expectCredentialRefused(
      await sendWebhook(sender, { url: unknown }, { secret: trigger.secret, body: '{}' }),
      'POST at an unknown token id',
    );
    // And the live one still works, so the refusal above is about the id and
    // not about the sender having been locked out by the probes.
    await expectCredentialAccepted(
      await sendWebhook(sender, trigger, { secret: trigger.secret, body: '{}' }),
      'POST at the live token id',
    );
  } finally {
    await sender.dispose();
  }
});

/*
 * The SCHEDULE half of the same package, as far as a stack with no runtime can
 * take it: what the write accepts, what it refuses, and what it stores.
 *
 * The FIRE is deliberately not here. The tick is registered only inside
 * `cmd/elitea-main/main.go`'s `publicRoutes.AgentStart != nil` block, so this
 * stack never runs it — which is also why an active schedule saved here can be
 * asserted to have NOT fired without racing anything. The fire itself
 * (ELITEA-0884's execution half, ELITEA-0873's run-history labels) belongs to
 * the chat-stream lane, where `savePipelineSchedule`'s doc comment explains the
 * one-tick "fire now" property a test there relies on.
 *
 * onetest: ELITEA-0884 (configuration half — the cron is saved, active, and
 * previews its next run).
 */
test('the schedule write refuses an unparseable cron, stores a five-field one, and previews its next run', async ({ page }) => {
  test.setTimeout(90_000);
  const name = `${AUTOTEST_PREFIX}sched-${Date.now() % 1e9}-${Math.floor(Math.random() * 1e4)}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const scope = { projectId: pipeline.projectId, versionId: pipeline.versionId };

  // Named cadences are refused by name: the parser is five-field only, and the
  // refusal says so rather than answering 500 from a parse panic.
  for (const bad of ['@daily', '* * *', 'not a cron']) {
    const refused = await page.request.put(
      `${API_BASE}/pipeline_schedules/prompt_lib/${scope.projectId}/${scope.versionId}`,
      { data: { cron: bad, active: true } },
    );
    expect(refused.status(), `"${bad}" must be refused by the write path`).toBe(400);
    expect(await refused.text()).toContain('cron');
  }

  const saved = await savePipelineSchedule(page.request, scope, {
    cron: '* * * * *',
    active: true,
    input: 'scheduled test input',
  });
  expect(saved['configured']).toBe(true);
  expect(saved['cron']).toBe('* * * * *');
  expect(saved['active'], 'the schedule is saved ACTIVE — the server defaults this to false when omitted').toBe(true);
  expect(saved['input']).toBe('scheduled test input');
  // Computed by the same parser that will fire it, so the preview and the
  // behaviour cannot disagree.
  expect(typeof saved['next_run'], 'a valid cron previews its next run').toBe('string');
  // The saver is the author, and the author's permission is what a fire
  // re-resolves — so this field is the identity a scheduled run executes as.
  expect(typeof saved['author_id']).toBe('number');

  const readBack = await readPipelineSchedule(page.request, scope);
  expect(readBack['cron']).toBe('* * * * *');
  expect(readBack['active']).toBe(true);
  expect(readBack['input']).toBe('scheduled test input');
  // Never fired here: this stack composes no runtime, so the tick that would
  // make an unfired schedule due on its next pass is not registered at all.
  expect(readBack['last_run'], 'a stack with no runtime never fires a schedule').toBeUndefined();

  await deletePipelineSchedule(page.request, scope);
  const afterDelete = await readPipelineSchedule(page.request, scope);
  // A schedule is DELETED, not tombstoned — unlike a trigger, which is a
  // credential somebody outside holds.
  expect(afterDelete['configured'], 'a deleted schedule reads back as unconfigured').toBe(false);
});
