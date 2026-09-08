/**
 * Guardrails LIVE RELOAD — a written policy changes what the platform serves
 * on the very next request, with no restart, and it matches case-insensitively.
 *
 * Ported from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/admin/
 * test_guardrails_live_reload.py`, issue 5199), by use case:
 *
 *   GR-1 ← `TestBlockedToolkitLiveReload::test_blocked_toolkit_live_reload_case_insensitive`
 *   GR-2 ← `TestBlockedToolLiveReload::test_blocked_tool_live_reload_case_insensitive`
 *   GR-3 ← `TestSensitiveToolLiveReload::test_sensitive_tool_live_reload_case_insensitive`
 *
 * ## What is ported, and what is not
 *
 * Each legacy case is two halves. The first is "the policy is written and
 * takes effect immediately, without reloading Pylon" — that half is the
 * enhancement, and every one of its observable consequences is a Go read this
 * stack serves. The second is "an agent then fails to call a GitHub tool in
 * chat", which needs a live GitHub credential AND the full standalone stack's
 * worker and model. That half is out of scope here and is NOT left behind as
 * a permanent skip; the file says so instead.
 *
 * `admin.configuration.spec.ts`'s J34a already proves the OTHER end: that the
 * Guardrails section renders a real form with real editors for both tool maps
 * and saves. What was missing is that the saved value does anything, which is
 * exactly the defect class this repository keeps meeting — a flag that
 * persists and is read by nothing.
 *
 * ## The four consumers, and the three this file can reach
 *
 * `internal/api/v2/toolkits/guardrails.go` names them: the type list, the
 * type-schema catalogue, the two per-project tool reads, and the agent tool
 * freeze inside `internal/application/agentexecution`. The freeze runs during
 * an execution, so it is the half that needs the worker. The other three are
 * ordinary reads, and the product UI's blocked banner
 * (`features/agents/ui/ToolCard.tsx`, fed by `useBlockedToolkitTypes` from
 * `GET /elitea_core/platform_settings/prompt_lib`) is a fourth surface the
 * same row drives.
 *
 * ## Why the writer half of the platform-flag lock
 *
 * `toolkit_security` is ONE row for the whole deployment. While a type is
 * blocked it is gone from every create form in every other journey —
 * `toolkits.catalogue.spec.ts` reads exactly that catalogue and would report
 * a category with no tiles. Those journeys take the SHARED half
 * (`readsPlatformFlags`), so holding the exclusive half here keeps them out
 * of the window. `e2e/fixtures/platformFlags.ts` explains the mechanism.
 */
import { expect, test, type APIRequestContext } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  attachToolkitThroughPicker,
  createAgentThroughForm,
  createGithubToolkit,
  deleteAgent,
  deleteGithubToolkit,
  DEFAULT_PROJECT_ID,
  EMPTY_TOOLKIT_GUARDRAILS,
  setToolkitGuardrails,
  type GithubToolkitFixture,
} from '../../fixtures/api';
import { withPlatformFlagLock } from '../../fixtures/platformFlags';

/**
 * The type the legacy suite names (`TEST_TOOLKIT = "github"`).
 *
 * Kept rather than derived, because the case-insensitivity this file is about
 * is a property of a NAME: `guardrails.CanonicalKey` collapses `GitHub`,
 * `github` and `git_hub` to one key, and a type chosen at run time could not
 * be written in a deliberately different case. That the type is served at all
 * is asserted, not assumed — see GR-1's first step.
 */
const BLOCKED_TYPE = 'github';
/** The same name, written the way an operator would type it. */
const BLOCKED_TYPE_AS_TYPED = 'GitHub';
/** The banner's `getToolkitTypeLabel('github')`. */
const BLOCKED_TYPE_LABEL = 'Github';

const RUN_ID = String(Date.now()).slice(-6);

interface ServedType {
  readonly properties?: {
    readonly selected_tools?: { readonly args_schemas?: Record<string, unknown> };
  };
}

/** `GET /elitea_core/toolkits/prompt_lib/{project}` — the type-schema catalogue. */
async function readCatalogue(request: APIRequestContext): Promise<Record<string, ServedType>> {
  const response = await request.get(
    `${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`,
  );
  expect(response.status(), await response.text()).toBe(200);
  return (await response.json()) as Record<string, ServedType>;
}

