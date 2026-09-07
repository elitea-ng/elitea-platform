import { ThemeProvider } from '@mui/material/styles';
import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme, docsLink } from '@/shared/brand';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../__tests__/testUtils';
import { ToolkitTypeSelector } from './ToolkitTypeSelector';
import type { ToolkitTypeSelectorProps } from './ToolkitTypeSelector';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * `CategorySection`'s own `CategoryItemCard` rows use `useTextOverflow`
 * (`shared/ui/lib/useTextOverflow.ts`), which constructs a real
 * `ResizeObserver` — jsdom (this project's `node` vitest environment) does
 * not provide one. Same stub `pages/credentials/CredentialTypeSelector.test.tsx`
 * (an analogous type-selector) already established for this exact,
 * pre-existing gap.
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

function renderSelector(props: Partial<ToolkitTypeSelectorProps> = {}) {
  const onSelectTool = props.onSelectTool ?? vi.fn();
  const setFormikInitialValues = props.setFormikInitialValues ?? vi.fn();

  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({
    component: () => (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <ToolkitTypeSelector
            onSelectTool={onSelectTool}
            setFormikInitialValues={setFormikInitialValues}
            {...props}
          />
        </ThemeProvider>
      </SocketClientContext.Provider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return { onSelectTool, setFormikInitialValues };
}

describe('ToolkitTypeSelector', () => {
  it('renders the toolkit-type entries from the real schema catalogue', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));

    renderSelector();

    expect(await screen.findByText('GitHub')).toBeInTheDocument();
    expect(screen.getByText('Choose the toolkit type')).toBeInTheDocument();
  });

  it('calls onSelectTool and setFormikInitialValues with the selected type on click', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    const user = userEvent.setup();
    const { onSelectTool, setFormikInitialValues } = renderSelector();

    await user.click(await screen.findByText('GitHub'));

    expect(onSelectTool).toHaveBeenCalledWith(expect.objectContaining({ type: 'github' }));
    expect(setFormikInitialValues).toHaveBeenCalledTimes(1);
  });

  it('filters entries by the search box, case-insensitively', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } }, jira: { metadata: { label: 'Jira' } } })),
    );
    const user = userEvent.setup();
    renderSelector();

    await screen.findByText('GitHub');
    await user.type(screen.getByRole('textbox', { name: 'Search toolkits' }), 'git');

    expect(screen.getByText('GitHub')).toBeInTheDocument();
    expect(screen.queryByText('Jira')).not.toBeInTheDocument();
  });

  it('shows the no-results message when the search matches nothing', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    const user = userEvent.setup();
    renderSelector();

    await screen.findByText('GitHub');
    await user.type(screen.getByRole('textbox', { name: 'Search toolkits' }), 'zzz-no-match');

    await waitFor(() => expect(screen.getByText('No toolkits found')).toBeInTheDocument());
  });

  /**
   * [visual-parity regression] The baseline groups the catalogue by
   * `metadata.categories[0]` and renders a category-chip row plus one
   * uppercase section heading per category (`Category.GroupedCategory`). This
   * port rendered ONE un-grouped section and no chips at all, so neither
   * existed on screen. Against the pre-fix component every assertion below
   * fails: `getAllByTestId('category-filter-tab')` finds nothing and there is
   * no `Code Repositories`/`Testing` heading.
   */
  it('groups the catalogue by category, with a chip per category and a section heading per group', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          github: { metadata: { label: 'GitHub', categories: ['code repositories'] } },
          testrail: { metadata: { label: 'TestRail', categories: ['testing'] } },
        }),
      ),
    );
    renderSelector();

    await screen.findByText('GitHub');
    // Word-wise Title Case, exactly as the baseline's `getCategoryForToolkit` produces.
    expect(screen.getByRole('button', { name: 'Code Repositories' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Testing' })).toBeInTheDocument();
    // The section headings are separate elements from the chips.
    expect(screen.getAllByText('Code Repositories')).toHaveLength(2);
    expect(screen.getAllByText('Testing')).toHaveLength(2);
  });

  it('narrows the catalogue to the picked category when a chip is clicked', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          github: { metadata: { label: 'GitHub', categories: ['code repositories'] } },
          testrail: { metadata: { label: 'TestRail', categories: ['testing'] } },
        }),
      ),
    );
    const user = userEvent.setup();
    renderSelector();

    await screen.findByText('GitHub');
    await user.click(screen.getByRole('button', { name: 'Testing' }));

    await waitFor(() => expect(screen.queryByText('GitHub')).not.toBeInTheDocument());
    expect(screen.getByText('TestRail')).toBeInTheDocument();
  });

  /**
   * [visual-parity regression] Every tile in the reference carries a leading
   * brand glyph (`useToolkitSearch.js`'s `getToolIcon`). This port passed
   * items with no `icon` at all, so `CategoryItemCard` rendered its icon slot
   * not at all — label-only tiles.
   */
  it('renders a leading icon inside each type tile', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    renderSelector();

    const tile = (await screen.findByText('GitHub')).closest('button');
    expect(tile?.querySelector('svg')).not.toBeNull();
  });

  it('uses application copy when isApplication is true', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub', application: true } } })));
    renderSelector({ isApplication: true });

    expect(await screen.findByText('Choose the application type')).toBeInTheDocument();
  });

  it('renders nothing when isMCP is true and the platform disables MCP', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
      http.get('/api/v2/platform/settings', () => HttpResponse.json({ mcp_enabled: false })),
    );
    renderSelector({ isMCP: true });

    await waitFor(() => expect(screen.queryByText('Choose the MCP type')).not.toBeInTheDocument());
  });

  /**
   * [R2 regression] Baseline: `ToolkitTypeSelector.jsx:165-190`'s MCP-only
   * `EmptyPlaceholder` (`allowEmptyCategory={isMCP}` +
   * `renderCategory`) — a project with zero locally-registered MCP toolkit
   * types gets a direct docs link instead of the generic "no results, try
   * adjusting your search terms" message (which is misleading here: there
   * is nothing to search for). Before this fix, `ToolkitTypeSelector.tsx`
   * had exactly one empty-state branch (`filteredItems.length === 0` ->
   * always the generic `NoResultsMessage`), so this assertion fails against
   * the pre-fix code (confirmed by reverting the fix locally and
   * re-running: the generic "No MCPs found" / "Try adjusting your search
   * terms" copy renders instead, and the docs link is absent) and passes
   * once the MCP-specific branch is restored.
   */
  it('shows the MCP-specific "no local MCP available" documentation link, not the generic no-results message, when there are zero MCP toolkit types', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
      http.get('/api/v2/elitea_core/platform_settings/prompt_lib', () => HttpResponse.json({ mcp_enabled: true })),
    );
    renderSelector({ isMCP: true });

    await waitFor(() => expect(screen.getByText('Choose the MCP type')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText(/Still no local MCP available/)).toBeInTheDocument());

    const link = screen.getByRole('link', { name: 'Documentation' });
    // Brand-derived (ADR-0024 WP8): the served pack states no docsUrl here, so this is the shipped origin.
    expect(link).toHaveAttribute('href', docsLink('integrations/mcp/create-and-use-server-stdio'));
    expect(link).toHaveAttribute('href', expect.stringMatching(/^https:\/\/.+\/integrations\/mcp\/create-and-use-server-stdio$/));
    expect(link).toHaveAttribute('target', '_blank');

    expect(screen.queryByText('No MCPs found')).not.toBeInTheDocument();
    expect(screen.queryByText('Try adjusting your search terms')).not.toBeInTheDocument();
  });

  /**
   * Validation gap 1: "MCPs cannot be created at all". The page above is the
   * whole of the MCP create surface, and it was a dead end because the served
   * catalogue carried no mcp-flavoured entry at all — `useGetCurrentMCPSchemas`
   * filtered the catalogue to `key === 'mcp' || value.type === 'mcp' || key
   * ends with 'mcp'` and found nothing, so `useToolMenuItems` produced no items
   * and only the pinned, empty `Local` section rendered.
   *
   * `internal/api/v2/toolkits/mcp_projection.go` now serves that entry. The two
   * assertions below are what "the chooser works" means, and they are
   * independent: LOCAL keeps its docs pointer (there is still no local runner),
   * and REMOTE gains a real, clickable `Remote MCP` tile. Production shows both
   * chips at once, which is why this is one test and not two.
   */
  it('shows both a Local section with the docs pointer and a Remote section offering Remote MCP once the catalogue serves the mcp type', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          github: { metadata: { label: 'GitHub' } },
          mcp: { type: 'object', title: 'mcp', metadata: { label: 'Remote MCP', categories: ['other'] } },
        }),
      ),
      http.get('/api/v2/elitea_core/platform_settings/prompt_lib', () => HttpResponse.json({ mcp_enabled: true })),
    );
    renderSelector({ isMCP: true });

    // The Remote half: a real tile, named as production names it.
    expect(await screen.findByRole('button', { name: 'Remote MCP' })).toBeEnabled();
    // `Remote` renders twice by design — once as a filter chip, once as the
    // section heading — so this counts rather than expecting a single node.
    expect(screen.getAllByText('Remote').length).toBeGreaterThanOrEqual(2);

    // The Local half is UNCHANGED by the Remote tile arriving. A pre-built
    // local runner is still not offered, so the guidance must stay.
    expect(screen.getAllByText('Local').length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText(/Still no local MCP available/)).toBeInTheDocument();

    // A toolkit type that is not MCP-shaped must not leak onto this tab.
    expect(screen.queryByText('GitHub')).not.toBeInTheDocument();
  });

  /**
   * The other direction of the same projection: a pre-built `mcp_<key>` type
   * belongs on the TOOLKIT tab under the MCP heading, exactly as production
   * files its 23 pre-built servers, and must not appear on the MCP tab (whose
   * Local section is for a user's own local runner).
   */
  it('files a pre-built mcp_ type under the MCP category of the toolkit chooser', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          mcp_context7: { type: 'object', title: 'mcp_context7', metadata: { label: 'Context7', categories: ['mcp'] } },
        }),
      ),
    );
    renderSelector();

    expect(await screen.findByRole('button', { name: 'Context7' })).toBeEnabled();
    // Chip plus section heading, as above.
    expect(screen.getAllByText('MCP').length).toBeGreaterThanOrEqual(2);
  });
});
