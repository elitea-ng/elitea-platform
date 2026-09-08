/**
 * Journey 36: A feature flag set on Admin › Features is a flag the platform
 *             actually obeys (JRNY-036)
 *
 * ## Why this journey needs to exist at all
 *
 * A feature flag that nothing reads is the purest form of the defect unit A14
 * exists to remove. It is worse than a missing feature and worse than an empty
 * table: the operator throws a platform-wide switch, is shown a success, and
 * believes the platform changed. Nothing about the page distinguishes that from
 * a switch that works — so every assertion here is against a SECOND surface,
 * reached after a full reload, that only a wired flag can change.
 *
 * Four of this page's six sections are live, and each is proved through its own
 * consumer:
 *
 *  - **MCP Configuration** → `GET /elitea_core/platform_settings/…` reports the
 *    flag, AND `POST /elitea_core/mcp_dcr_proxy/…` answers 403. The second is
 *    the half that cannot be faked in a client: the field's own description
 *    promises the switch "removes all MCP-related functionality … including API
 *    endpoints", and before this unit the three MCP routes never asked.
 *  - **Agent Publishing** → the publish endpoint refuses. Before this unit
 *    `Publish` validated the version name, the agent type and the publish status
 *    and never consulted the guardrail, so an operator blocking publishing
 *    during an incident would have been told it was blocked while every publish
 *    kept succeeding.
 *  - **Voice Features** → `GET /elitea_core/platform_settings/…` reports both
 *    flags, and `widgets/chat`'s `VoiceButton` — mounted on `/chat` through
 *    `ChatBox`'s slot bundle — reads them. It hardcoded both as module
 *    constants until this unit, so the admin switch named the control and could
 *    not change it.
 *  - **Help Center** (`resources`) → the round trip journey 34b used to own,
 *    moved here with the section. A link saved on this page is an anchor on
 *    `/help-center`, which is issue #26's end-to-end claim.
 *
 * The other two state a server-declared reason, and their endpoints refuse.
 *
 * ## Per-engine partitioning
 *
 * There is ONE platform configuration table and `fullyParallel` is on, so
 * chromium and webkit run this file concurrently against the same rows.
 * `describe.configure({ mode: 'serial' })` orders tests within a project and
 * does nothing across them.
 *
 * The Help Center test therefore owns a different resource CARD per engine, as
 * journey 34b did.
 *
 * The MCP and publishing tests cannot be partitioned that way: there is exactly
 * one `mcp_enabled` row and one guardrail for the whole platform, and their
 * whole point is that flipping it changes the platform. Two engines flipping the
 * same switch at once would have each engine observing the other's window —
 * `mcp_enabled` read back as `true` immediately after writing `false`, or a
 * publish that succeeded because the other engine had just restored the
 * guardrail. That is a test that fails for a reason unrelated to the product,
 * which is worse than no test: it teaches the next reader to re-run rather than
 * to look.
 *
 * So they take the EXCLUSIVE half of the platform-flag lock
 * (`withPlatformFlagLock`, `e2e/fixtures/platformFlags.ts`). Playwright's own
 * `describe.serial` cannot help here, since it orders tests within a project
 * and does nothing across them.
 *
 * ## The readers (issue #519)
 *
 * The mutex used to be held by the writers only, and that is not enough. While
 * `mcp_enabled` is off the MCP surfaces are gone for EVERY journey, not just
 * for the ones that take the lock: `useIsMcpVisible()` is false, so
 * `ToolkitTypeSelector` returns null, the `/mcps` route is closed and
 * `ToolBase` stops drawing "Make tools available by MCP". Journey 17.3 and the
 * two MCP journeys of JRNY-018 failed inside that window in three CI runs and
 * in 2 local runs of 10.
 *
 * Those journeys now take the SHARED half (`readsPlatformFlags`), so the
 * window below is closed to them. The lock is one writer and many readers,
 * over the filesystem; that file's header states the protocol.
 */
