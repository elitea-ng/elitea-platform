import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { SettingsDrawer, type SettingsSection } from './SettingsDrawer';

const SECTIONS: SettingsSection[] = [
  {
    section: 'PROJECT',
    tabs: [
      { id: 'model-configuration', label: 'AI Providers' },
      { id: 'secrets', label: 'Secrets' },
    ],
  },
  {
    section: 'PERSONAL',
    tabs: [
      { id: 'tokens', label: 'Personal Tokens' },
      { id: 'profile', label: 'Profile' },
    ],
  },
];

/** Every tab id `routes/_shell/settings/settings-layout.tsx` can emit. */
const EVERY_TAB_ID = [
  'model-configuration',
  'project-params',
  'prompts',
  'environment',
  'secrets',
  'users',
  'analytics',
  'usage',
  'profile',
  'preferences',
  'ai-personality',
  'memory',
  'tokens',
  'notifications',
] as const;

/**
 * `SettingsDrawer` reads `useLocation`, so it needs a router. The drawer is
 * mounted at /settings/secrets, which makes "Secrets" the active tab.
 */
function renderDrawer(onItemClick = vi.fn()) {
  const rootRoute = createRootRoute();
  const settingsRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/settings/$tab',
    component: () => (
      <SettingsDrawer
        sections={SECTIONS}
        onItemClick={onItemClick}
      />
    ),
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([settingsRoute]),
    history: createMemoryHistory({ initialEntries: ['/settings/secrets'] }),
  });
  renderWithTheme(<RouterProvider router={router as never} />);
  return { onItemClick };
}

describe('SettingsDrawer', () => {
  /**
   * A REAL BUTTON, NOT A DIV WITH onClick. Every item was a plain
   * `<Box onClick>` — a <div> with no role, no tabindex and no href — so a
   * keyboard reached none of them and assistive technology read all eleven
   * labels, "Log out" included, as plain text. That is what the accessibility
   * tree of a live deployment reported.
   */
  it('renders every menu item as a real button', async () => {
    renderDrawer();
    for (const label of ['AI Providers', 'Secrets', 'Personal Tokens', 'Profile']) {
      expect(await screen.findByRole('button', { name: label })).toBeInTheDocument();
    }
  });

  it('marks the active item with aria-current, and no other item', async () => {
    renderDrawer();
    const active = await screen.findByRole('button', { name: 'Secrets' });
    expect(active).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('button', { name: 'AI Providers' })).not.toHaveAttribute('aria-current');
    expect(screen.getByRole('button', { name: 'Profile' })).not.toHaveAttribute('aria-current');
  });

  /**
   * ONE GLYPH PER ROW, and the fallback must not be answering for a live tab.
   *
   * `ICON_COMPONENTS` was keyed on ids the layout no longer emits, so
   * `getIconComponent`'s `?? ConfigurationIcon` covered five of the twelve
   * rows and they all drew the same gear. The assertion is on the RENDERED
   * markup rather than on the map: a map entry that points at the same
   * component as the fallback would still pass a map-shaped test.
   */
  it('gives every settings tab its own icon', async () => {
    const sections: SettingsSection[] = [
      { section: 'ALL', tabs: EVERY_TAB_ID.map((id) => ({ id, label: id })) },
    ];
    const rootRoute = createRootRoute();
    const settingsRoute = createRoute({
      getParentRoute: () => rootRoute,
      path: '/settings/$tab',
      component: () => <SettingsDrawer sections={sections} />,
    });
    const router = createRouter({
      routeTree: rootRoute.addChildren([settingsRoute]),
      history: createMemoryHistory({ initialEntries: ['/settings/secrets'] }),
    });
    renderWithTheme(<RouterProvider router={router as never} />);

    await screen.findByRole('button', { name: 'notifications' });
    const paths = EVERY_TAB_ID.map((id) => {
      const button = screen.getByRole('button', { name: id });
      const path = button.querySelector('svg path');
      return path?.getAttribute('d') ?? '';
    });
    for (const [index, d] of paths.entries()) {
      expect(d, `tab ${EVERY_TAB_ID[index]} rendered no icon`).not.toBe('');
    }
    /* `profile` and `users` intentionally share `HumanIcon`, as the baseline's
       own map does (`Person` / `UsersIcon` are the same glyph in this set), so
       the distinct count is one short of the tab count — not fourteen. */
    expect(new Set(paths).size).toBe(EVERY_TAB_ID.length - 1);
  });

  it('activates an item from the keyboard alone', async () => {
    const user = userEvent.setup();
    const { onItemClick } = renderDrawer();
    const tokens = await screen.findByRole('button', { name: 'Personal Tokens' });

    tokens.focus();
    expect(tokens).toHaveFocus();
    await user.keyboard('{Enter}');

    await waitFor(() => {
      expect(onItemClick).toHaveBeenCalledWith('tokens');
    });
  });
});
