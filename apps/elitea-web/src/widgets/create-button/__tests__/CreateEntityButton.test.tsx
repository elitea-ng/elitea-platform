import type { ReactElement, ReactNode } from 'react';

import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import userEvent from '@testing-library/user-event';
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useChatSessionStore } from '@/entities/conversation';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { CreateEntityButton } from '../ui/CreateEntityButton';
import { renderAtPath } from './testRouter';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * `renderAtPath` (`./testRouter`) builds its router with NO explicit
 * `context` option — every test that needs a resolved `personal_project_id`
 * (R4's `isPersonalSpaceBlocked`) needs a router that actually carries one.
 * A local variant, not a `testRouter.tsx` edit: this widget's file-scope
 * fence covers this cluster's 5 files plus their paired `*.test.ts(x)`
 * files, and `testRouter.tsx` is neither. Mirrors
 * `features/toolkits/lib/hooks/useSelectedProjectId.test.tsx`'s
 * `renderWithRouterContext` (`context: { auth }` passed straight to
 * `createRouter`) plus `./testRouter`'s own `router.load()`-before-render
 * step.
 */
async function renderAtPathWithPersonalProject(
  pathname: string,
  ui: ReactNode,
  personalProjectId: string | undefined,
) {
  const rootRoute = createRootRoute();
  const testRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: pathname.replace(/^\//, ''),
    component: () => ui as ReactElement,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([testRoute]),
    history: createMemoryHistory({ initialEntries: [pathname] }),
    context: { auth: { getUser: () => ({ personal_project_id: personalProjectId }) } },
  });
  await router.load();
  const result = render(
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      <CssBaseline />
      <RouterProvider router={router} />
    </ThemeProvider>,
  );
  return { ...result, router };
}

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
});

afterEach(() => {
  vi.unstubAllEnvs();
  resetConfigForTests();
  delete (globalThis as { elitea_ui_config?: unknown }).elitea_ui_config;
  useChatSessionStore.setState({ isCreatingNewConversation: false });
});

/** `PERMISSIONS` nests 1-3 levels deep (e.g. `chat.canvas.create`) — collects every leaf string regardless of depth. */
function collectPermissionStrings(node: unknown): string[] {
  if (typeof node === 'string') return [node];
  if (node && typeof node === 'object') return Object.values(node).flatMap(collectPermissionStrings);
  return [];
}

const allPermissions = new Set(collectPermissionStrings(PERMISSIONS));