import { test as adminTest, expect, request as apiRequest, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { withPlatformFlagLock } from '../../fixtures/platformFlags';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

/*
 * THIS FILE IS NOT SERIAL (issue #539).
 *
 * It used to be. `describe.configure({ mode: 'serial' })` stops the whole group
 * at the first failure, so one failed test made Playwright report the eight
 * tests after it as "did not run". That is not a pass and it is not a failure,
 * and the summary line counts it as neither. Three runs of `E2E (webkit)`
 * reported the same shape — "N failed, 8 did not run" — and the eight were
 * always the same eight. Ten journeys then told a reader about two.
 *
 * Three of these tests need an ORDER, and they keep `serial` in their own
 * describe below: journey 36g writes a Help Center card, journey 36h forges
 * writes at the same section, and journey 36j puts the card back and asserts
 * the value.
 *
 * The others need EXCLUSION, and a mode that orders tests within a project
 * cannot give it to them — chromium and webkit run this file at the same time
 * against ONE platform configuration table. Each test that writes a
 * platform-wide section takes the exclusive half of the platform-flag lock
 * instead (`withPlatformFlags` below). That lock holds across browser projects
 * and across worker processes, which is what these tests actually need.
 *
 * A failure of journey 36b now costs one result, not nine.
 */

/** The Help Center card this browser project owns. See the header. */
function ownedCard(projectName: string): {
  title: string;
  linksLabel: string;
  cardName: string;
  titleKey: string;
  linksKey: string;
} {
  return projectName === 'chromium'
    ? {
        title: 'Documentation Card Title',
        linksLabel: 'Documentation Links',
        cardName: 'Documentation',
        titleKey: 'resources_documentation_title',
        linksKey: 'resources_documentation_links',
      }
    : {
        title: 'Tutorials Card Title',
        linksLabel: 'Tutorials Links',
        cardName: 'Tutorials',
        titleKey: 'resources_tutorials_title',
        linksKey: 'resources_tutorials_links',
      };
}

function probeTitle(projectName: string): string {
  return `E2E ${projectName} handbook`;
}

function probeLink(projectName: string): { title: string; url: string } {
  return {
    title: `E2E ${projectName} link`,
    url: `https://docs.example.com/${projectName}`,
  };
}

async function openFeatures(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/features', {
    waitUntil: 'domcontentloaded',
  });
  expect(response?.status(), 'the admin SPA must serve the features route, not 404').toBeLessThan(
    400,
  );
  // MCP Configuration is the first available section, so it is what the page
  // opens on — and its presence proves the SCHEMA read was authorised.
  await expect(page.getByRole('switch', { name: 'Enable MCP' })).toBeVisible({
    timeout: 20_000,
  });
}

async function openSection(page: Page, name: string): Promise<void> {
  await page.getByRole('button', { name: new RegExp(name) }).click();
}

/*
 * The exclusive half of the platform-flag lock now lives in
 * `e2e/fixtures/platformFlags.ts`, next to the SHARED half that the journeys
 * reading these flags take (issue #519). A mutex that only the writers held
 * left every reader inside the window — see that file's header.
 */

/** Writes one section through the product's own PUT, from the page's session. */
async function putValues(
  page: Page,
  section: string,
  values: Record<string, unknown>,
): Promise<{ status: number; body: string }> {
  return page.evaluate(
    async ([sectionId, payload]) => {
      const response = await fetch(
        `/api/v2/admin/plugin_config_values/administration/${String(sectionId)}`,
        {
          method: 'PUT',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ values: payload }),
        },
      );
      return { status: response.status, body: await response.text() };
    },
    [section, values] as const,
  );
}

/**
 * Puts a platform-wide section back, and proves the platform reads it back.
 *
 * WHY THIS IS NOT "ADD A RETRY" (issue #519). This does not retry an
 * assertion and it cannot turn a failing test green: if the section is not
 * restored by the deadline the test still fails, and it now fails with the
 * SERVER'S OWN BODY in the message. What it removes is a different failure —
 * one test's single failed write leaving a platform-wide switch off for every
 * other journey in the run.
 *
 * Measured, on `mcp_enabled`: one restore answered 401, the flag stayed off,
 * and the two MCP journeys plus the toolkit journey then failed for a reason
 * that was not theirs. The file was `mode: 'serial'` then, so the eight tests
 * after the failing one were reported as "did not run" — which is not a pass
 * and is not a failure, and nothing in the output says which. #539 removed the
 * file-level serial for that reason; this restore is what stops the failure
 * that started it.
 *
 * The status alone was also not enough to say WHY. A 401 on this route has
 * two very different causes in elitea-main — a principal that is no longer
 * active, and a session cookie that was not accepted — and they carry
 * different bodies (`authentication_error` / `unauthenticated`). The body is
 * in the failure message now so the next occurrence is read once, not
 * investigated.
 *
 * `verify` reads the PRODUCT surface, not this admin route, because a PUT that
 * answers 200 and writes nothing is the defect class this whole file exists
 * for.
 */
async function restoreSection(
  page: Page,
  section: string,
  values: Record<string, unknown>,
  verify?: () => Promise<boolean>,
): Promise<void> {
  // Well inside the 30 s test budget, deliberately. A restore that ran to a
  // longer deadline could END the test by timeout, and a timed-out test loses
  // its page mid-restore — measured, `page.evaluate: Target page, context or
  // browser has been closed` on this very line. The `afterEach` net below
  // covers the case this bound gives up on.
  const deadline = Date.now() + 6_000;
  let last = { status: 0, body: 'no attempt was made' };

  for (;;) {
    last = await putValues(page, section, values);
    if (last.status === 200 && (verify === undefined || (await verify()))) return;
    if (Date.now() > deadline) break;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }

  expect(
    last.status,
    `${section} was NOT restored, so the stack is left changed for every other journey. ` +
      `Last response: ${last.status} ${last.body}`,
  ).toBe(200);
  // The PUT answered 200 and the product still disagrees — the #519 shape.
  expect(
    verify === undefined ? true : await verify(),
    `${section} answered 200 but the platform still does not read the restored value`,
  ).toBe(true);
}

