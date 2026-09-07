import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { CreateToolkit, createdEntityKind } from './CreateToolkit';
import { renderToolkitsRoute } from './__tests__/testRouter';

/**
 * `ToolkitTypeSelector`'s `CategorySection` rows use `useTextOverflow`
 * (`shared/ui/lib/useTextOverflow.ts`), which constructs a real
 * `ResizeObserver` — jsdom (this project's `node` vitest environment) does
 * not provide one. Same stub `features/toolkits/ui/ToolkitTypeSelector.test.tsx`/
 * `pages/credentials/CredentialTypeSelector.test.tsx` already establish for
 * this exact, pre-existing gap.
 */
class ResizeObserverStub {
  observe(): void {
    // no-op
  }
  disconnect(): void {
    // no-op
  }
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
});

describe('CreateToolkit', () => {
  it('shows the type selector first, then the toolkit form once a type is picked', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    const user = userEvent.setup();
    const createToolkit = vi.fn().mockResolvedValue({ id: 'tk-new', type: 'github', name: 'GitHub' });

    renderToolkitsRoute(<CreateToolkit deps={{ createToolkit }} />, '/toolkits/create', { projectId: 'proj-1' });

    expect(await screen.findByText('New Toolkit')).toBeInTheDocument();
    expect(screen.getByText('Choose the toolkit type')).toBeInTheDocument();

    await user.click(await screen.findByText('GitHub'));

    await waitFor(() => expect(screen.queryByText('Choose the toolkit type')).not.toBeInTheDocument());
  });

  /**
   * The E2E locator, at unit level.
   *
   * `e2e/journeys/toolkits/*` and `e2e/fixtures/api.ts:1097` all reach the
   * create page through `getByPlaceholder('Search toolkits')` — the chooser's
   * `CategoryFilter` search box, chosen because a stub route with a bare
   * heading has no form control. That placeholder is therefore a CONTRACT of
   * this page, not an implementation detail of `CategoryFilter`, and nothing
   * below the page level pins it: `ToolkitTypeSelector.test.tsx` addresses the
   * same box by its `aria-label` instead, so the placeholder could be dropped
   * with every unit test still green and five journeys failing at once.
   */
  it('offers the chooser search box the journeys address it by', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));

    renderToolkitsRoute(<CreateToolkit deps={{ createToolkit: vi.fn() }} />, '/toolkits/create', { projectId: 'proj-1' });

    expect(await screen.findByPlaceholderText('Search toolkits')).toBeInTheDocument();
    expect(await screen.findByRole('button', { name: 'GitHub' })).toBeInTheDocument();
  });

  it('shows "New MCP" when isMCP is true', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})));
    const createToolkit = vi.fn();

    renderToolkitsRoute(<CreateToolkit isMCP deps={{ createToolkit }} />, '/toolkits/create', { projectId: 'proj-1' });

    expect(await screen.findByText('New MCP')).toBeInTheDocument();
  });

  it('shows "New Application" when isApplication is true', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})));
    const createToolkit = vi.fn();

    renderToolkitsRoute(<CreateToolkit isApplication deps={{ createToolkit }} />, '/toolkits/create', { projectId: 'proj-1' });

    expect(await screen.findByText('New Application')).toBeInTheDocument();
  });

  /**
   * [visual-parity regression] The baseline renders this page inside
   * `StyledTabs` WITHOUT `hideBackButton`, so its tab bar always opens with a
   * `BackButton` left of the title. This port rendered the title alone, so the
   * screen had no back affordance at all — a missing element, not a style
   * difference, and this assertion fails against the pre-fix component.
   */
  it('renders a back button left of the title in the sub-header', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})));
    const createToolkit = vi.fn();

    renderToolkitsRoute(<CreateToolkit deps={{ createToolkit }} />, '/toolkits/create', { projectId: 'proj-1' });

    const title = await screen.findByText('New Toolkit');
    const back = screen.getByRole('button', { name: 'Back' });
    expect(back).toBeInTheDocument();
    // Left of the title, in the same bar.
    expect(back.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('does not render the save/cancel tab bar before a type is picked', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    const createToolkit = vi.fn();

    renderToolkitsRoute(<CreateToolkit deps={{ createToolkit }} />, '/toolkits/create', { projectId: 'proj-1' });

    await screen.findByText('Choose the toolkit type');
    expect(screen.queryByRole('button', { name: /save/i })).not.toBeInTheDocument();
  });

  it('clicking Save after picking a type calls deps.createToolkit with the real project id and selected type', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    const user = userEvent.setup();
    const createToolkit = vi.fn().mockResolvedValue({ id: 'tk-new', type: 'github', name: 'GitHub' });

    renderToolkitsRoute(<CreateToolkit deps={{ createToolkit }} />, '/toolkits/create', { projectId: 'proj-1' });

    await user.click(await screen.findByText('GitHub'));

    const saveButton = await screen.findByRole('button', { name: /save/i });
    await waitFor(() => expect(saveButton).not.toBeDisabled());
    await user.click(saveButton);

    await waitFor(() => expect(createToolkit).toHaveBeenCalledTimes(1));
    expect(createToolkit.mock.calls[0]?.[0]).toMatchObject({ projectId: 'proj-1', type: 'github' });
  });

  // Regression: a successful create used to be silently discarded — no
  // navigation, form still dirty, a second Save duplicated the toolkit.
  it('a successful Save REPLACES the create route with the created toolkit detail route', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    const user = userEvent.setup();
    const createToolkit = vi.fn().mockResolvedValue({ id: 'tk-new', type: 'github', name: 'GitHub' });

    const { router } = renderToolkitsRoute(<CreateToolkit deps={{ createToolkit }} />, '/toolkits/create', { projectId: 'proj-1' });
    const replaceSpy = vi.spyOn(router.history, 'replace');

    await user.click(await screen.findByText('GitHub'));
    const saveButton = await screen.findByRole('button', { name: /save/i });
    await waitFor(() => expect(saveButton).not.toBeDisabled());
    await user.click(saveButton);

    await waitFor(() => expect(router.state.location.pathname).toBe('/toolkits/all/tk-new'));
    // History REPLACE, not push — Back must not reopen the stale create form.
    expect(replaceSpy).toHaveBeenCalled();
  });

  it('a failed Save stays on the create route and shows the error', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    const user = userEvent.setup();
    const createToolkit = vi.fn().mockRejectedValue(new Error('boom'));

    const { router } = renderToolkitsRoute(<CreateToolkit deps={{ createToolkit }} />, '/toolkits/create', { projectId: 'proj-1' });

    await user.click(await screen.findByText('GitHub'));
    const saveButton = await screen.findByRole('button', { name: /save/i });
    await waitFor(() => expect(saveButton).not.toBeDisabled());
    await user.click(saveButton);

    expect(await screen.findByRole('alert')).toHaveTextContent(/failed to create/i);
    expect(router.state.location.pathname).toBe('/toolkits/create');
  });
});

/**
 * The destination branch of the baseline's
 * `CreateToolkitToolTabBar.jsx:148-156`: pre-built `mcp_*` types land on the
 * MCP detail route even when created from `/toolkits/create`.
 */
describe('createdEntityKind', () => {
  it('routes MCP creates (and pre-built mcp_* types from the toolkit route) to the MCP page', () => {
    expect(createdEntityKind(true, false, 'mcp')).toBe('mcp');
    expect(createdEntityKind(false, false, 'mcp_github')).toBe('mcp');
  });

  it('the bare "mcp" type means remote MCP, not pre-built — a plain toolkit create keeps its own route', () => {
    expect(createdEntityKind(false, false, 'mcp')).toBe('toolkit');
    expect(createdEntityKind(false, false, 'github')).toBe('toolkit');
  });

  it('application creates land on the apps page', () => {
    expect(createdEntityKind(false, true, 'github')).toBe('app');
  });
});
