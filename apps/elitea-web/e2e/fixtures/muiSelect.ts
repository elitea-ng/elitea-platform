/**
 * Opens a MUI `Select` deterministically: from the keyboard, then waits for
 * its listbox (#3156).
 *
 * Do not use `select.click()` to open a Select whose options a test then
 * clicks. MUI's `Select` opens on the trigger's MOUSEDOWN and arms an "opening
 * mouseup" selection (`SelectInput`'s `scheduleMouseUpSelection` /
 * `handleItemMouseUp`): a mouseup that arrives more than 200 ms later (400 ms
 * when nothing is selected) CLICKS whichever option the menu has put under the
 * pointer, the press-drag-release gesture of a native select. A human click
 * releases well inside that window. Playwright sends mousedown, waits for the
 * page to process it (including the synchronous render of every option), and
 * only then sends mouseup. On a loaded webkit runner that gap passed 200 ms,
 * so the mouseup picked a random row and closed the menu, and the test's next
 * option click had nothing to land on.
 *
 * `focus()` dispatches no mouse event, and `Select` opens on Enter, so that
 * path cannot fire. The options render in a portal at the document root and
 * are then clicked normally: a mousedown ON an option disarms the opening
 * mouseup, and the option's own click does the selection.
 *
 * The listbox being visible does not mean every option has rendered yet.
 * Callers click options through auto-waiting locators (by role and name), not
 * by position.
 *
 * A spec that exists to pin the MOUSE path on purpose
 * (`streaming/chat.pipeline-authored.spec.ts`'s `openSelect`) keeps clicking.
 */
import { expect, type Locator } from '@playwright/test';

export async function openMuiSelect(select: Locator): Promise<void> {
  await select.focus();
  await select.press('Enter');
  await expect(select.page().getByRole('listbox')).toBeVisible({ timeout: 10_000 });
}