/**
 * True while the RUNNING test holds, or has held, the exclusive lock.
 *
 * The net below reads it. Before #539 the file was serial, so only one of its
 * tests could run in a browser project at a time. They run in parallel now, and
 * a test that never wrote a platform-wide section must not write the defaults
 * back on its way out: another worker can be inside its own window at that
 * moment, and the write would switch MCP back on under journey 36b.
 *
 * A worker process runs one test at a time, so a module-level flag is per-test
 * state here — the same property `readsPlatformFlags` relies on.
 */
let heldPlatformFlags = false;

adminTest.beforeEach(() => {
  heldPlatformFlags = false;
});

/** Takes the exclusive platform-flag lock, and records that this test took it. */
async function withPlatformFlags<T>(body: () => Promise<T>): Promise<T> {
  // Set BEFORE the lock is taken. A test that fails while it waits wrote
  // nothing, so the net costs one wasted restore; a test that fails after the
  // lock is taken and is NOT recorded leaves the platform switched off.
  heldPlatformFlags = true;

  /*
   * THE BUDGET MUST COVER THE QUEUE (issue #539).
   *
   * A test's 30 s default budget starts when the test starts, and the wait for
   * this lock is inside it. While the file was serial, only one of its tests
   * could be waiting per browser project, so the queue was one window long.
   * Parallel, five of them can ask at once per project, and a test that waits
   * behind four windows would fail on its own clock — a "timed out" result that
   * says nothing about the product, and one that loses the page mid-restore.
   * The measured harm of that is written on the `afterEach` net below.
   *
   * 210 s is the arithmetic of the lock, not a round number: `platformFlags.ts`
   * waits up to STALE_MS (90 s) to take the writer, then up to STALE_MS again
   * for the readers to drain, and the assertions after that need about 30 s.
   * Both 90 s bounds are the degenerate case — a token left by a run that was
   * killed — and neither can wedge the suite, because the lock breaks a stale
   * token and continues.
   */
  adminTest.setTimeout(210_000);
  return withPlatformFlagLock(body);
}

/**
 * The platform-wide sections this file writes, and the values the seed leaves
 * behind. Used by the net below.
 */
const PLATFORM_DEFAULTS: readonly { section: string; values: Record<string, unknown> }[] = [
  { section: 'mcp_configuration', values: { mcp_enabled: true } },
  { section: 'agent_publishing', values: { is_publish_blocked: false } },
  {
    section: 'voice_features',
    values: {
      vite_voice_features_enabled: true,
      vite_voice_features_temporarily_disabled: false,
    },
  },
];

/**
 * A NET, not the restore (issue #519).
 *
 * Each test that flips a platform-wide switch puts it back in its own
 * `finally`, inside the lock, so the window stays as short as the assertions
 * allow. This runs afterwards, and it exists for the one case the `finally`
 * cannot cover: a test that ends on its own TIMEOUT loses its page while the
 * restore is still running. Measured on this file — `page.evaluate: Target
 * page, context or browser has been closed`, raised from inside the restore,
 * with `mcp_enabled` left false for the rest of the run.
 *
 * Playwright gives hooks their own budget and runs them after a timeout, and
 * this hook opens its OWN request context, so neither the dead page nor the
 * test's spent clock can stop it.
 *
 * Only after a test that did not pass AND took the exclusive lock: a passing
 * test has already restored, a test that never entered the window wrote none of
 * these rows, and writing them on every test would put a second writer on a
 * platform-wide row for no reason. Since #539 that second condition also keeps
 * one test's teardown out of another test's window — see `withPlatformFlags`.
 * The lock is not taken here — the run is already in the state the lock exists
 * to avoid, and leaving the platform switched off is the worse of the two.
 */
// Playwright requires the fixture parameter to be an object pattern, and this
// hook takes no fixture.
// eslint-disable-next-line no-empty-pattern
adminTest.afterEach(async ({}, testInfo) => {
  if (testInfo.status === 'passed' || !heldPlatformFlags) return;
  const api = await apiRequest.newContext({
    baseURL: BASE_URL,
    storageState: STORAGE_STATE.admin,
  });
  try {
    for (const { section, values } of PLATFORM_DEFAULTS) {
      await api.put(`/api/v2/admin/plugin_config_values/administration/${section}`, {
        data: { values },
      });
    }
  } finally {
    await api.dispose();
  }
});

/* ── the page itself ───────────────────────────────────────────────────── */