/** `GET /elitea_core/toolkit_types/prompt_lib/{project}` — the type list the chooser reads. */
async function readTypeList(request: APIRequestContext): Promise<readonly string[]> {
  const response = await request.get(
    `${API_BASE}/elitea_core/toolkit_types/prompt_lib/${DEFAULT_PROJECT_ID}`,
  );
  expect(response.status(), await response.text()).toBe(200);
  const body = (await response.json()) as { rows?: readonly string[] };
  return body.rows ?? [];
}

/** The tool names one served type offers, sorted — the keys of `selected_tools.args_schemas`. */
function toolsOf(catalogue: Record<string, ServedType>, type: string): readonly string[] {
  return Object.keys(catalogue[type]?.properties?.selected_tools?.args_schemas ?? {}).sort();
}

/**
 * `blocked_toolkits` as `GET /elitea_core/platform_settings/prompt_lib`
 * publishes it — the document `features/agents`' `useBlockedToolkitTypes`
 * reads, and therefore the source of the card banner below.
 *
 * Through the `request` context and an ABSOLUTE url, not a relative `fetch`
 * inside `page.evaluate`: two of the three tests here never navigate the page
 * at all, and a relative fetch on `about:blank` has no origin to resolve
 * against.
 */
async function publishedBlockedToolkits(request: APIRequestContext): Promise<readonly string[]> {
  const response = await request.get(
    `${API_BASE}/elitea_core/platform_settings/prompt_lib`,
  );
  expect(response.status(), await response.text()).toBe(200);
  const body = (await response.json()) as { blocked_toolkits?: readonly string[] };
  return body.blocked_toolkits ?? [];
}

/**
 * The canonical comparison key both ends of this field derive —
 * `guardrails.CanonicalKey` in Go and `canonToolkitKey` in
 * `features/agents/lib/toolkitBlocklist.ts`: lowercase, then drop every
 * character that is not a letter or a digit.
 */
function canonicalToolkitKey(value: string): string {
  return value.toLowerCase().replace(/[^a-z0-9]/g, '');
}

/**
 * Whether the published document blocks this type, whatever spelling either
 * end wrote it in.
 *
 * BY CANONICAL KEY, and not by the operator's own spelling, because those are
 * two different documents and only one of them is this one. The STORE keeps
 * what was typed — the admin section GET reads `GitHub` back, and
 * `services/elitea-main/internal/api/v2/admin/guardrails_postgres_integration_test.go`
 * asserts the row survives unmangled — while `blocked_toolkits` here is a
 * projection of the MATCHER: `blockedToolkits` marshals
 * `Policy.BlockedToolkits()`
 * (`services/elitea-main/internal/api/v2/eliteacore/platform_flags.go:266-278`),
 * and `api/openapi/v2.yaml`'s own description states the contract — canonical
 * comparison keys, "for COMPARISON and never for display".
 *
 * Nothing displays this value: the card's banner labels itself from the
 * toolkit's own `type` (`features/agents/ui/ToolCard.tsx`'s
 * `getToolkitTypeLabel`), and `isToolkitTypeBlocked` canonicalises both sides
 * before comparing — `AgentToolRow.test.tsx` publishes `Git-Hub` at it on
 * purpose. So an assertion on the typed casing would pin a property no
 * consumer has and the API contract denies, which is why this asks the
 * question the product asks.
 */
async function publishedBlocks(request: APIRequestContext, toolkitType: string): Promise<boolean> {
  const key = canonicalToolkitKey(toolkitType);
  return (await publishedBlockedToolkits(request)).some(
    (entry) => canonicalToolkitKey(entry) === key,
  );
}

test.afterEach(async () => {
  const testInfo = test.info();
  /*
   * A NET, not the restore. Every test puts the policy back inside its own
   * `finally`, within the lock, so the window is as short as the assertions
   * allow. This covers the case that `finally` cannot: a test that ends on
   * its own TIMEOUT. A leaked `blocked_toolkits` entry takes a whole toolkit
   * category out of every other journey's create form, which is the failure
   * `platformFlags.ts` was written for.
   *
   * The lock is deliberately NOT taken here: the run is already in the state
   * the lock exists to avoid, and leaving the platform blocked is the worse
   * of the two outcomes.
   *
   * ONLY FOR A TEST THAT DID NOT FINISH. `fullyParallel: true` puts the three
   * tests below in three workers, so this hook used to run — unlocked, on a
   * green test — while a SIBLING was inside its own flag window, and wiped the
   * block that sibling was about to assert. GR-1 failed exactly that way: the
   * card's banner was gone because another worker's net had just cleared the
   * flags. A test that ended normally has already restored them in its
   * `finally`, so the net has nothing to do for it and every reason not to.
   */
  if (testInfo.status === testInfo.expectedStatus) return;
  await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS).catch(() => {});
});

