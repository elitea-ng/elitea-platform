/**
 * The agent editor's TAG control (`features/agents/ui/AgentTagEditor.tsx`,
 * #345) — adding several tags in ONE save and having all of them persist
 * across a reload.
 *
 * Ported BY USE CASE from the onetest wave-1 agents package:
 *   ELITEA-0037, ELITEA-0038, ELITEA-0039.
 *
 * No sibling spec in this directory exercised the tag control at all before
 * this file (`grep -rn 'AgentTagEditor\|agent-tags' e2e/journeys/agents` was
 * empty) — the hint sheet's "COVERED | agents.editor.spec.ts" for all three
 * cases was wrong; that file never mentions tags. `e2e/journeys/api/
 * api.tags-authors.spec.ts` covers the tag API in isolation (create/list/
 * dedupe), not the agent-editor round trip these three cases are about.
 *
 * `AgentTagEditor` has no `data-testid` of its own. Rather than add one to
 * `src/` (this stack runs a pre-built container image — `docker-compose.
 * e2e-standalone.yml`'s `elitea-web` service pulls `ghcr.io/eliteaai/
 * elitea-web:e2e` — so a testid added now would not exist in the already-
 * running build this file has to pass against today), this file reaches the
 * control the way it is already reachable: `getByRole('combobox', { name:
 * 'Tags' })` for the input (its `<label>` already reads "Tags"), and MUI
 * `Autocomplete`'s own rendered chip shape for the rest — confirmed live
 * against this exact build (`role="button"` on the `Chip` root; the delete
 * icon inside is `aria-hidden`, so the chip's accessible NAME is the tag
 * text alone).
 *
 * The control is MUI `Autocomplete` (`multiple`, `freeSolo`, `autoSelect`):
 * typing a name and pressing Enter creates a NEW tag chip (`autoSelect`
 * commits the freeSolo text without a matching option); typing a name that
 * matches an EXISTING option and clicking it selects that option instead
 * (`isOptionEqualToValue` matches by id for a real option). Both paths are
 * exercised below.
 *
 * EVERY tag name below carries a run-unique suffix (not a fixed literal):
 * `readTags`'s project-wide list and the `Autocomplete`'s own option-match
 * are both SHARED, project-scoped state, and this suite runs its files with
 * multiple workers — a literal name collided across two concurrently-running
 * instances of this same file (`--repeat-each` measured it directly: one
 * worker's freeSolo commit briefly saw the OTHER worker's in-flight tag as
 * an existing option, and `existing.id` mismatched after save). Uniqueness
 * is the fix, not a wait.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgentWithVersion, createTag, deleteAgent, deleteAutotestTags, readVersion } from '../../fixtures/api';

import type { Page } from '@playwright/test';

/**
 * A run-unique stem — every tag/agent name below is built from one of these,
 * never a literal. UNDERSCORE-joined, not hyphen-joined: a tag name is
 * validated client-side against `AgentTagEditor.tsx`'s own
 * `/^[\w,\s]+$/` (`isValidTagName`) — word characters, comma and whitespace
 * only — and a hyphen (the separator every OTHER `uniqueName` helper in this
 * directory uses for agent/version names) would silently fail that check,
 * so a freeSolo-typed tag carrying one is dropped rather than committed.
 */
function uniqueStem(tag: string): string {
  return `${AUTOTEST_PREFIX}tags_${tag}_${String(Date.now()).slice(-7)}_${Math.random().toString(36).slice(2, 6)}`;
}

async function openAgentEditor(page: Page, agentId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
}

/** The Tags `Autocomplete`'s own text input — the only one on the agent editor page. */
function tagsInput(page: Page) {
  return page.getByRole('combobox', { name: 'Tags' });
}

/** Types a tag NAME and commits it as a chip — `autoSelect` resolves the freeSolo text on Enter. */
async function addTagByTyping(page: Page, name: string): Promise<void> {
  const input = tagsInput(page);
  await input.click();
  await input.fill(name);
  await input.press('Enter');
  // `autoSelect`/`freeSolo` clears the input's own text once the value is
  // COMMITTED as a chip — waited on rather than assumed, so two adds in a
  // row cannot have the second keystroke land before the first commit's
  // state update has actually applied.
  await expect(input).toHaveValue('', { timeout: 5_000 });
}

/** Types enough to match an EXISTING tag and clicks the real option (not freeSolo text). */
async function addTagByPickingOption(page: Page, tagName: string): Promise<void> {
  const input = tagsInput(page);
  await input.click();
  await input.fill(tagName);
  const option = page.getByRole('option', { name: tagName });
  await expect(option).toBeVisible({ timeout: 10_000 });
  await option.click();
  await expect(input).toHaveValue('', { timeout: 5_000 });
}

/**
 * The rendered chips. There is exactly one `Autocomplete` on this page (the
 * Tags field), so `.MuiAutocomplete-tag` (the class MUI's `Autocomplete`
 * itself puts on every `Chip` it draws for a selected value) needs no further
 * scoping — see this file's own module doc comment for why a class selector
 * is used here rather than a fresh `data-testid`.
 */
function tagChips(page: Page) {
  return page.locator('.MuiAutocomplete-tag');
}