adminTest(
  'J36: the page opens on a section this deployment can actually serve',
  async ({ page }) => {
    await openFeatures(page);

    await expect(page.getByText('Failed to load the feature sections.')).toHaveCount(0);
    // All six of the reference's sections are offered, live or not. Omitting the
    // unavailable ones would read as a page that lost features rather than a
    // platform that does not have them.
    for (const section of [
      'MCP Configuration',
      'Agent Publishing',
      'Skill Publishing',
      'Help Center',
      'Support Assistant',
      'Voice Features',
    ]) {
      await expect(page.getByRole('button', { name: new RegExp(section) })).toBeVisible();
    }
    /*
     * THE MARKER IS ASSERTED AGAINST THE SERVER'S OWN ANSWER, not against a
     * standing assumption that some section is withheld.
     *
     * This line used to read `expect(getByText('Not available here').first())
     * .toBeVisible()`, which was true while `skill_publishing` and
     * `support_assistant` were both withheld. #585 and #588 gave each of them a
     * consumer, so the marker now renders zero times and the unconditional
     * assertion fails — not because the page regressed, but because the product
     * caught up with it.
     *
     * The property worth holding is the CORRESPONDENCE: the sidebar marks
     * exactly those sections the server declares unavailable, however many that
     * is. Asserting a count of zero when the server withholds nothing is a real
     * assertion, not a vacuous one — it would catch the page marking a live
     * section as unavailable.
     */
    const withheldCount = await page.evaluate(async () => {
      const response = await fetch('/api/v2/admin/plugin_config_schemas/administration', {
        credentials: 'include',
      });
      const body = (await response.json()) as {
        sections?: { page?: string; unavailable_reason?: string }[];
      };
      return (body.sections ?? []).filter(
        (section) => section.page === 'features' && Boolean(section.unavailable_reason),
      ).length;
    });
    await expect(page.getByText('Not available here')).toHaveCount(withheldCount);

    await checkA11y(page);
  },
);

/* ── MCP: the switch, and the API it closes ────────────────────────────── */

adminTest(
  'J36b: switching MCP off is read by the platform AND closes the API',
  async ({ page }) => {
    await openFeatures(page);
    await withPlatformFlags(async () => {
      try {
        const settingsBefore = await page.evaluate(async () => {
          const response = await fetch('/api/v2/elitea_core/platform_settings/prompt_lib', {
            credentials: 'include',
          });
          return (await response.json()) as Record<string, unknown>;
        });
        /*
         * INSIDE the try, deliberately (issue #519). It used to sit above it,
         * and that one line is what turned a single failed restore into a red
         * suite.
         *
         * `mcp_enabled` is ONE platform-wide row. When the restore below
         * failed once, the flag stayed off; the retry then failed on THIS
         * assertion, before entering the try, so the `finally` never ran and
         * the flag was never put back. Every remaining MCP-reading journey in
         * the run — the two MCP ones, the toolkit one — failed for a reason
         * that had nothing to do with them, and the eight tests after this one
         * were reported "did not run" — the file was serial then (#539).
         *
         * From inside the try, a retry that finds the flag off still restores
         * it on its way out.
         */
        expect(settingsBefore.mcp_enabled, 'the probe starts from MCP enabled').toBe(true);

        await page.getByRole('switch', { name: 'Enable MCP' }).click();
        const [saved] = await Promise.all([
          page.waitForResponse(
            (r) =>
              r.url().includes('/admin/plugin_config_values/administration/mcp_configuration') &&
              r.request().method() === 'PUT',
          ),
          page.getByRole('button', { name: 'Save' }).click(),
        ]);
        expect(saved.status(), 'the write must be authorised server-side').toBe(200);

        // A FULL RELOAD, not a cache read: the assertion a PUT that answers 200 and
        // writes nothing cannot pass.
        await page.reload({ waitUntil: 'domcontentloaded' });
        await page.goto(BASE_URL + '/admin/app/features', {
          waitUntil: 'domcontentloaded',
        });
        await expect(page.getByRole('switch', { name: 'Enable MCP' })).not.toBeChecked();

        // …and the platform reads it. This is the flag apps/elitea-web's four
        // `useIsMcpVisible` hooks and its `/mcps` route gate on.
        const settings = await page.evaluate(async () => {
          const response = await fetch('/api/v2/elitea_core/platform_settings/prompt_lib', {
            credentials: 'include',
          });
          return (await response.json()) as Record<string, unknown>;
        });
        expect(settings.mcp_enabled, 'the product flag must follow the admin switch').toBe(false);
        expect(
          settings.mcp_in_menu_enabled,
          'MCP off implies out of the menu — a menu entry whose endpoints 403 is worse than none',
        ).toBe(false);

        // The half that cannot be done in a client. A switch that only hides
        // buttons leaves every one of these routes open to anyone with the URL,
        // while telling the operator they closed them.
        const proxied = await page.evaluate(async () => {
          const response = await fetch('/api/v2/elitea_core/mcp_dcr_proxy/1', {
            method: 'POST',
            credentials: 'include',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              registration_endpoint: 'https://example.invalid/register',
            }),
          });
          return { status: response.status, body: await response.text() };
        });
        expect(proxied.status, 'the MCP API must refuse while the master switch is off').toBe(403);
        expect(proxied.body).toContain('MCP exposure is disabled');
      } finally {
        // Restored inside the test, so a later failure cannot leave the stack
        // with MCP switched off for every other journey — and restored through
        // `restoreSection`, which keeps trying for the length of the lock and
        // then reads the PRODUCT flag back. See that function's note.
        await restoreSection(page, 'mcp_configuration', { mcp_enabled: true }, async () => {
          const settings = await page.evaluate(async () => {
            const response = await fetch('/api/v2/elitea_core/platform_settings/prompt_lib', {
              credentials: 'include',
            });
            return (await response.json()) as Record<string, unknown>;
          });
          return settings.mcp_enabled === true;
        });
      }
    });
  },
);