test('GR-1: blocking a toolkit type takes effect on the next read, whatever case it was typed in (legacy test_blocked_toolkit_live_reload_case_insensitive)', async ({
  page,
}) => {
  // The lock's own arithmetic (see `admin.features.spec.ts`) plus an agent,
  // a toolkit and two full page loads.
  test.setTimeout(300_000);

  // ── the deployment serves this type to begin with ───────────────────────
  const before = await readCatalogue(page.request);
  expect(
    Object.keys(before),
    `this deployment does not serve the ${BLOCKED_TYPE} toolkit type, so nothing here can be blocked`,
  ).toContain(BLOCKED_TYPE);

  const toolkitName = `${AUTOTEST_PREFIX}gr_tk_${RUN_ID}`;
  const agentName = `${AUTOTEST_PREFIX}gr_ag_${RUN_ID}`;
  let toolkit: GithubToolkitFixture | undefined;
  let agentId = '';

  try {
    /*
     * A toolkit INSTANCE of that type. Created over the API: the github form
     * needs a stored credential to save, and this journey is about the
     * policy, not about that form (J17C.1 owns it).
     *
     * Through `createGithubToolkit`, which supplies the two settings the
     * create route requires for this type — `repository` and
     * `github_configuration` (`validateToolkitCreate`,
     * `services/elitea-main/internal/api/v2/toolkits/handler.go:987-1004`) —
     * and the placeholder credential the second one has to reference. A body
     * carrying only `selected_tools` is answered 400, which is what this line
     * used to send.
     */
    toolkit = await createGithubToolkit(page.request, DEFAULT_PROJECT_ID, toolkitName);

    /*
     * THE TYPE LIST IS ASSERTED HERE, AFTER THE INSTANCE EXISTS, and that
     * order is not incidental.
     *
     * `ListTypes` answers the STATIC `knownToolkitTypes` (datasource,
     * openapi, custom, the loaders…) merged with `SELECT DISTINCT type` over
     * the project's own `elitea_tools` rows. `github` is not in the static
     * list, so it reaches this route only because the row above exists.
     * Asserting it before the create would have failed on a correct
     * deployment — and, worse, asserting its ABSENCE after the block would
     * then have passed for that same reason rather than because the
     * guardrail fired.
     */
    expect(
      await readTypeList(page.request),
      'the type list must offer a type this project holds an instance of',
    ).toContain(BLOCKED_TYPE);

    const agent = await createAgentThroughForm(page, agentName);
    agentId = agent.agentId;
    await expect(
      page.getByTestId('agent-toolkits-section'),
      'the save must land on the agent edit page, where the Tools panel lives',
    ).toBeVisible({ timeout: 30_000 });
    await attachToolkitThroughPicker(page, toolkitName);

    const card = page.getByTestId('agent-toolkit-card').filter({ hasText: toolkitName });
    await expect(card).toBeVisible({ timeout: 30_000 });
    // Nothing is blocked yet, so the banner must be ABSENT — `.count()`, which
    // reads the DOM as it stands rather than waiting for an attach that must
    // not happen.
    await expect(page.getByText(`${BLOCKED_TYPE_LABEL} toolkit is blocked`)).toHaveCount(0);

    await withPlatformFlagLock(async () => {
      try {
        // ── the write, in the OPERATOR's casing ───────────────────────────
        await setToolkitGuardrails({
          ...EMPTY_TOOLKIT_GUARDRAILS,
          blocked_toolkits: [BLOCKED_TYPE_AS_TYPED],
        });

        /*
         * LIVE. No restart, no redeploy — the very next read is filtered.
         * Polled rather than read once, because the write and this read are
         * two requests and the assertion is about the SECOND one; a bare read
         * would be asserting request ordering.
         */
        await expect
          .poll(async () => (await readTypeList(page.request)).includes(BLOCKED_TYPE), {
            timeout: 30_000,
            message: `blocking "${BLOCKED_TYPE_AS_TYPED}" did not remove "${BLOCKED_TYPE}" from the type list`,
          })
          .toBe(false);

        // …and out of the type-SCHEMA catalogue, which is what the create
        // form renders from. Both, because they are two different filters
        // (`filterBlockedToolkitTypes` and `applyGuardrailsToCatalogue`) and
        // a fix to one has been shipped without the other before.
        expect(Object.keys(await readCatalogue(page.request))).not.toContain(BLOCKED_TYPE);

        // The platform-settings document the PRODUCT UI reads carries it too —
        // as the canonical key, which is the contract `publishedBlocks`
        // documents above.
        expect(
          await publishedBlocks(page.request, BLOCKED_TYPE_AS_TYPED),
          'the document the blocked-toolkit banner reads does not carry the block',
        ).toBe(true);

        // ── the user-visible half: the agent's toolkit card says so ───────
        await page.reload({ waitUntil: 'domcontentloaded' });
        await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });
        await expect(
          page.getByText(`${BLOCKED_TYPE_LABEL} toolkit is blocked by your organization.`),
          'the operator blocked "GitHub"; the card must say so for a toolkit stored as "github"',
        ).toBeVisible({ timeout: 30_000 });

        // The INSTANCE is still listed, deliberately — `guardrails.go` states
        // it: blocking stops a toolkit working, it does not hide the row that
        // holds its settings and its credential reference.
        await expect(
          page.getByTestId('agent-toolkit-card').filter({ hasText: toolkitName }),
        ).toBeVisible();
      } finally {
        await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
      }

      // ── and unblocking is live too ─────────────────────────────────────
      await expect
        .poll(async () => (await readTypeList(page.request)).includes(BLOCKED_TYPE), {
          timeout: 30_000,
          message: 'removing the block did not put the type back',
        })
        .toBe(true);

      await page.reload({ waitUntil: 'domcontentloaded' });
      await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });
      await expect(page.getByText(`${BLOCKED_TYPE_LABEL} toolkit is blocked`)).toHaveCount(0);
    });
  } finally {
    await deleteGithubToolkit(page.request, DEFAULT_PROJECT_ID, toolkit).catch(() => {});
    if (agentId) await deleteAgent(page.request, agentId).catch(() => {});
  }
});

