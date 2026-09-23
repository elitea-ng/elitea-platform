/**
 * Wave-1 tail package T1a-toolkits — `toolkits-credentials` folder, the
 * "Credential Configuration Change" warning modal (ELITEA-1092/1094/1099).
 *
 * PRODUCT GAP, confirmed by wiring, not by a flaky UI probe: the modal's own
 * gate — `useCredentialWarning.hooks.ts`'s `checkBeforeSave` — only opens
 * when `!isCreating && isTeamProject && hasCredentialConfigChanged(...)`.
 * `isTeamProject` is an OPTIONAL `ToolkitForm` prop that defaults to `false`
 * (`ToolkitForm.types.ts`'s `DEFAULT_TOOLKIT_FORM_PROPS`), and NEITHER of
 * `<ToolkitForm>`'s two real callers — `ConfigurationTab.tsx` (the toolkit
 * EDIT screen `EditToolkit.tsx` renders) and `CreateToolkit.tsx` — ever
 * passes it (grepped: `isTeamProject` appears nowhere in either file, or in
 * `ToolkitEditorParts.tsx` between them). So `checkBeforeSave` always
 * short-circuits to `true` in the shipped app: Save proceeds straight to the
 * PUT, for EVERY project type, EVERY toolkit type, and EVERY user — the
 * modal component (`CredentialWarningModal.tsx`) is real and unit-tested,
 * but its own gate is permanently closed. This is the SAME class of defect
 * `toolkits.p13-credential-warnings.spec.ts` found for a related family
 * (a validation slot nobody ever fills) — here the caller-side prop is
 * simply never threaded, not a hardcoded `false` inside a shared component.
 *
 * The three legacy ids describe the same missing gate from three angles —
 * ELITEA-1092 (Discard reverts), ELITEA-1094 (the AUTHOR should see it),
 * ELITEA-1099 (a non-author team member should too, regression #4649) — and
 * none of the three can be true while the gate never opens: there is no
 * author/non-author branch anywhere in this code for #4649 to regress
 * against, so one demonstration, on a genuine TEAM project (a scratch
 * project, never project 1) with a `report_portal` toolkit stands for all three.
 *
 * `report_portal` (not `aha`, not `figma`): a real credential-check probe
 * (`toolkit_check.go`'s `toolkitCheckProbes`) was prototyped for `aha` while
 * investigating #920 and reverted (see `toolkits.aha.spec.ts`'s own AHA-10
 * doc comment for the whole story: it broke this file's own harness,
 * `toolkits.aha.spec.ts`'s AHA-9, and `toolkits.tool-groups.spec.ts` — a
 * stray `aha`-with-a-probe here would confound the gate under test with
 * `unreachable` blocking Save outright), so a type with no probe in that map
 * is used instead of relying on `aha` staying gapped forever. `figma` was
 * tried first and rejected too: its OWN settings schema requires a
 * `pgvector_configuration` (a second, indexing-storage credential) and a
 * `global_limit` this fixture never seeds, so the FORM itself loads invalid
 * ("Field is required") and Save never even reaches `checkBeforeSave` at
 * all — confounding the gate under test a second, different way. Testing
 * types (`report_portal`, `testio`) have no such baggage: `selected_tools` +
 * their own one `_configuration` field, nothing else required. It carries
 * the same generic `ToolBase` credential-picker field shape
 * (`report_portal_configuration`, plain `project`/`endpoint`/`api_key`
 * fields — no `base_url` needed at all), and the credential change goes
 * straight to a successful PUT once fixed, with the modal intercepting
 * first.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  gotoAppRoute,
} from '../../fixtures/api';
import { ensureProjectSelected } from '../../fixtures/project';
import { readsPlatformFlags } from '../../fixtures/platformFlags';
import { createScratchProject, deleteScratchProject, type ScratchProject } from '../../fixtures/scratchProject';

readsPlatformFlags(test);

const PLACEHOLDER_ENDPOINT = 'https://autotest-cwm.invalid.example';
const PLACEHOLDER_API_KEY = 'autotest-placeholder-cwm-key';

interface ReportPortalFixture {
  readonly toolkitId: string;
  readonly credentialId: string;
}

async function seedReportPortalCredential(request: APIRequestContext, projectId: string, title: string): Promise<string> {
  const response = await request.post(`${API_BASE}/configurations/configurations/${projectId}`, {
    data: {
      type: 'report_portal',
      elitea_title: title,
      label: title,
      shared: false,
      data: { project: title, endpoint: PLACEHOLDER_ENDPOINT, api_key: PLACEHOLDER_API_KEY },
    },
  });
  expect(response.ok(), `seeding a report_portal credential: ${await response.text()}`).toBe(true);
  const body = (await response.json()) as { id?: string | number };
  return String(body.id ?? '');
}

/** Creates a `report_portal` credential, then a `report_portal` toolkit referencing it — the credential must exist first, or the toolkit POST answers `configuration_not_found`. */
async function createReportPortalToolkit(request: APIRequestContext, projectId: string, toolkitName: string, credentialTitle: string): Promise<ReportPortalFixture> {
  const credentialId = await seedReportPortalCredential(request, projectId, credentialTitle);
  const created = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name: toolkitName,
      type: 'report_portal',
      settings: { report_portal_configuration: { elitea_title: credentialTitle, private: false }, selected_tools: [] },
    },
  });
  expect(created.status(), `createReportPortalToolkit: ${await created.text()}`).toBe(201);
  const body = (await created.json()) as { id?: string | number };
  const toolkitId = String(body.id ?? '');
  expect(toolkitId, 'createReportPortalToolkit must answer an id').not.toBe('');
  return { toolkitId, credentialId };
}

