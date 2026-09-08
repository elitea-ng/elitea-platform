/**
 * CREDENTIAL STATUS INDICATORS against a real provider — the live port of
 * `qa/elitea-testing-public/automation/tests/ui/toolkits/
 * test_toolkit_indicators_for_credentials.py` (legacy enhancement #5114,
 * bugs #4906 / #5183; cases TC-1778, TC-1782, TC-1784, TC-1785):
 *
 *  - LIVE-TK-IND-1 ← `TestToolkitCredentialIndicators::
 *    test_toolkit_credential_indicators_e2e`   (the toolkit page)
 *  - LIVE-TK-IND-2 ← `TestPipelineCredentialIndicators::
 *    test_pipeline_credential_indicators_e2e`  (the pipeline page)
 *  - LIVE-TK-IND-3 ← `TestAgentCredentialIndicators::
 *    test_agent_credential_indicators_e2e`     (the agent page)
 *
 * Gated on Jira, the provider the legacy file uses, because the case NEEDS a
 * reachable provider that will genuinely refuse a wrong secret: an indicator
 * driven by a made-up failure proves the icon renders, not that the platform
 * noticed.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THESE THREE ARE EXPECTED RED TODAY, AND THAT IS WHY THEY ARE HERE
 * ─────────────────────────────────────────────────────────────────────────────
 * Two named product gaps stand between this lane and a green run. Both were
 * read out of the code rather than inferred from a failure:
 *
 *  1. **No toolkit-type connection check.** `checkableConnectionTypes`
 *     (`services/elitea-main/internal/api/v2/configurations/check_connection.go`)
 *     holds the eight `ai_credentials` provider types only, and #319 scoped
 *     that route to the LiteLLM path deliberately. A `jira` payload therefore
 *     answers "Checking connection is not supported yet for configuration type
 *     jira", which `useCredentialValidation.ts` maps to the `unsupported`
 *     status — and `CredentialOptionLabel` renders the attention indicator on
 *     `invalid` alone. So the indicator cannot light for ANY toolkit
 *     credential, valid or not.
 *  2. **No credential picker on the agent or pipeline page.**
 *     `useToolkitCredentialPickerSlot` is composed by
 *     `pages/toolkits/{Create,Edit}Toolkit.tsx` and by nothing else, so
 *     LIVE-TK-IND-2 and LIVE-TK-IND-3 have no picker to open at all.
 *
 * They are written as journeys, not left as skips, for the reason this wave
 * states throughout: a skip reads as coverage. Each one fails on a single
 * named line that says which gap it hit, which is what turns "we know" into
 * "we will be told the day it changes". `e2e/journeys/api/
 * api.export-import-agents.spec.ts` carries the same shape for Go defect D1.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { API_BASE } from '../fixtures/api';

import { LIVE_TOOLKIT_PROVIDERS } from './liveEnv';
import {
  provisionLiveToolkit,
  removeLiveToolkit,
  runTag,
  selectedProjectId,
  type ProvisionedLiveToolkit,
} from './liveToolkits';

const provider = LIVE_TOOLKIT_PROVIDERS.jira;

/** Open the toolkit's Configuration tab and its credential dropdown. */
async function openCredentialPicker(page: Page, toolkitId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 60_000 });
  const picker = page.getByRole('combobox').first();
  await expect(
    picker,
    'the toolkit configuration form must render a credential picker — ' +
      'pages/toolkits/lib/credentialPickerSlots.tsx is what composes it',
  ).toBeVisible({ timeout: 30_000 });
  await picker.click();
}

test('LIVE-TK-IND-1: a Jira credential the provider rejects shows the auth-failed indicator on the toolkit page, and fixing it clears it (legacy test_toolkit_credential_indicators_e2e)', async ({
  page,
}) => {
  test.setTimeout(420_000);

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
  const projectId = await selectedProjectId(page);

  let provisioned: ProvisionedLiveToolkit | undefined;
  try {
    // The WRONG secret with a real base URL and a real user name: what comes
    // back has to be an authentication refusal, not a name-resolution failure.
    provisioned = await provisionLiveToolkit(page.request, projectId, provider, {
      tag: runTag(),
      broken: true,
    });

    await openCredentialPicker(page, provisioned.toolkitId);

    // ── the four controls the legacy body reads, in its own order ─────────
    const indicator = page.getByTestId('credential-status-indicator');
    await expect(
      indicator,
      'a credential the provider refuses must carry the attention indicator — ' +
        'today it cannot, because check_connection answers "unsupported" for every ' +
        'toolkit type (checkableConnectionTypes, #319)',
    ).toBeVisible({ timeout: 60_000 });
    await expect(indicator).toHaveAttribute('aria-label', /Authentication failed/i);
    await expect(page.getByTestId('credential-reload-button')).toBeVisible();
    // …and this one is visible whatever the status, which is the half of
    // TC-1782 that a "hide everything on failure" regression would break.
    await expect(page.getByTestId('credential-open-in-new-tab-button')).toBeVisible();

    // ── fix the credential, then Reload ──────────────────────────────────
    const fixed = await page.request.put(
      `${API_BASE}/configurations/configuration/${projectId}/${provisioned.credentialId}`,
      { data: { type: provider.id, data: provider.credentialData() } },
    );
    expect(fixed.ok(), `fixing the credential answered ${fixed.status()}`).toBe(true);

    await page.getByTestId('credential-reload-button').click();

    await expect(
      page.getByTestId('credential-status-indicator'),
      'the indicator must clear once the credential works, without a page reload',
    ).toHaveCount(0, { timeout: 60_000 });
    await expect(page.getByTestId('credential-reload-button')).toHaveCount(0);
    await expect(page.getByTestId('credential-open-in-new-tab-button')).toBeVisible();
  } finally {
    await removeLiveToolkit(page.request, provisioned);
  }
});

test('LIVE-TK-IND-2: the same indicator is surfaced on the pipeline page (legacy test_pipeline_credential_indicators_e2e)', async ({
  page,
}) => {
  test.setTimeout(300_000);

  await page.goto(`${BASE_URL}/app/pipelines`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('main')).toBeVisible({ timeout: 60_000 });

  // The gap named in this file's header, asserted where it is decided: no
  // pipeline route composes `useToolkitCredentialPickerSlot`, so there is no
  // picker on this page for an indicator to sit in.
  await expect(
    page.getByTestId('credential-status-indicator').or(page.getByTestId('credential-reload-button')),
    'the pipeline page must offer the credential controls the toolkit page does — ' +
      'today only pages/toolkits composes the credential picker slot',
  ).toBeVisible({ timeout: 30_000 });
});

test('LIVE-TK-IND-3: the same indicator is surfaced on the agent page (legacy test_agent_credential_indicators_e2e)', async ({
  page,
}) => {
  test.setTimeout(300_000);

  await page.goto(`${BASE_URL}/app/agents`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('main')).toBeVisible({ timeout: 60_000 });

  await expect(
    page.getByTestId('credential-status-indicator').or(page.getByTestId('credential-reload-button')),
    'the agent page must offer the credential controls the toolkit page does — ' +
      'today only pages/toolkits composes the credential picker slot',
  ).toBeVisible({ timeout: 30_000 });
});