test('GR-2: blocking one tool removes just that tool from its type, whatever case it was typed in (legacy test_blocked_tool_live_reload_case_insensitive)', async ({
  page,
}) => {
  test.setTimeout(210_000);

  await withPlatformFlagLock(async () => {
    /*
     * THE PRECONDITION IS READ INSIDE THE WINDOW.
     *
     * It used to be read before the lock was taken, and the catalogue it reads
     * is exactly what a SIBLING test blocks: while GR-1 holds `github` blocked,
     * this read answers a schema with no tools in it, and this test failed as
     * "the github type schema offers no tools, so there is nothing to block" —
     * a report about another test's window, not about guardrails. Inside the
     * lock the catalogue is the deployment's own.
     */
    const before = await readCatalogue(page.request);
    const tools = toolsOf(before, BLOCKED_TYPE);
    expect(
      tools.length,
      `the ${BLOCKED_TYPE} type schema offers no tools, so there is nothing to block`,
    ).toBeGreaterThan(1);

    /*
     * DERIVED from the served catalogue, then RE-CASED.
     *
     * The legacy case names `get_ISSUE` — a real tool written in a case nobody
     * stores it in, which is the whole point. Naming a literal here would make
     * this test fail the day the SDK renames a tool, and it would fail as
     * "guardrails are broken". Taking the first tool the deployment actually
     * serves and shouting it keeps the case-insensitivity and drops the
     * coupling.
     */
    const target = tools[0] as string;
    const survivor = tools[1] as string;
    const targetAsTyped = target.toUpperCase();
    expect(targetAsTyped, 'the point of this test is a DIFFERENT case').not.toBe(target);

    try {
      await setToolkitGuardrails({
        ...EMPTY_TOOLKIT_GUARDRAILS,
        blocked_tools: { [BLOCKED_TYPE_AS_TYPED]: [targetAsTyped] },
      });

      await expect
        .poll(async () => toolsOf(await readCatalogue(page.request), BLOCKED_TYPE).includes(target), {
          timeout: 30_000,
          message: `blocking "${targetAsTyped}" did not remove "${target}" from the ${BLOCKED_TYPE} schema`,
        })
        .toBe(false);

      const after = await readCatalogue(page.request);
      // ONE tool, not the type. A `blocked_tools` entry that took the whole
      // type out would satisfy the assertion above and be a different, worse
      // bug — `withoutBlockedTools` removes keys, `applyGuardrailsToCatalogue`
      // removes types, and only the first should have run.
      expect(Object.keys(after), 'blocking a TOOL must not block its TYPE').toContain(BLOCKED_TYPE);
      expect(
        toolsOf(after, BLOCKED_TYPE),
        'blocking one tool must leave its siblings alone',
      ).toContain(survivor);
    } finally {
      await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
    }

    await expect
      .poll(async () => toolsOf(await readCatalogue(page.request), BLOCKED_TYPE).includes(target), {
        timeout: 30_000,
        message: 'removing the block did not put the tool back',
      })
      .toBe(true);
  });
});