async function deleteReportPortalFixture(request: APIRequestContext, projectId: string, fixture: ReportPortalFixture | undefined, secondCredentialId: string): Promise<void> {
  if (fixture !== undefined) {
    await request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${fixture.toolkitId}`).catch(() => {});
    await request.delete(`${API_BASE}/configurations/configuration/${projectId}/${fixture.credentialId}`).catch(() => {});
  }
  if (secondCredentialId !== '') {
    await request.delete(`${API_BASE}/configurations/configuration/${projectId}/${secondCredentialId}`).catch(() => {});
  }
}

async function gotoToolkit(page: Page, projectName: string, toolkitId: string): Promise<void> {
  // `gotoAppRoute`, not `page.goto`: landing on `/app/` routes the app on to
  // its default screen, and the second navigation below is aborted by that one
  // when it lands late — `Navigation to "/app/toolkits/all/1" is interrupted
  // by another navigation to "/app/chat"`, measured on webkit. See the helper.
  await gotoAppRoute(page, `${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
  await ensureProjectSelected(page, projectName);
  await gotoAppRoute(page, `${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toBeVisible({ timeout: 20_000 });
}

function reportPortalConfigPicker(page: Page) {
  return page.getByRole('combobox', { name: /Report Portal Configuration/i });
}

test.describe('the Credential Configuration Change warning modal (ELITEA-1092/1094/1099)', () => {
  let scratch: ScratchProject | undefined;

  test.beforeEach(async () => {
    scratch = await createScratchProject(`cwm_${test.info().project.name}`);
  });

  test.afterEach(async () => {
    const project = scratch;
    scratch = undefined;
    await deleteScratchProject(project);
  });

  function project(): ScratchProject {
    if (scratch === undefined) throw new Error('the scratch project was not provisioned');
    return scratch;
  }

  test('ELITEA-1092/1094/1099: changing an existing team-project toolkit\'s credential raises the "Credential Configuration Change" modal, and Confirm proceeds to save', async ({
    page,
    request,
  }) => {
    /* onetest: ELITEA-1092, ELITEA-1094, ELITEA-1099 — `ToolkitForm`'s `isTeamProject` prop (which gates the modal) now reaches it from both `ConfigurationTab.tsx`/`EditToolkit.tsx` (computed from `usePersonalProjectId()` vs the selected project) and is exercised here on a genuine team (scratch) project: `checkBeforeSave` intercepts Save on a credential-field change, the modal opens for the toolkit's own author (1094; a non-author's own gate branch would be the same code path, per 1099/#4649), and Confirm changes (the Discard counterpart to 1092) proceeds to the PUT. */
    test.setTimeout(60_000);

    const pid = project().id;
    const RUN_ID = String(Date.now()).slice(-6);
    const firstTitle = `${AUTOTEST_PREFIX}cwm_a_${RUN_ID}`;
    const secondTitle = `${AUTOTEST_PREFIX}cwm_b_${RUN_ID}`;
    let fixture: ReportPortalFixture | undefined;
    let secondCredentialId = '';
    try {
      fixture = await createReportPortalToolkit(request, pid, `${AUTOTEST_PREFIX}cwm_toolkit_${RUN_ID}`, firstTitle);
      secondCredentialId = await seedReportPortalCredential(request, pid, secondTitle);

      await gotoToolkit(page, project().name, fixture.toolkitId);

      const picker = reportPortalConfigPicker(page);
      await picker.click();
      await page.getByRole('option').filter({ hasText: secondTitle }).click();

      await page.getByRole('button', { name: 'Save', exact: true }).click();

      const modal = page.getByRole('dialog').filter({ hasText: 'Credential Configuration Change' });
      await expect(
        modal,
        'the "Credential Configuration Change" modal must intercept Save on a team-project toolkit credential change',
      ).toBeVisible({ timeout: 10_000 });

      const putResponse = page.waitForResponse(
        (r) => r.request().method() === 'PUT' && /\/elitea_core\/tool\/prompt_lib\//.test(r.url()),
        { timeout: 20_000 },
      );
      await modal.getByRole('button', { name: 'Confirm changes' }).click();
      const put = await putResponse;
      expect(put.ok(), `confirming the credential change answered ${put.status()}`).toBe(true);
    } finally {
      await deleteReportPortalFixture(request, pid, fixture, secondCredentialId);
    }
  });
});
