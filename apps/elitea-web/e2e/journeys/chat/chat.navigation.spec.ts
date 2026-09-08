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
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT COLLAPSING TAKES AWAY, AND WHAT IT MUST NOT
 * ─────────────────────────────────────────────────────────────────────────────
 * A collapsed row loses its VISIBLE label — `SidebarNavItem` renders the
 * `Typography` only while `showLabel` is true (`SidebarNavItem.tsx:79`) — and
 * that is the discriminator the legacy test used. It does NOT lose its
 * accessible name: the same component wraps every row in a `Tooltip` whose
 * title is the label exactly while the label is hidden
 * (`SidebarNavItem.tsx:33-38`), and MUI puts a string title on the child as
 * `aria-label` (`@mui/material/Tooltip/Tooltip.js:489`). That is the correct
 * behaviour for an icon-only link — an unnamed one would be an a11y defect,
 * and `checkA11y` below would say so — so the collapsed rail is asserted on
 * the label text, and the link is asserted to still be there and still named.
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

  // Scoped to the shell rail (`Sidebar.tsx:64-66`'s `nav[aria-label="side-bar"]`),
  // never to the page: the conversation rail beside it holds rows of its own.
  const rail = page.getByRole('navigation', { name: 'side-bar' });
  // Expanded: the row carries its text label. This is the legacy test's own
  // discriminator, and it is the one the collapse really removes.
  const agents = rail.getByRole('link', { name: AGENTS_ROW, exact: true });
  await expect(agents).toBeVisible({ timeout: 15_000 });
  // The row's own rendered text, not a search of the page: the label is the
  // only text this link ever holds, so `''` is exactly "the label is gone".
  await expect(agents).toHaveText(AGENTS_ROW, { timeout: 15_000 });
  await expect(toggle).toHaveAccessibleName('Collapse sidebar');

  await checkA11y(page);

  // Collapsed: `SidebarNavItem` renders no `Typography` at all while
  // `showLabel` is false, so the row holds no text at all — the label is GONE
  // from the document, not merely hidden.
  await toggle.click();
  await expect(toggle).toHaveAccessibleName('Expand sidebar');
  await expect(agents).toHaveText('');
  // The row itself stays, and stays NAMED: the collapsed rail is icons, and an
  // icon-only link with no accessible name would be unreachable by name for a
  // screen reader. The tooltip is what keeps the name (see the header).
  await expect(agents).toHaveCount(1);
  await expect(agents).toBeVisible();
  // The chat surface is unaffected by the rail's width — a collapse that
  // unmounted the page would pass the assertions above.
  await expect(page.getByTestId('chat-message-input')).toBeEditable();

  // …and back, on the same control.
  await toggle.click();
  await expect(toggle).toHaveAccessibleName('Collapse sidebar');
  await expect(agents).toHaveText(AGENTS_ROW, { timeout: 10_000 });

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
