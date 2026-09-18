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
 * project, never project 1) with an `aha` toolkit (its `check_connection`
 * answers `unsupported_type` unconditionally — see `toolkits.aha.spec.ts`'s
 * AHA-10 — so `useCredentialSaveGate`'s OWN gate, keyed on the platform's
 * `ELITEA_TOOLKIT_CHECK_ALLOWLIST` refusing every other placeholder
 * credential in this stack, does not confound this one), stands for all
 * three: the credential change goes straight to a successful PUT, with no
 * modal ever rendered.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX } from '../../fixtures/api';
import { ensureProjectSelected } from '../../fixtures/project';
import { readsPlatformFlags } from '../../fixtures/platformFlags';
import { createScratchProject, deleteScratchProject, type ScratchProject } from '../../fixtures/scratchProject';

readsPlatformFlags(test);

const PLACEHOLDER_BASE_URL = 'https://autotest-cwm.invalid.example';
const PLACEHOLDER_API_KEY = 'autotest-placeholder-cwm-key';

interface AhaFixture {
  readonly toolkitId: string;
  readonly credentialId: string;
}

async function seedAhaCredential(request: APIRequestContext, projectId: string, title: string): Promise<string> {
  const response = await request.post(`${API_BASE}/configurations/configurations/${projectId}`, {
    data: { type: 'aha', elitea_title: title, label: title, shared: false, data: { base_url: PLACEHOLDER_BASE_URL, api_key: PLACEHOLDER_API_KEY } },
  });
  expect(response.ok(), `seeding an aha credential: ${await response.text()}`).toBe(true);
  const body = (await response.json()) as { id?: string | number };
  return String(body.id ?? '');
}

/** Creates an `aha` credential, then an `aha` toolkit referencing it — the credential must exist first, or the toolkit POST answers `configuration_not_found`. */
async function createAhaToolkit(request: APIRequestContext, projectId: string, toolkitName: string, credentialTitle: string): Promise<AhaFixture> {
  const credentialId = await seedAhaCredential(request, projectId, credentialTitle);
  const created = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name: toolkitName,
      type: 'aha',
      settings: { aha_configuration: { elitea_title: credentialTitle, private: false }, selected_tools: [] },
    },
  });
  expect(created.status(), `createAhaToolkit: ${await created.text()}`).toBe(201);
  const body = (await created.json()) as { id?: string | number };
  const toolkitId = String(body.id ?? '');
  expect(toolkitId, 'createAhaToolkit must answer an id').not.toBe('');
  return { toolkitId, credentialId };
}

async function deleteAhaFixture(request: APIRequestContext, projectId: string, fixture: AhaFixture | undefined, secondCredentialId: string): Promise<void> {
  if (fixture !== undefined) {
    await request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${fixture.toolkitId}`).catch(() => {});
    await request.delete(`${API_BASE}/configurations/configuration/${projectId}/${fixture.credentialId}`).catch(() => {});
  }
  if (secondCredentialId !== '') {
    await request.delete(`${API_BASE}/configurations/configuration/${projectId}/${secondCredentialId}`).catch(() => {});
  }
}

async function gotoToolkit(page: Page, projectName: string, toolkitId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
  await ensureProjectSelected(page, projectName);
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toBeVisible({ timeout: 20_000 });
}

function ahaConfigPicker(page: Page) {
  return page.getByRole('combobox', { name: /Aha Configuration/i });
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

  test('ELITEA-1092/1094/1099: PRODUCT GAP — changing an existing team-project toolkit\'s credential never raises the "Credential Configuration Change" modal, for anyone', async ({
    page,
    request,
  }) => {
    /* onetest: ELITEA-1092, ELITEA-1094, ELITEA-1099 — product gap: `ToolkitForm`'s `isTeamProject` prop (which gates the modal) defaults to `false` and neither of its two real callers, `ConfigurationTab.tsx`/`EditToolkit.tsx` or `CreateToolkit.tsx`, ever passes it — so `checkBeforeSave` never intercepts Save on ANY team-project toolkit, for its author (1094) or any other team member (1099, regression #4649), and Discard (1092) has nothing to demonstrate because the modal it would discard never opens. */
    test.setTimeout(60_000);
    test.fail(
      true,
      'ELITEA-1092 (#952)/1094/1099 (#4649 regressed silently): product gap — ConfigurationTab.tsx never passes isTeamProject ' +
        'to ToolkitForm, so useCredentialWarning.checkBeforeSave (isTeamProject defaults false) never intercepts Save; ' +
        'the "Credential Configuration Change" modal cannot open on any team-project toolkit, for any user',
    );

    const pid = project().id;
    const RUN_ID = String(Date.now()).slice(-6);
    const firstTitle = `${AUTOTEST_PREFIX}cwm_a_${RUN_ID}`;
    const secondTitle = `${AUTOTEST_PREFIX}cwm_b_${RUN_ID}`;
    let fixture: AhaFixture | undefined;
    let secondCredentialId = '';
    try {
      fixture = await createAhaToolkit(request, pid, `${AUTOTEST_PREFIX}cwm_toolkit_${RUN_ID}`, firstTitle);
      secondCredentialId = await seedAhaCredential(request, pid, secondTitle);

      await gotoToolkit(page, project().name, fixture.toolkitId);

      const picker = ahaConfigPicker(page);
      await picker.click();
      await page.getByRole('option').filter({ hasText: secondTitle }).click();

      const putResponse = page.waitForResponse(
        (r) => r.request().method() === 'PUT' && /\/elitea_core\/tool\/prompt_lib\//.test(r.url()),
        { timeout: 20_000 },
      );
      await page.getByRole('button', { name: 'Save', exact: true }).click();

      // What SHOULD happen: the modal intercepts, and the PUT waits for
      // Confirm changes. What ACTUALLY happens: the PUT fires immediately,
      // with no modal ever rendered — this assertion is the one that fails.
      const modal = page.getByRole('dialog').filter({ hasText: 'Credential Configuration Change' });
      await expect(
        modal,
        'the "Credential Configuration Change" modal must intercept Save on a team-project toolkit credential change',
      ).toBeVisible({ timeout: 5_000 });

      await putResponse;
    } finally {
      await deleteAhaFixture(request, pid, fixture, secondCredentialId);
    }
  });
});
