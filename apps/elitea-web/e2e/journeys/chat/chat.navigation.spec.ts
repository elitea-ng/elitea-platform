/**
 * The shell rail as seen FROM the chat: it collapses and expands, and it is
 * how a user leaves chat for the agents list.
 *
 * Ported by use case from the legacy public suite:
 *  - `tests/ui/chat/test_chat_interface.py::TestSidebarNavigation::test_open_close_sidebar`
 *    (TC-CHAT-021)
 *  - `tests/ui/chat/test_chat_interface.py::TestSidebarNavigation::test_navigate_to_agents_from_sidebar`
 *    (TC-CHAT-022)
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHICH SIDEBAR, AND WHY THE STARTING STATE IS SEEDED
 * ─────────────────────────────────────────────────────────────────────────────
 * `/app/chat` has TWO collapsible columns, and their controls share their
 * English wording: the shell nav rail (`widgets/sidebar`, toggle
 * `sidebar-collapse-toggle`) and the conversation rail
 * (`features/chat-conversation-list`, an unlabelled `IconButton` inside
 * `chat-conversation-sidebar`). Both say "Collapse sidebar". This file is about
 * the SHELL rail — the one the legacy test drove, which it recognised by
 * whether the "Agents" row showed its text label. Everything here is addressed
 * by test id or scoped to the shell, never by that shared string alone.
 *
 * The collapsed flag is PERSISTED (`el.sidebar.collapsed`, `collapsedPersistence.ts`),
 * so the rail's state on arrival is whatever the last run left. A test that
 * branched on what it found — "if expanded, collapse first; else expand first",
 * which is exactly what the legacy page object did — has two bodies and proves
 * neither. The flag is therefore seeded to "expanded" before the app loads, so
 * there is one path through this test and one meaning for every assertion.
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';

/** `STORAGE_NAMESPACE` + `collapsedPersistence.ts`'s own key. `'0'` is "expanded". */
const COLLAPSED_KEY = 'el.sidebar.collapsed';

/** `navSections()`'s second row of the first group. */
const AGENTS_ROW = 'Agents';

test('the shell rail collapses and expands from the chat, and reaches the Agents page', async ({ page }) => {
  await page.addInitScript((key: string) => {
    try {
      window.localStorage.setItem(key, '0');
    } catch {
      // A browser that refuses storage still renders the default (expanded),
      // which is the state this test seeds; nothing below depends on the write
      // having succeeded, only on the state it produces.
    }
  }, COLLAPSED_KEY);

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  const toggle = page.getByTestId('sidebar-collapse-toggle');
  await expect(toggle).toBeVisible({ timeout: 15_000 });

  // Expanded: the rail's rows carry their text label, so each one has an
  // accessible name. This is the legacy test's own discriminator.
  const agents = page.getByRole('link', { name: AGENTS_ROW, exact: true });
  await expect(agents).toBeVisible({ timeout: 15_000 });
  await expect(toggle).toHaveAccessibleName('Collapse sidebar');

  await checkA11y(page);

  // Collapsed: `SidebarNavItem` renders no `Typography` at all while
  // `showLabel` is false, so the named row is GONE from the accessibility tree
  // — not merely hidden. `count()` is what states that; a visibility assertion
  // would wait for an element that is never going to attach.
  await toggle.click();
  await expect(toggle).toHaveAccessibleName('Expand sidebar');
  await expect(agents).toHaveCount(0);
  // The chat surface is unaffected by the rail's width — a collapse that
  // unmounted the page would pass the two assertions above.
  await expect(page.getByTestId('chat-message-input')).toBeEditable();

  // …and back, on the same control.
  await toggle.click();
  await expect(toggle).toHaveAccessibleName('Collapse sidebar');
  await expect(agents).toBeVisible({ timeout: 10_000 });

  // ── the row is a real link out of chat ───────────────────────────────────
  await agents.click();

  // `toHaveURL`, not `waitForURL`: this is a client-side route change, and
  // `waitForURL` waits on a navigation lifecycle event a history push never
  // fires (the same correction `chat.management.spec.ts`'s M1 carries).
  await expect(page).toHaveURL(/\/app\/agents/, { timeout: 15_000 });
  // The route really changed — the chat composer is gone. A URL rewrite that
  // left the chat mounted would satisfy the address assertion alone.
  await expect(page.getByTestId('chat-message-input')).toHaveCount(0);
});
