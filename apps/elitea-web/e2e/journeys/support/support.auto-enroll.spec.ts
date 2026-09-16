/**
 * Support Assistant — a brand-new, never-before-seen user's FIRST contact
 * (issue #940 Bucket D2, onetest ELITEA-0602, `elitea-chat-bot` folder).
 *
 * Runs against the FULL standalone stack, project `support-stack`
 * (`scripts/support-e2e.sh`), the same as `support.spec.ts` — `Predict`
 * admits a real agent turn (`StartCurrentApplication`), which the plain
 * `e2e-standalone` compose has no runtime plane/worker for.
 *
 * ── WHY THIS NEEDED ITS OWN FIXTURE, NOT AN EXISTING PERSONA ────────────────
 *
 * Every existing persona (`member`, `admin`, `chat`, and this package's own
 * `viewer`) is minted once by `scripts/e2e-stack.sh seed` and then signed in
 * by `auth.setup.ts`, whose very first act on the support surface — if it
 * ever touched one — would no longer be a FIRST contact by the time this file
 * runs. ELITEA-0602 is specifically about a user with no prior enrolment
 * anywhere, so this file mints its own: `signInThroughOidc` (`e2e/fixtures/
 * session.ts`) drives a real OIDC round trip for an email nothing has ever
 * PUT into oidc-mock or written into `auth_core__user`, inside a BROWSER
 * CONTEXT this test creates and closes itself — never the shared setup
 * personas' storage state. oidc-provider-mock does not require an email to be
 * pre-registered to sign in (member/admin/viewer all do get a PUT, but that
 * is for the personas' own oidc-mock user profile, not a signin
 * precondition), and elitea-main's own OIDC callback upserts
 * `auth_core__user` on ANY new email (`internal/api/v2/auth/oidc.go:742`) —
 * which is exactly the mechanism a genuinely first-time user goes through.
 *
 * ── WHAT IS PROVEN, AND WHAT IS NOT (delta from the onetest case) ──────────
 *
 * Proven, against this stack's own admin-configured support section
 * (`enableSupportAssistant`, same helper `support.spec.ts` uses):
 *
 *  1. The FIRST EVER request this identity makes to the support surface
 *     (`GET /support_assistant/conversations/`) answers 200, not 403 —
 *     `requireSupportProject`'s `ensureEnrolled` runs BEFORE the permission
 *     gate (`handler.go`'s own doc comment states the ordering), so an
 *     unenrolled caller is admitted rather than refused.
 *  2. The enrolment granted is `viewer`, not editor/admin. The support
 *     project this stack configures (`SUPPORT_PROJECT_ID` below) is a
 *     Go-provisioned project with no per-project permission override rows,
 *     so its permission resolution falls straight through to the CENTRAL
 *     default-mode grants migrations 0068/0070 record: `viewer` holds
 *     `models.chat.conversations.list` (and `.create`, `.messages.create`)
 *     but NOT `models.applications.applications.create`, which only
 *     admin/editor hold. Checking the caller's OWN resolved permission set
 *     is a more precise proof than an admin-side membership read, and needs
 *     no admin-only route this package would otherwise have to add.
 *  3. Conversation and message creation both succeed (201, then 200) for the
 *     newly enrolled identity — `POST conversations/` then `POST
 *     predict/{uuid}`. `Predict` ADMITS the turn synchronously
 *     (`StartCurrentApplication`) and returns once it has started, so this
 *     does not wait for a full model reply — same boundary `support.
 *     entrypoints.spec.ts`'s header states for why IT never sends either.
 *
 * NOT proven — PRODUCT GAP, not merely a fixture shortcut:
 *
 *  ELITEA-0602's step 3 asks that the hidden project NOT appear in the
 *  caller's project list. On THIS deployment (and every deployment this
 *  repository currently builds — `cmd/elitea-main/main.go` never calls
 *  `supportassistant.WithProvisioner`, grep-verified), the support project is
 *  never actually bootstrapped as a separate hidden project: `store.
 *  bootstrapProject` always fails closed with `ErrNoProvisioner`, so the ONLY
 *  way to make the assistant serve at all is to point `support_project_id`
 *  at an EXISTING, ordinary, visible project — which is exactly what
 *  `support.spec.ts` and this file both do, at `SUPPORT_PROJECT_ID` (project
 *  1, the seeded shared project). That project is not hidden from anybody;
 *  asserting "it does not appear in this user's list" would assert something
 *  false about a project every persona already sees. The case's real, deeper
 *  claim — that a purpose-provisioned hidden project stays hidden — is
 *  currently unreachable on any build of this service, and is recorded here
 *  as a product gap rather than silently dropped or falsely asserted.
 */
import { randomUUID } from 'node:crypto';

import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, createAgent, deleteAgent, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { signInThroughOidc } from '../../fixtures/session';
import { disableSupportAssistant, enableSupportAssistant } from './helpers';

test.use({ storageState: STORAGE_STATE.admin });