/* ── publishing: the guardrail ─────────────────────────────────────────── */

adminTest('J36c: blocking publishing is enforced by the publish endpoint', async ({ page }) => {
  await openFeatures(page);
  await openSection(page, 'Agent Publishing');
  await expect(page.getByRole('switch', { name: 'Block Agent Publishing' })).toBeVisible();

  await withPlatformFlags(async () => {
    try {
      const blocked = await putValues(page, 'agent_publishing', {
        is_publish_blocked: true,
        publish_whitelist_project_ids: [],
      });
      expect(blocked.status).toBe(200);

      // The publish route is asked for a version that may not exist; what matters
      // is that the answer is the GUARDRAIL's 403 and not a 404 about the version,
      // because the guardrail is checked before anything is looked up — a refusal
      // that arrives after validation leaks which version ids exist to a caller
      // the platform has just decided may not publish at all.
      const refused = await page.evaluate(async () => {
        const response = await fetch('/api/v2/elitea_core/publish/prompt_lib/1/99999', {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ version_name: 'e2e-probe' }),
        });
        return { status: response.status, body: await response.text() };
      });
      expect(refused.status, 'the publish guardrail must be enforced server-side').toBe(403);
      expect(refused.body).toContain('publishing is blocked');
    } finally {
      // Same reason as J36b's restore: this guardrail is platform-wide, and a
      // restore that fails silently blocks every publish in the rest of the
      // run. See `restoreSection`.
      await restoreSection(page, 'agent_publishing', { is_publish_blocked: false });
    }
  });
});

adminTest(
  'J36d: a field the server cannot honour is read-only and refuses its write',
  async ({ page }) => {
    await openFeatures(page);
    await openSection(page, 'Agent Publishing');

    // Rendered, read-only, with the server's own sentence — not hidden, which
    // would read as a page that lost a control, and not live, which would let a
    // save be attempted that the server refuses.
    const field = page.getByTestId('admin-config-field-unavailable-publish_validation_rules');
    await expect(field).toBeVisible();
    await expect(field.getByRole('textbox')).toBeDisabled();
    await expect(field).toContainText('no AI evaluator');

    /*
     * THE WRITES TAKE THE EXCLUSIVE LOCK (issue #539).
     *
     * Both address `agent_publishing`, and journey 36c holds that section
     * switched ON for the length of its window. The accepted write below clears
     * `is_publish_blocked`, so without the lock it can land inside that window
     * and journey 36c reads a publish that succeeds — a failure that belongs to
     * neither test. The file-level `serial` used to hide this by ordering the
     * two within a project; it never ordered them across projects, and it is
     * gone.
     *
     * The page assertions above stay outside the window, which keeps the window
     * as short as the writes.
     */
    await withPlatformFlags(async () => {
      // Forged, because the page offers no way to send it.
      const refused = await putValues(page, 'agent_publishing', {
        publish_validation_rules: 'reject anything mentioning production',
      });
      // 400, not 501: the SECTION is implemented. What is wrong is the field,
      // and the message names it.
      expect(refused.status).toBe(400);
      expect(refused.body).toContain('publish_validation_rules');

      // The sibling fields of the same section still save.
      const accepted = await putValues(page, 'agent_publishing', {
        is_publish_blocked: false,
      });
      expect(accepted.status).toBe(200);
    });
  },
);

adminTest(
  'J36e: an array element of the wrong type is refused, not stored and ignored',
  async ({ page }) => {
    await openFeatures(page);

    // The exclusive lock, for the reason journey 36d states: this is the same
    // platform-wide section journey 36c switches on, and a refusal is only
    // proof of a refusal while nobody else is writing the section (#539).
    await withPlatformFlags(async () => {
      // Both consumers type-assert their elements and SKIP what does not match,
      // so a wrongly-typed entry would be stored, echoed back by the GET,
      // rendered in the form — and do nothing. That is "saves into a void" one
      // level down.
      const refusedCategory = await putValues(page, 'agent_publishing', {
        agent_categories: [{ name: 'Security Review' }],
      });
      expect(refusedCategory.status).toBe(400);
      expect(refusedCategory.body).toContain('agent_categories');

      const refusedProject = await putValues(page, 'agent_publishing', {
        publish_whitelist_project_ids: ['1'],
      });
      expect(refusedProject.status).toBe(400);
      expect(refusedProject.body).toContain('publish_whitelist_project_ids');
    });
  },
);

