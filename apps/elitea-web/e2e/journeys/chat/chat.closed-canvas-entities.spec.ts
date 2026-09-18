/**
 * Issues programme, package C-chat — a GENUINE product gap, not a bug in an
 * existing feature: the legacy "Canvas" multi-tab entity editor described by
 * eight CLOSED elitea_issues never got ported. This app's equivalent
 * (`processes/chat/ui/ChatEditorColumn.tsx`, `chat-editor-panel`) is ONE
 * single-slot editor column that shows AT MOST one Agent/Pipeline/Toolkit
 * editor at a time — there is no tab bar, no `role="tab"`/`tablist`
 * anywhere in that column, and no Skill or Project-Context editor exists in
 * chat at all (grepped `src/processes/chat/` for `SkillEditor`/
 * `ProjectContext` — no hits; `EditorShell.tsx`'s own doc comment names only
 * `AgentEditorShellProps`/`PipelineEditorShellProps`/`ToolkitEditorShellProps`).
 *
 * Covers, all against the SAME root cause (see S/issues/gaps.md):
 *  - #6219 — "Open Chat-Generated Entities in Canvas Mode" (the feature
 *    request this all traces back to: multiple tabs, one per generated
 *    entity).
 *  - #6239 — Skill/Project-Context creation from chat: the module TOGGLE
 *    exists (`chat.internalTools.spec.ts`'s ELITEA-2783), but there is no
 *    editor to open once created — same absent-editor root cause.
 *  - #6337 — "Canvas allows only one entity tab" — true, and permanent: this
 *    is not a regression, it is the only mode that was ever built.
 *  - #6542 — "Pipeline Flow Editor content shared across tabs" — cannot
 *    reproduce AS DESCRIBED (there is no second tab to leak into), but the
 *    underlying complaint (two entities cannot be reviewed side by side)
 *    stands.
 *  - #6536 — "deleting a pipeline tab corrupts the agent's save" — same:
 *    no simultaneous tabs means no cross-tab save corruption, but also no
 *    way to keep two drafts alive to compare.
 *  - #6529 — "deleted entity tab not auto-closed" — not independently
 *    reproduced this run (would need a chat-generated entity's delete
 *    action while its single editor is open); tracked as the same class.
 *  - #6478, #6535 — Project Context Save/Discard behaviour in Canvas — moot:
 *    Project Context has no chat-embedded editor of any kind to have that
 *    behaviour in.
 *
 * This test pins the observable, falsifiable half: opening a SECOND
 * participant's editor while a FIRST one is open and unsaved does not open a
 * second tab alongside it — it replaces the panel's content outright, with
 * no tab affordance ever appearing.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  addConversationParticipant,
  createAgentWithVersion,
  createConversation,
  deleteAgent,
  deleteConversation,
} from '../../fixtures/api';

const SUFFIX = '-closed-canvas';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now() % 1_000_000}${SUFFIX}`;
}

/* elitea_issues: #6219, #6239, #6337, #6542, #6536 — product gap: the chat page has
 * no multi-tab Canvas for chat-generated/attached entities, only a single-slot editor
 * column. Opening a second entity's editor while a first is open replaces it instead
 * of opening alongside it as a second tab. */
test('opening a second agent editor keeps the first one open in its own tab', async ({ page }) => {
  test.setTimeout(90_000);
  test.fail(
    true,
    '#6219/#6239/#6337/#6542/#6536: product gap — chat has no multi-tab entity Canvas; ' +
      'ChatEditorColumn (chat-editor-panel) shows at most one Agent/Pipeline/Toolkit editor ' +
      'at a time — opening a second entity replaces the first rather than opening a second ' +
      'tab — and Skill/Project-Context have no chat-embedded editor at all',
  );
  const nameA = uniqueName('agent-a');
  const nameB = uniqueName('agent-b');
  const agentA = await createAgentWithVersion(page.request, nameA, { instructions: 'Instructions A' });
  const agentB = await createAgentWithVersion(page.request, nameB, { instructions: 'Instructions B' });
  const conversationId = await createConversation(page.request, uniqueName('conv'));
  try {
    await addConversationParticipant(page.request, conversationId, { entity_name: 'application', entity_meta: { id: agentA.id } });
    await addConversationParticipant(page.request, conversationId, { entity_name: 'application', entity_meta: { id: agentB.id } });

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

    const expand = page.getByRole('button', { name: 'Expand participants' });
    if ((await expand.count()) > 0) await expand.click();
    await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 15_000 });

    async function openEditor(name: string): Promise<void> {
      const row = page.getByTestId('participants-container').getByText(name, { exact: false }).first();
      await expect(row).toBeVisible({ timeout: 20_000 });
      await row.hover();
      const edit = page.getByRole('button', { name: new RegExp(`^Edit .*${name}$`) });
      await expect(edit).toBeVisible({ timeout: 15_000 });
      await edit.click();
    }

    await openEditor(nameA);
    const panel = page.getByTestId('chat-editor-panel');
    await expect(panel).toContainText(nameA, { timeout: 15_000 });

    await openEditor(nameB);
    await expect(panel).toContainText(nameB, { timeout: 15_000 });

    // The actual product ask (#6219): BOTH stay open as separate tabs. There is
    // no tab affordance at all, and A's own editor is gone once B opens.
    await expect(page.getByRole('tab')).toHaveCount(1); // fails: 0 tabs exist, not >=2
    await expect(panel).toContainText(nameA); // fails: A's content was replaced by B's
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agentA.id);
    await deleteAgent(page.request, agentB.id);
  }
});