async function saveAndWaitForVersionPut(page: Page): Promise<void> {
  const saved = page.waitForResponse(
    (response) => response.request().method() === 'PUT' && response.url().includes('/elitea_core/version/'),
  );
  await page.getByTestId('agent-save-button').click();
  const response = await saved;
  expect(response.status(), `the version PUT carrying tags must succeed`).toBeLessThan(400);
}

/*
 * ELITEA-0037 — an agent that already carries ONE saved tag gets two more
 * brand-new tags added in the SAME save, and all three survive a reload.
 */
test('J14c-tags: an agent with one saved tag keeps it when two new tags are added in one save', async ({
  page,
  request,
}) => {
  const savedTagName = uniqueStem('saved');
  const newTag1 = uniqueStem('new1');
  const newTag2 = uniqueStem('new2');
  const agent = await createAgentWithVersion(request, uniqueStem('agent'), { tags: [{ name: savedTagName }] });
  try {
    await openAgentEditor(page, agent.id);
    await expect(tagChips(page)).toHaveText([savedTagName], { timeout: 20_000 });

    await addTagByTyping(page, newTag1);
    await addTagByTyping(page, newTag2);
    await expect(tagChips(page)).toHaveCount(3);

    await saveAndWaitForVersionPut(page);
    await page.reload();
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });

    const stored = await readVersion(request, agent.id, agent.versionId);
    expect(
      stored.tags.map((tag) => tag.name).sort(),
      'the server must keep the pre-existing tag AND both new ones',
    ).toEqual([savedTagName, newTag1, newTag2].sort());
    await expect(tagChips(page)).toHaveCount(3, { timeout: 20_000 });
  } finally {
    await deleteAgent(request, agent.id);
    await deleteAutotestTags(request, [savedTagName, newTag1, newTag2]);
  }
});

/*
 * ELITEA-0038 — an agent with NO tags gets three brand-new (system-new) tags
 * added in one save; all three persist.
 */
test('J14c-tags: an agent with no tags persists three brand-new tags added in one save', async ({ page, request }) => {
  const tag1 = uniqueStem('alpha');
  const tag2 = uniqueStem('beta');
  const tag3 = uniqueStem('gamma');
  const agent = await createAgentWithVersion(request, uniqueStem('agent'), {});
  try {
    await openAgentEditor(page, agent.id);
    await expect(tagsInput(page)).toBeVisible({ timeout: 20_000 });
    await expect(tagChips(page)).toHaveCount(0);

    await addTagByTyping(page, tag1);
    await addTagByTyping(page, tag2);
    await addTagByTyping(page, tag3);
    await expect(tagChips(page)).toHaveCount(3);

    await saveAndWaitForVersionPut(page);
    await page.reload();
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });

    const stored = await readVersion(request, agent.id, agent.versionId);
    expect(stored.tags.map((tag) => tag.name).sort()).toEqual([tag1, tag2, tag3].sort());
    await expect(tagChips(page)).toHaveCount(3, { timeout: 20_000 });
  } finally {
    await deleteAgent(request, agent.id);
    await deleteAutotestTags(request, [tag1, tag2, tag3]);
  }
});

/*
 * ELITEA-0039 (variant B, the harder of the two parametrized cases — see
 * this file's own module doc comment for why variant A, all-system-existing,
 * is not run separately: it exercises the SAME `isOptionEqualToValue`/
 * option-select branch this test already drives for two of its four tags) —
 * mixing SYSTEM-EXISTING tags (selected from the autocomplete's own option
 * list) and SYSTEM-NEW tags (typed freeSolo) in one save.
 */
test('J14c-tags: mixing system-existing and system-new tags in one save persists all of them', async ({
  page,
  request,
}) => {
  const existing1 = await createTag(request, uniqueStem('existing1'));
  const existing2 = await createTag(request, uniqueStem('existing2'));
  const newTag1 = uniqueStem('mix1');
  const newTag2 = uniqueStem('mix2');
  const agent = await createAgentWithVersion(request, uniqueStem('agent'), {});
  try {
    await openAgentEditor(page, agent.id);

    // SYSTEM-EXISTING: type enough to match the seeded tag and pick the real
    // OPTION from the dropdown — not freeSolo text, so this exercises the
    // `isOptionEqualToValue`/id-matched branch rather than minting a second
    // tag of the same name.
    await addTagByPickingOption(page, existing1.name);
    await addTagByPickingOption(page, existing2.name);

    // SYSTEM-NEW: freeSolo-typed, matching no option.
    await addTagByTyping(page, newTag1);
    await addTagByTyping(page, newTag2);

    await expect(tagChips(page)).toHaveCount(4);

    await saveAndWaitForVersionPut(page);
    await page.reload();
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });

    const stored = await readVersion(request, agent.id, agent.versionId);
    expect(stored.tags.map((tag) => tag.name).sort()).toEqual(
      [existing1.name, existing2.name, newTag1, newTag2].sort(),
    );
    // The two SYSTEM-EXISTING tags must be the SAME rows, not duplicates —
    // this is what tells the option-select path from a freeSolo mint of an
    // identical name.
    expect(stored.tags.find((tag) => tag.name === existing1.name)?.id).toBe(existing1.id);
    expect(stored.tags.find((tag) => tag.name === existing2.name)?.id).toBe(existing2.id);
    await expect(tagChips(page)).toHaveCount(4, { timeout: 20_000 });
  } finally {
    await deleteAgent(request, agent.id);
    await deleteAutotestTags(request, [existing1.name, existing2.name, newTag1, newTag2]);
  }
});