adminTest(
  'J36f: the voice switches reach the platform settings the chat button reads',
  async ({ page }) => {
    await openFeatures(page);
    await openSection(page, 'Voice Features');
    await expect(page.getByRole('switch', { name: 'Voice Features Enabled' })).toBeVisible();

    const settings = async (): Promise<Record<string, unknown>> =>
      page.evaluate(async () => {
        const response = await fetch('/api/v2/elitea_core/platform_settings/prompt_lib', {
          credentials: 'include',
        });
        return (await response.json()) as Record<string, unknown>;
      });

    await withPlatformFlags(async () => {
      try {
        const [saved] = await Promise.all([
          page.waitForResponse(
            (r) =>
              r.url().includes('/admin/plugin_config_values/administration/voice_features') &&
              r.request().method() === 'PUT',
          ),
          (async () => {
            await page
              .getByRole('switch', { name: 'Disable Voice Controls but Keep Them Visible' })
              .click();
            await page.getByRole('button', { name: 'Save' }).click();
          })(),
        ]);
        expect(saved.status()).toBe(200);

        await page.reload({ waitUntil: 'domcontentloaded' });
        await openFeatures(page);
        await openSection(page, 'Voice Features');
        await expect(
          page.getByRole('switch', { name: 'Disable Voice Controls but Keep Them Visible' }),
        ).toBeChecked();

        // The flag the product reads. `widgets/chat`'s VoiceButton renders it as
        // a visible-but-disabled control with an admin tooltip; before this unit
        // it read a module constant hardcoded to `false`.
        const after = await settings();
        expect(after.voice_features_temporarily_disabled).toBe(true);
        expect(
          after.voice_features_enabled,
          'temporarily disabling must not HIDE the control',
        ).toBe(true);

        /*
         * ── THE CHAT SURFACE, not just the row ────────────────────────────
         *
         * Ported from the legacy public suite's
         * `tests/ui/voice/test_voice_configuration.py::
         * TestVoiceConfiguration::test_voice_settings_not_visible_by_default`
         * (TC5, regression for bug 5235). Everything above this point proves
         * the WRITE reaches `platform_settings`; the legacy case is about
         * what a user sees, and the defect unit A14 removed was precisely a
         * flag that persisted and changed nothing.
         *
         * Two facts, on the real chat page:
         *
         *  1. THE READ-OUT MINI PLAYER IS NOT THERE BY DEFAULT. That is the
         *     legacy regression, and here it is structural rather than
         *     stateful: `features/chat-input`'s `VoiceMiniPlayer` /
         *     `VoiceControlButton` have no render site in this app at all
         *     (their own module docs say so), so the TTS controls cannot
         *     appear on a chat surface that nobody asked to read aloud.
         *  2. THE MICROPHONE OBEYS THE SWITCH. `widgets/chat`'s `VoiceButton`
         *     is the one MOUNTED voice control, reached through ChatBox's
         *     slot bundle; `temporarily disabled` must leave it visible and
         *     inert, and `enabled: false` must remove it.
         *
         * `.count()` for both absences: it reads the DOM as it stands rather
         * than waiting for an attach that must never happen.
         */
        /*
         * ── THE ENGINE THE CONTROL NEEDS, MADE THE SAME ON BOTH BROWSERS ──
         *
         * `VoiceButton` returns null on `!isSupported` BEFORE it reads either
         * platform flag (`src/widgets/chat/ui/chat-button/VoiceButton.tsx:277`).
         * With no project ASR model configured, `isSupported` is the client
         * hook's — whether `window.SpeechRecognition` or
         * `webkitSpeechRecognition` exists at all
         * (`src/features/chat-input/lib/hooks/useSpeechRecognition.ts:82-105`).
         * Playwright's Chromium ships `webkitSpeechRecognition`; its WebKit
         * ships neither, so the mic had no render site there and the
         * assertions below failed as "the switch did not reach the chat
         * surface" on one engine only.
         *
         * A stub, not a `browserName` branch: the OPERATOR SWITCH is what this
         * journey is about, and gating half of it on the engine would leave
         * WebKit asserting nothing about it for ever. `settings.voice.spec.ts`
         * installs a `speechSynthesis` stub for the same reason, on the same
         * argument.
         *
         * `addInitScript` before the navigation, because the hook probes in its
         * first effect. Nothing here ever clicks the control — it is asserted
         * DISABLED — so a bare constructor is the whole of what is needed.
         */
        await page.addInitScript(() => {
          if ('SpeechRecognition' in window || 'webkitSpeechRecognition' in window) return;
          class AutotestSpeechRecognition {
            continuous = false;
            interimResults = false;
            lang = 'en-US';
            onresult: unknown = null;
            onerror: unknown = null;
            onend: unknown = null;
            start(): void {}
            stop(): void {}
            abort(): void {}
          }
          Object.defineProperty(window, 'webkitSpeechRecognition', {
            configurable: true,
            value: AutotestSpeechRecognition,
          });
        });

        await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
        await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

        const microphone = page.getByRole('button', { name: 'start voice input' });
        await expect(
          microphone,
          'temporarily disabling must leave the control on screen',
        ).toBeVisible({ timeout: 20_000 });
        await expect(microphone).toBeDisabled();
        await expect(page.getByTestId('chat-voice-mini-player')).toHaveCount(0);

        // Now HIDE it, which is the other switch in the same section. The stub
        // is still installed, so an absent control here is the FLAG's doing and
        // not the engine's — which is exactly what makes this assertion mean
        // something on both browsers.
        const hidden = await putValues(page, 'voice_features', {
          vite_voice_features_enabled: false,
          vite_voice_features_temporarily_disabled: true,
        });
        expect(hidden.status, hidden.body).toBe(200);

        await page.reload({ waitUntil: 'domcontentloaded' });
        await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
        await expect
          .poll(async () => page.getByRole('button', { name: 'start voice input' }).count(), {
            timeout: 20_000,
            message: 'the voice control survived an operator switching voice features off',
          })
          .toBe(0);
        await expect(page.getByTestId('chat-voice-mini-player')).toHaveCount(0);
      } finally {
        // Same reason as J36b's restore — see `restoreSection`.
        await restoreSection(page, 'voice_features', {
          vite_voice_features_enabled: true,
          vite_voice_features_temporarily_disabled: false,
        });
      }
    });
  },
);