const SUPPORT_PROJECT_ID = Number(DEFAULT_PROJECT_ID);
const RUN_ID = Date.now();
const AGENT_NAME = `${AUTOTEST_PREFIX}support_auto_enroll_agent_${RUN_ID}`;
const FRESH_USER_EMAIL = `${AUTOTEST_PREFIX}fresh_support_user_${RUN_ID}@autotest.local`;

let agentId = '';

test.afterAll(async ({ request }) => {
  // Best effort, same shape as `support.spec.ts`'s own cleanup: restore the
  // switch to OFF and remove the agent this file created.
  await disableSupportAssistant(request).catch(() => {});
  if (agentId) {
    await deleteAgent(request, agentId).catch(() => {});
  }
});

/* onetest: ELITEA-0602 — a brand-new user is auto-enrolled as viewer on first contact with the Support Assistant; see the file header for the one sub-claim (hidden-project exclusion) recorded as a product gap instead */
test('ELITEA-0602: a brand-new user is auto-enrolled as viewer on first contact with the Support Assistant', async ({
  request,
  browser,
}) => {
  test.setTimeout(120_000);

  const agent = await createAgent(request, AGENT_NAME);
  agentId = agent.id;
  await enableSupportAssistant(request, {
    projectId: SUPPORT_PROJECT_ID,
    agentId: Number(agentId),
    name: `${AUTOTEST_PREFIX}Auto-enroll support`,
    welcomeMessage: `${AUTOTEST_PREFIX}welcome`,
    placeholder: `${AUTOTEST_PREFIX}placeholder`,
  });

  // A context THIS TEST owns — never the shared setup personas' storage
  // state — see the file header's "WHY THIS NEEDED ITS OWN FIXTURE" note.
  const freshContext = await browser.newContext();
  try {
    const freshPage = await freshContext.newPage();
    await signInThroughOidc(freshPage, FRESH_USER_EMAIL);

    // ── 1. First-ever call succeeds, and enrols ────────────────────────────
    const listResponse = await freshPage.request.get(`${API_BASE}/support_assistant/conversations/`);
    expect(
      listResponse.status(),
      `first-ever GET /support_assistant/conversations for ${FRESH_USER_EMAIL} must not be refused`,
    ).toBe(200);

    // ── 2. Enrolled as VIEWER, not editor/admin ─────────────────────────────
    const permsResponse = await freshPage.request.get(
      `${API_BASE}/auth/permissions/prompt_lib/${SUPPORT_PROJECT_ID}`,
    );
    expect(permsResponse.ok(), 'permission read for the newly enrolled user must succeed').toBe(true);
    const perms = (await permsResponse.json()) as readonly { name: string; enabled: boolean }[];
    const granted = new Set(perms.filter((p) => p.enabled).map((p) => p.name));
    expect(
      granted.has('models.chat.conversations.list'),
      'enrolment must grant the viewer-level list permission',
    ).toBe(true);
    expect(
      granted.has('models.applications.applications.create'),
      'enrolment must NOT grant an editor/admin-only permission',
    ).toBe(false);

    // ── 3. PRODUCT GAP, asserted as what actually happens, not silently
    // skipped — see the file header. Project 1 is NOT hidden on this (or any)
    // build, so it DOES appear in this brand-new user's project list; the
    // onetest case's own expectation (it must NOT appear) does not hold here.
    const projectListResponse = await freshPage.request.get(`${API_BASE}/projects/project/default/1`);
    expect(projectListResponse.ok(), 'the current-project-list route must answer').toBe(true);
    const projectList = (await projectListResponse.json()) as readonly { id?: number | string }[];
    expect(
      projectList.some((p) => String(p.id) === DEFAULT_PROJECT_ID),
      'PRODUCT GAP (no Provisioner wired anywhere in cmd/elitea-main/main.go): the support ' +
        'project is never a real hidden project on this build, so it appears in the list — ' +
        "the opposite of ELITEA-0602's step 3",
    ).toBe(true);

    // ── 4. Conversation + message creation succeed for the new identity ────
    const createResp = await freshPage.request.post(`${API_BASE}/support_assistant/conversations/`, {
      data: {},
    });
    expect(createResp.status(), 'conversation creation must succeed (201)').toBe(201);
    const created = (await createResp.json()) as { uuid?: string };
    expect(created.uuid, 'the created conversation must carry a uuid').toBeTruthy();

    const predictResp = await freshPage.request.post(
      `${API_BASE}/support_assistant/predict/${created.uuid}`,
      {
        data: {
          content: `${AUTOTEST_PREFIX}hello from a brand-new user`,
          question_id: randomUUID(),
        },
      },
    );
    expect(predictResp.status(), 'the first message must be admitted (200)').toBe(200);
    const predictBody = (await predictResp.json()) as { task_id?: string; execution_id?: string };
    expect(predictBody.task_id || predictBody.execution_id, 'the admitted turn must carry an id').toBeTruthy();
  } finally {
    await freshContext.close();
  }
});