describe('CreateEntityButton', () => {
  it('renders a plain "Create" trigger on a simple route (SHELL-026)', async () => {
    await renderAtPath('/onboarding', <CreateEntityButton permissions={allPermissions} />);
    expect(screen.getByTestId('sidebar-create-button')).toHaveTextContent('Create');
  });

  it('renders the split button with the route-implied entity label on a recognised route', async () => {
    await renderAtPath('/agents', <CreateEntityButton permissions={allPermissions} />);
    expect(screen.getByTestId('sidebar-create-button')).toHaveTextContent('Agent');
    expect(screen.getByRole('button', { name: 'Choose what to create' })).toBeInTheDocument();
  });

  it('collapsed mode always renders the simple icon-only trigger, even on a recognised route', async () => {
    await renderAtPath(
      '/agents',
      <CreateEntityButton
        permissions={allPermissions}
        collapsed
      />,
    );
    expect(screen.getByTestId('sidebar-create-button')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Choose what to create' })).not.toBeInTheDocument();
  });

  it('disables the trigger when the current entity is not permitted', async () => {
    await renderAtPath('/agents', <CreateEntityButton permissions={new Set()} />);
    expect(screen.getByTestId('sidebar-create-button')).toBeDisabled();
  });

  it('opens the dropdown and lists all 13 entities', async () => {
    const user = userEvent.setup();
    await renderAtPath('/agents', <CreateEntityButton permissions={allPermissions} />);
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));
    expect(screen.getByRole('menuitem', { name: 'Chat' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Invite User' })).toBeInTheDocument();
  });

  it('marks a permission-denied dropdown option aria-disabled, distinct from the (permitted, enabled) active one', async () => {
    const user = userEvent.setup();
    // Active kind on /chat is 'chat', which the old app's CreationPermissions
    // grants via EITHER folders.create or chat.create — held here, so the
    // trigger itself stays enabled and the dropdown can be opened. 'agent'
    // requires PERMISSIONS.applications.create, deliberately NOT granted.
    const permissions = new Set([PERMISSIONS.chat.create]);
    await renderAtPath('/chat', <CreateEntityButton permissions={permissions} />);
    expect(screen.getByTestId('sidebar-create-button')).not.toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));
    expect(screen.getByRole('menuitem', { name: 'Chat' })).toHaveAttribute('aria-disabled', 'false');
    const agentOption = screen.getByRole('menuitem', { name: 'Agent' });
    expect(agentOption).toHaveAttribute('aria-disabled', 'true');
  });

  // SHELL-024 (waived) — both directions of the Secret entry's gate. The
  // dropdown is opened from `/chat`, whose active kind is `chat`, so the
  // trigger stays enabled in BOTH cases and the only variable under test is
  // the Secret option itself.
  it('SHELL-024: a caller holding secrets.create gets a live "Secret" option that opens the new-secret row', async () => {
    const user = userEvent.setup();
    const permissions = new Set([PERMISSIONS.chat.create, PERMISSIONS.secrets.create]);
    const { router } = await renderAtPath('/chat', <CreateEntityButton permissions={permissions} />);
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));
    expect(screen.getByRole('menuitem', { name: 'Secret' })).toHaveAttribute('aria-disabled', 'false');
    await user.click(screen.getByRole('menuitem', { name: 'Secret' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings/secrets'));
    expect(router.state.location.search).toEqual({ createSecret: '1' });
  });

  it('SHELL-024: a caller holding ONLY secrets.list gets a dead "Secret" option that never navigates', async () => {
    const user = userEvent.setup();
    // Exactly the migration-0083 project `viewer`: it lists secret names and
    // cannot create one. Before this gate moved to `secrets.create`, this
    // caller got an enabled option that landed on a page opening no row.
    const permissions = new Set([PERMISSIONS.chat.create, PERMISSIONS.secrets.list]);
    const { router } = await renderAtPath('/chat', <CreateEntityButton permissions={permissions} />);
    const before = router.state.location.pathname;
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));
    expect(screen.getByRole('menuitem', { name: 'Secret' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('menuitem', { name: 'Secret' }));
    expect(router.state.location.pathname).toBe(before);
  });

  it('disables the trigger on the system-prompts settings page regardless of permissions', async () => {
    await renderAtPath('/settings/prompts', <CreateEntityButton permissions={allPermissions} />);
    expect(screen.getByTestId('sidebar-create-button')).toBeDisabled();
  });

  it('clicking the main trigger navigates to the active kind\'s create destination', async () => {
    const user = userEvent.setup();
    const { router } = await renderAtPath('/skills', <CreateEntityButton permissions={allPermissions} />);
    await user.click(screen.getByTestId('sidebar-create-button'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/create'));
  });

  it('selecting a dropdown option navigates to that kind\'s destination and closes the menu', async () => {
    const user = userEvent.setup();
    const { router } = await renderAtPath('/agents', <CreateEntityButton permissions={allPermissions} />);
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));
    await user.click(screen.getByRole('menuitem', { name: 'Pipeline' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/create'));
    expect(screen.queryByRole('menuitem')).not.toBeInTheDocument();
  });

  it('selecting a dropdown option via the keyboard (Enter) also navigates', async () => {
    const user = userEvent.setup();
    const { router } = await renderAtPath('/agents', <CreateEntityButton permissions={allPermissions} />);
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));
    screen.getByRole('menuitem', { name: 'Skill' }).focus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/create'));
  });

  it('clicking a permission-denied dropdown option does not navigate and leaves the menu open', async () => {
    const user = userEvent.setup();
    const permissions = new Set([PERMISSIONS.chat.create]);
    const { router } = await renderAtPath('/chat', <CreateEntityButton permissions={permissions} />);
    const before = router.state.location.pathname;
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));
    await user.click(screen.getByRole('menuitem', { name: 'Agent' }));
    expect(router.state.location.pathname).toBe(before);
    expect(screen.getByRole('menuitem', { name: 'Agent' })).toBeInTheDocument();
  });

  it('disables the trigger when allow_project_own_llms=false and the selected project is not the public one', async () => {
    // `allow_project_own_llms` reads `unknown` and is compared with a
    // strict `=== false` (schema.ts) — env vars are always strings, so a
    // real boolean `false` can only come from the `window.elitea_ui_config`
    // C6 source (checked first, per-key `hasOwnProperty`).
    (globalThis as { elitea_ui_config?: Record<string, unknown> }).elitea_ui_config = {
      allow_project_own_llms: false,
    };
    await renderAtPath(
      '/settings/model-configuration',
      <CreateEntityButton
        permissions={allPermissions}
        projectId="42"
      />,
    );
    expect(screen.getByTestId('sidebar-create-button')).toBeDisabled();
  });

  it('does NOT disable for the own-LLMs gate when the selected project IS the public one', async () => {
    (globalThis as { elitea_ui_config?: Record<string, unknown> }).elitea_ui_config = {
      allow_project_own_llms: false,
    };
    // A resolved personal project id is supplied so this stays isolated to
    // the own-LLMs gate specifically — without it, R4's
    // `isPersonalSpaceBlocked` gate would ALSO fire for "public project, no
    // personal project resolved" and this assertion would be testing two
    // gates' combined polarity instead of one.
    await renderAtPathWithPersonalProject(
      '/settings/model-configuration',
      <CreateEntityButton
        permissions={allPermissions}
        projectId="11"
      />,
      'priv-1',
    );
    expect(screen.getByTestId('sidebar-create-button')).not.toBeDisabled();
  });

  it('does NOT disable for the own-LLMs gate when allow_project_own_llms is unset (defaults true)', async () => {
    await renderAtPath(
      '/settings/model-configuration',
      <CreateEntityButton
        permissions={allPermissions}
        projectId="42"
      />,
    );
    expect(screen.getByTestId('sidebar-create-button')).not.toBeDisabled();
  });

  // R1 — collapsed/simple-route trigger must open the dropdown, not navigate directly.
  it('R1: collapsed mode clicking the trigger opens the dropdown instead of navigating directly', async () => {
    const user = userEvent.setup();
    const { router } = await renderAtPath(
      '/agents',
      <CreateEntityButton
        permissions={allPermissions}
        collapsed
      />,
    );
    const before = router.state.location.pathname;
    await user.click(screen.getByTestId('sidebar-create-button'));
    // The dropdown opened (all 13 entities reachable) instead of firing a
    // direct navigation against `activeKind` — this is the ONLY way to
    // reach the entity picker while collapsed (no chevron exists here).
    expect(screen.getByRole('menuitem', { name: 'Chat' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Agent' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe(before);
  });

  it('R1: on a simple route (expanded sidebar), clicking the trigger opens the dropdown instead of navigating directly', async () => {
    const user = userEvent.setup();
    const { router } = await renderAtPath('/onboarding', <CreateEntityButton permissions={allPermissions} />);
    const before = router.state.location.pathname;
    await user.click(screen.getByTestId('sidebar-create-button'));
    expect(screen.getByRole('menuitem', { name: 'Chat' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe(before);
  });

  it('R1: the dropdown opened from the collapsed/simple trigger still navigates once an entity is chosen', async () => {
    const user = userEvent.setup();
    const { router } = await renderAtPath(
      '/agents',
      <CreateEntityButton
        permissions={allPermissions}
        collapsed
      />,
    );
    await user.click(screen.getByTestId('sidebar-create-button'));
    await user.click(screen.getByRole('menuitem', { name: 'Pipeline' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/create'));
  });

  // R2 — the create-chat action must be disabled while a conversation creation is already in flight.
  it('R2: disables the trigger when isCreatingNewConversation is true and the active kind is chat', async () => {
    useChatSessionStore.setState({ isCreatingNewConversation: true });
    await renderAtPath('/chat', <CreateEntityButton permissions={allPermissions} />);
    expect(screen.getByTestId('sidebar-create-button')).toBeDisabled();
  });

  it('R2: does NOT disable the trigger for isCreatingNewConversation when the active kind is not chat', async () => {
    useChatSessionStore.setState({ isCreatingNewConversation: true });
    await renderAtPath('/agents', <CreateEntityButton permissions={allPermissions} />);
    expect(screen.getByTestId('sidebar-create-button')).not.toBeDisabled();
  });

  // R4 — "no personal project / viewing public project" disable gate.
  it('R4: disables the trigger when no personal project is resolved and the selected project is the public one', async () => {
    await renderAtPathWithPersonalProject(
      '/agents',
      <CreateEntityButton
        permissions={allPermissions}
        projectId="11"
      />,
      undefined,
    );
    expect(screen.getByTestId('sidebar-create-button')).toBeDisabled();
  });

  it('R4: does NOT disable the trigger when a personal project id IS resolved, even on the public project', async () => {
    await renderAtPathWithPersonalProject(
      '/agents',
      <CreateEntityButton
        permissions={allPermissions}
        projectId="11"
      />,
      'priv-1',
    );
    expect(screen.getByTestId('sidebar-create-button')).not.toBeDisabled();
  });

  it('R4: does NOT disable the trigger for the personal-space gate when the selected project is NOT the public one', async () => {
    await renderAtPathWithPersonalProject(
      '/agents',
      <CreateEntityButton
        permissions={allPermissions}
        projectId="42"
      />,
      undefined,
    );
    expect(screen.getByTestId('sidebar-create-button')).not.toBeDisabled();
  });

  // R6 — the own-LLMs gate's polarity when no project id has resolved yet.
  it('R6: disables the own-LLMs gate when allow_project_own_llms=false and NO project id has resolved yet', async () => {
    (globalThis as { elitea_ui_config?: Record<string, unknown> }).elitea_ui_config = {
      allow_project_own_llms: false,
    };
    await renderAtPath('/settings/model-configuration', <CreateEntityButton permissions={allPermissions} />);
    expect(screen.getByTestId('sidebar-create-button')).toBeDisabled();
  });

  // issue #903/ELITEA-1044 — the active entity gets a checkmark, not just a background highlight.
  it('issue #903 — marks the active entity with a checkmark icon in the dropdown, and no other item', async () => {
    const user = userEvent.setup();
    await renderAtPath('/agents', <CreateEntityButton permissions={allPermissions} />);
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));

    const activeOption = screen.getByRole('menuitem', { name: 'Agent' });
    expect(activeOption.querySelector('svg[data-testid="CheckIcon"]')).not.toBeNull();

    const inactiveOption = screen.getByRole('menuitem', { name: 'Chat' });
    expect(inactiveOption.querySelector('svg[data-testid="CheckIcon"]')).toBeNull();
  });

  it('issue #903 — a permission-denied active-looking option gets no checkmark', async () => {
    const user = userEvent.setup();
    // /artifacts' active kind is 'bucket'. `hasMainButtonCreatePermission`
    // bypasses the permission check for 'bucket' (see `command.ts`'s own
    // doc comment: "if the user can access the Artifacts page, they should
    // be able to create buckets"), so the TRIGGER stays enabled and
    // clickable even with no bucket permission granted — while the
    // DROPDOWN item still uses the real `hasCreatePermission` gate and
    // renders `aria-disabled`. This is the one route where the active kind
    // is both open-able and permission-denied at once.
    await renderAtPath('/artifacts', <CreateEntityButton permissions={new Set()} />);
    expect(screen.getByTestId('sidebar-create-button')).not.toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Choose what to create' }));
    const bucketOption = screen.getByRole('menuitem', { name: 'Artifact Bucket' });
    expect(bucketOption).toHaveAttribute('aria-disabled', 'true');
    expect(bucketOption.querySelector('svg[data-testid="CheckIcon"]')).toBeNull();
  });

  // issue #904/ELITEA-1046 — a "Create New" tooltip on hover, collapsed or expanded.
  it('issue #904 — the collapsed trigger carries a "Create New" tooltip', async () => {
    const user = userEvent.setup();
    await renderAtPath(
      '/agents',
      <CreateEntityButton
        permissions={allPermissions}
        collapsed
      />,
    );
    await user.hover(screen.getByTestId('sidebar-create-button'));
    expect(await screen.findByRole('tooltip', { name: 'Create New' })).toBeInTheDocument();
  });

  it('issue #904 — the expanded (split) trigger also carries a "Create New" tooltip', async () => {
    const user = userEvent.setup();
    await renderAtPath('/agents', <CreateEntityButton permissions={allPermissions} />);
    await user.hover(screen.getByTestId('sidebar-create-button'));
    expect(await screen.findByRole('tooltip', { name: 'Create New' })).toBeInTheDocument();
  });
});