/* ── the Help Center round trip, moved here with its section ───────────── */

adminTest.describe('the Help Center round trip', () => {
  /*
   * SERIAL, AND ONLY THESE THREE (issue #539).
   *
   * These three share the `resources` section and they run in an order.
   * Journey 36g writes the card, journey 36h forges writes at the same
   * section, and journey 36j puts the card back and reads the value. Journey
   * 36j is the assertion that the write is reversible, so it says nothing at
   * all about a journey 36g that did not run.
   *
   * `serial` orders them within a browser project. The two projects still run
   * this group at the same time, and what keeps them apart is the OWNED CARD
   * — see `ownedCard` and the file header.
   *
   * The rest of the file stays parallel. That is what #539 asks for: the group
   * that stops at its first failure must be the smallest group that really
   * needs an order.
   */
  adminTest.describe.configure({ mode: 'serial' });

  adminTest(
    'J36g: a Help Center link saved here survives a reload and reaches /help-center',
    async ({ page }, testInfo) => {
      const card = ownedCard(testInfo.project.name);
      const link = probeLink(testInfo.project.name);

      await openFeatures(page);
      // RETRY POISONING. The seed clears this card, so on a fresh stack the
      // baseline is the schema default. But a first attempt that fails AFTER
      // its PUT leaves the probe title in the platform-wide `resources`
      // section, and Playwright's retry starts this journey again with the
      // section as the failure left it: the baseline assertion below then
      // fails on every retry ("Tutorials" expected, "E2E webkit handbook"
      // received — run 33765076425), and journeys 36h/36j never run because
      // the group is serial. Journey 36j is the restore, and it sits AFTER
      // this one, so nothing else puts the card back between attempts.
      // Restore the OWNED card only — the other browser project's card is its
      // own, and this write must not touch it — and reload so the form shows
      // the restored value rather than what it fetched a moment ago.
      await restoreSection(page, 'resources', {
        [card.titleKey]: card.cardName,
        [card.linksKey]: [],
      });
      await page.reload({ waitUntil: 'domcontentloaded' });
      await openFeatures(page);
      await openSection(page, 'Help Center');

      const title = page.getByRole('textbox', { name: card.title });
      // From here the baseline IS the schema default, on a fresh stack and on a
      // retry alike. Starting from "already set to the probe value" would prove
      // nothing about the write.
      await expect(title).toHaveValue(card.cardName);
      await title.fill(probeTitle(testInfo.project.name));

      // Every link interaction is scoped to the OWNED card's editor: both engines
      // run this file at once against one platform-wide table, so `.last()` over
      // the page's link fields would sometimes be the other engine's row.
      const editor = page.getByTestId(`admin-config-links-${card.linksKey}`);
      await editor.getByRole('button', { name: `Add link — ${card.linksLabel}` }).click();
      await editor.getByRole('textbox', { name: 'URL' }).last().fill(link.url);
      await editor.getByRole('textbox', { name: 'Title', exact: true }).last().fill(link.title);

      const [saved] = await Promise.all([
        page.waitForResponse(
          (r) =>
            r.url().includes('/admin/plugin_config_values/administration/resources') &&
            r.request().method() === 'PUT',
        ),
        page.getByRole('button', { name: 'Save' }).click(),
      ]);
      expect(saved.status(), 'the configuration write must be authorised server-side').toBe(200);

      await page.reload({ waitUntil: 'domcontentloaded' });
      await openFeatures(page);
      await openSection(page, 'Help Center');
      await expect(page.getByRole('textbox', { name: card.title })).toHaveValue(
        probeTitle(testInfo.project.name),
      );

      // …and the product reads it, from a page the section's move did not touch:
      // the Help Center calls the public `prompt_lib` route, which has no notion of
      // which admin page authored the row. This is issue #26's end-to-end claim,
      // and the proof that moving the section did not break it.
      await page.goto(BASE_URL + '/app/help-center', {
        waitUntil: 'domcontentloaded',
      });
      await expect(page.getByRole('link', { name: link.title })).toHaveAttribute('href', link.url, {
        timeout: 20_000,
      });
    },
  );

  adminTest(
    'J36h: the server refuses a link URL that would run in a reader’s browser',
    async ({ page }, testInfo) => {
      await openFeatures(page);
      const card = ownedCard(testInfo.project.name);

      // Forged, not typed. The form warns about the scheme, so typing it would only
      // prove the form does — and a client-side check is not a boundary.
      const refused = await putValues(page, 'resources', {
        [card.linksKey]: [{ title: 'Docs', url: 'javascript:alert(document.cookie)' }],
      });
      expect(
        refused.status,
        'a link scheme that executes in every reader’s browser must be refused by the SERVER',
      ).toBe(400);
      expect(refused.body).toContain('http or https');

      // A caller that believes it set something the schema does not declare has a
      // wrong model of the system, and a 200 would confirm it.
      const unknown = await putValues(page, 'resources', {
        resources_backdoor: 'x',
      });
      expect(unknown.status).toBe(400);
      expect(unknown.body).toContain('resources_backdoor');

      // And nothing was stored: the Help Center must not be carrying it.
      await page.goto(BASE_URL + '/app/help-center', {
        waitUntil: 'domcontentloaded',
      });
      await expect(page.getByRole('link', { name: 'Docs' })).toHaveCount(0);
    },
  );

  adminTest(
    'J36j: the probe values are restored so the run is repeatable',
    async ({ page }, testInfo) => {
      await openFeatures(page);
      await openSection(page, 'Help Center');
      const card = ownedCard(testInfo.project.name);

      await page.getByRole('textbox', { name: card.title }).fill(card.cardName);

      const editor = page.getByTestId(`admin-config-links-${card.linksKey}`);
      const remove = editor.getByRole('button', { name: /^Remove link/ });
      if ((await remove.count()) > 0) {
        await remove.last().click();
      }

      const [saved] = await Promise.all([
        page.waitForResponse(
          (r) =>
            r.url().includes('/admin/plugin_config_values/administration/resources') &&
            r.request().method() === 'PUT',
        ),
        page.getByRole('button', { name: 'Save' }).click(),
      ]);
      expect(saved.status()).toBe(200);

      await page.reload({ waitUntil: 'domcontentloaded' });
      await openFeatures(page);
      await openSection(page, 'Help Center');
      await expect(page.getByRole('textbox', { name: card.title })).toHaveValue(card.cardName);
    },
  );
});