test('GR-3: a sensitive tool is stored and applied as sensitive, not as blocked (legacy test_sensitive_tool_live_reload_case_insensitive)', async ({
  page,
}) => {
  test.setTimeout(210_000);

  /*
   * THE HALF THIS STACK CAN ANSWER.
   *
   * `sensitive_tools` is read at TOOL-CALL time, by the worker, out of the
   * agent execution input — there is no read route that reports "this tool
   * would pause". So the legacy assertion (the agent asks for authorization
   * before running `get_ISSUE`) belongs to `chat.toolkit-hitl.spec.ts` and
   * the full standalone stack, and it needs a live GitHub credential on top.
   *
   * What IS assertable here, live and without a restart, is the distinction
   * the legacy suite's three cases exist to draw: `sensitive_tools` and
   * `blocked_tools` are two different maps with two different effects. A
   * sensitive tool must stay in the catalogue — an operator who marks a tool
   * "ask me first" and finds it gone has lost the tool, not gated it. The
   * two maps were one editor on the same form (J34a), and writing to the
   * wrong one is a mistake this catches.
   */
  await withPlatformFlagLock(async () => {
    // Read inside the window, for the reason GR-2 states in full: a sibling's
    // block makes this same read answer an empty schema.
    const before = await readCatalogue(page.request);
    const tools = toolsOf(before, BLOCKED_TYPE);
    expect(tools.length).toBeGreaterThan(0);
    const target = tools[0] as string;
    const targetAsTyped = target.toUpperCase();

    try {
      await setToolkitGuardrails({
        ...EMPTY_TOOLKIT_GUARDRAILS,
        sensitive_tools: { [BLOCKED_TYPE_AS_TYPED]: [targetAsTyped] },
        sensitive_action_company_name: `${AUTOTEST_PREFIX}Org`,
      });

      // The write landed under `sensitive_tools` — `setToolkitGuardrails`
      // reads the section back and fails if the server stored a different
      // map, so reaching this line is already that assertion.
      const after = await readCatalogue(page.request);
      expect(Object.keys(after), 'a sensitive tool must not block its type').toContain(BLOCKED_TYPE);
      expect(
        toolsOf(after, BLOCKED_TYPE),
        'a sensitive tool must remain callable — it is gated, not removed',
      ).toContain(target);
      // The same canonical question, asked in the negative. Spelt as a literal
      // `not.toContain("GitHub")` this could not fail: the field publishes
      // canonical keys, so `"GitHub"` is never in it whether the type is
      // blocked or not, and a write that landed under `blocked_toolkits`
      // instead of `sensitive_tools` would have passed.
      expect(
        await publishedBlocks(page.request, BLOCKED_TYPE_AS_TYPED),
        'marking a tool sensitive must not publish its type as blocked',
      ).toBe(false);
    } finally {
      await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
    }
  });
});

/*
 * OUT OF SCOPE, stated rather than skipped.
 *
 * The chat half of all three legacy cases — the agent tries the tool and is
 * refused, or pauses for authorization — needs a real GitHub credential (the
 * toolkit has to reach api.github.com for the call to be attempted at all)
 * AND the full standalone stack's worker and model. Neither is available to
 * the e2e-standalone stack these journeys run against. The equivalent shape
 * with a credential-free toolkit is already proven on the streaming project:
 * `e2e/streaming/chat.toolkit-hitl.spec.ts` drives `sensitive_tools` through
 * a real pause, over the `openapi` toolkit and the mock tool server.
 */
