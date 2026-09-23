/**
 * Issues programme, package C-chat — participants-panel grouping/visual
 * treatment, judged against two CLOSED legacy issues.
 *
 * ── #5367 (local MCP grouped under Toolkits instead of MCPs) ────────────────
 * Investigated further than the original bug and found a BIGGER, adjacent
 * gap instead: `Participants.tsx`'s own grouping code buckets ANY
 * `entity_name === 'toolkit'` participant whose `entity_settings
 * .toolkit_type` matches `isMcpToolkitType` into a `'mcp'` group — correctly,
 * regardless of local/remote transport — but then `if (key === 'mcp' &&
 * !isMcpVisible) continue;` DROPS every such participant from the rendered
 * groups entirely unless `isMcpVisible` is `true`. `Participants.tsx`
 * defaults it to `false`, `ParticipantsWrapper.tsx` is a pure prop
 * pass-through (does not call the global `useIsMcpVisible()` feature-flag
 * hook itself), and `pages/chat/index.tsx`'s own `<ParticipantsWrapper>` call
 * site never passed `isMcpVisible` at all. So on this chat page, an
 * MCP-classified participant (local OR remote) was invisible in the
 * Participants panel — not merely mis-grouped.
 *
 * FIXED at the root: `pages/chat/index.tsx` now calls a new 5th feature-local
 * `useIsMcpVisible()` (`features/chat-participants/api/useIsMcpVisible.ts`,
 * matching this codebase's own established per-feature duplication
 * convention for this exact hook) and passes it through. Pinned below.
 *
 * ── #6405 (OpenAPI delegated-OAuth toolkit gets no orange warning frame) ────
 * GAP, confirmed structurally: `ExpandedParticipantsList`/`ParticipantItem`
 * accept exactly two auth-warning slots — `mcpLoginSlot`/`mcpLogoutSlot` (fed
 * only to the `'mcp'` group) and `sharepointLoginSlot` (fed only to
 * SharePoint toolkit rows, per `ParticipantsWrapper.tsx`). There is no third
 * slot, and no generic "this toolkit needs delegated auth" prop keyed off
 * `entity_settings.auth_type`/`requires_auth` for an ordinary (non-MCP,
 * non-SharePoint) Toolkit-type participant — an OpenAPI toolkit configured
 * with OAuth 2.0 delegated auth is exactly that: `entity_name: 'toolkit'`,
 * `toolkit_type: 'openapi'`. No code path can light up a warning frame for
 * it regardless of its auth configuration.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  addConversationParticipant,
  createConversation,
  createGithubToolkit,
  deleteConversation,
  deleteGithubToolkit,
} from '../../fixtures/api';

const SUFFIX = '-closed-participants';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now() % 1_000_000}${SUFFIX}`;
}

async function expandParticipants(page: import('@playwright/test').Page): Promise<void> {
  const expand = page.getByRole('button', { name: 'Expand participants' });
  if ((await expand.count()) > 0) await expand.click();
  await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 15_000 });
}

/* elitea_issues: #5367 — a LOCAL (stdio) MCP participant groups under the MCPs
 * section, never under Toolkits, regardless of transport. */
test('#5367: an MCP-classified participant is grouped under MCPs, not dropped or misfiled', async ({ page }) => {
  test.setTimeout(60_000);
  // FIXED at the root: `pages/chat/index.tsx` now passes `isMcpVisible={useIsMcpVisible()}`
  // to `<ParticipantsWrapper>` (a new 5th feature-local copy of the hook, per this
  // codebase's own established "no-sideways-features" duplication convention — see
  // `features/chat-participants/api/useIsMcpVisible.ts`). Before this fix, the prop was
  // never passed at all, `Participants.tsx` defaulted it `false`, and its own
  // `if (key === 'mcp' && !isMcpVisible) continue;` dropped every mcp-classified
  // participant from every rendered group — not merely placed under the wrong one, as
  // #5367's original report describes, but invisible outright.
  const conversationId = await createConversation(page.request, uniqueName('conv'));
  // A real, resolvable toolkit is needed — a fabricated `entity_meta.id` gets the whole
  // participant dropped by the details-resolution step before grouping ever runs. Its
  // OWN stored type ('github') does not matter: grouping reads `entity_settings
  // .toolkit_type` off the PARTICIPANT row, which this test controls independently.
  const toolkit = await createGithubToolkit(page.request, DEFAULT_PROJECT_ID, uniqueName('tk'));
  try {
    await addConversationParticipant(page.request, conversationId, {
      entity_name: 'toolkit',
      entity_meta: { id: toolkit.toolkitId },
      entity_settings: { toolkit_type: 'mcp_stdio' },
    });

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
    await expandParticipants(page);

    const container = page.getByTestId('participants-container');
    await expect(container, 'the mcp-classified participant must not be dropped').not.toContainText('No participants', {
      timeout: 15_000,
    });
    // Grouped under MCPs, per #5367's actual ask — never under Toolkits.
    await expect(container).toContainText(/MCP/i, { timeout: 15_000 });
    const toolkitsHeader = container.getByText(/^Toolkits/i);
    expect(await toolkitsHeader.count(), 'a local (stdio) MCP must never file under a Toolkits section').toBe(0);
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteGithubToolkit(page.request, DEFAULT_PROJECT_ID, toolkit);
  }
});

/* elitea_issues: #6405 — product gap: an OpenAPI toolkit configured with delegated
 * OAuth gets no orange warning frame in the Participants panel (only the 'mcp' group
 * and SharePoint rows are wired to a login-warning slot at all). */
test('#6405: an OpenAPI delegated-auth toolkit gets no auth warning frame in Participants', async ({ page }) => {
  test.setTimeout(60_000);
  test.fail(
    true,
    '#6405: product gap — ExpandedParticipantsList/ParticipantItem only accept mcpLoginSlot/' +
      'mcpLogoutSlot (fed to the mcp group) and sharepointLoginSlot (fed to SharePoint rows); ' +
      'an OpenAPI toolkit with delegated OAuth has no equivalent slot, so it never gets the ' +
      'orange warning frame regardless of its auth configuration',
  );
  const conversationId = await createConversation(page.request, uniqueName('conv'));
  const toolkit = await createGithubToolkit(page.request, DEFAULT_PROJECT_ID, uniqueName('tk'));
  try {
    await addConversationParticipant(page.request, conversationId, {
      entity_name: 'toolkit',
      entity_meta: { id: toolkit.toolkitId },
      entity_settings: { toolkit_type: 'openapi', auth_type: 'oauth2', requires_auth: true },
    });

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
    await expandParticipants(page);

    const container = page.getByTestId('participants-container');
    await expect(container).toContainText('Toolkits', { timeout: 15_000 });
    // The expected behaviour (per #6405): a "Requires authorization. Log in."
    // warning message next to the row. This never appears for a plain Toolkit row.
    await expect(container.getByText(/Requires authorization/i)).toBeVisible({ timeout: 5_000 });
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteGithubToolkit(page.request, DEFAULT_PROJECT_ID, toolkit);
  }
});