/* ── the sections with no consumer ─────────────────────────────────────── */

adminTest(
  'J36i: a section with no consumer states its reason and refuses its write; a live one saves',
  async ({ page }) => {
    await openFeatures(page);

    /*
     * THE WITHHELD SECTIONS ARE DISCOVERED, NOT NAMED.
     *
     * This loop used to hardcode two: `Skill Publishing` ("no publish endpoint,
     * no catalog, no categories") and `Support Assistant` ("the widget has no
     * render site"). Two branches then closed one gap each — #585 built the
     * skill publishing pipeline, #588 built the support assistant — and each
     * correctly deleted the OTHER name. Both were right when written; merged,
     * neither name is left, and a hardcoded empty list is a test that asserts
     * nothing and passes.
     *
     * Reading the schema keeps the assertion — "a withheld section states its
     * reason and refuses its write" — true for however many sections are
     * withheld, including none. Against a real server there is no fixture to
     * stand one in, so zero is reported rather than passed over in silence.
     */
    const withheld = await page.evaluate(async () => {
      const response = await fetch('/api/v2/admin/plugin_config_schemas/administration', {
        credentials: 'include',
      });
      const body = (await response.json()) as {
        sections?: { id: string; title: string; page?: string; unavailable_reason?: string }[];
      };
      return (body.sections ?? [])
        .filter((section) => section.page === 'features' && Boolean(section.unavailable_reason))
        .map((section) => ({ id: section.id, title: section.title }));
    });

    if (withheld.length === 0) {
      // Not a failure: every Features section having a consumer is the goal.
      // Logged so a reader of the run can tell "nothing to check" from
      // "checked and fine".
      // eslint-disable-next-line no-console -- the whole point is that this is visible in the run log
      console.log('J36i: no Features section is withheld; the refusal half of this journey is inert');
    }

    for (const section of withheld) {
      await openSection(page, section.title);
      const notice = page.getByTestId('admin-features-unavailable');
      await expect(notice).toBeVisible();
      // The reason must name the actual obstacle, so an operator can tell
      // "this platform cannot do that" from "that is switched off".
      await expect(notice).not.toHaveText('');
      await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
    }

    for (const section of withheld.map((entry) => entry.id)) {
      const refused = await putValues(page, section, { anything: true });
      expect(refused.status, `${section} must refuse its write, not accept and discard it`).toBe(
        501,
      );
    }

    // Skill Publishing used to be in both lists above. It is LIVE now — the
    // publish endpoint, the public catalog and the categories route all exist
    // (services/elitea-main/internal/api/v2/skillpublish) — so the assertion
    // that matters is the opposite one: the section renders a form, and its
    // write is accepted rather than refused.
    await openSection(page, 'Skill Publishing');
    await expect(page.getByTestId('admin-features-unavailable')).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Save' })).toBeVisible();
    const accepted = await putValues(page, 'skill_publishing', {
      is_skill_publish_blocked: false,
    });
    expect(accepted.status, 'skill_publishing is implemented and must accept its write').toBe(200);

    await checkA11y(page);
  },
);
