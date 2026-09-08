import type { ComponentProps } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getListTagsMockHandler } from '@/shared/api/generated/tags/tags.msw';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../__tests__/testUtils';
import { ApplicationEditForm } from './ApplicationEditForm';

/** `EntityIcon`/`GradientIconWrapper` read `theme.shape`/`theme.vars.palette.*` — this file drives its own `RouterProvider` (needed for `useSelectedProjectId`'s `useRouteContext`) rather than the shared `renderWithProviders` helper, so the theme has to be wired in here too. */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(getListTagsMockHandler({ rows: [{ id: 1, name: 'billing', data: null }], total: 1 }));
});

afterEach(() => {
  resetGeneratedClient();
});

function renderForm(props: Partial<ComponentProps<typeof ApplicationEditForm>> = {}) {
  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <ApplicationEditForm
            name="My Agent"
            onNameChange={vi.fn()}
            description="Does things"
            onDescriptionChange={vi.fn()}
            tags={[]}
            onTagsChange={vi.fn()}
            projectId="proj-1"
            {...props}
          />
        </ThemeProvider>
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  return render(<RouterProvider router={router} />);
}

describe('ApplicationEditForm', () => {
  it('wraps its content in the titled, collapsible "General" accordion', async () => {
    renderForm();
    await screen.findByTestId('agent-name-input');
    expect(screen.getByRole('button', { name: 'General' })).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'General' })).toBeInTheDocument();
  });

  it('renders the name and description fields seeded from props', async () => {
    renderForm();
    expect(await screen.findByTestId('agent-name-input')).toHaveValue('My Agent');
    expect(screen.getByTestId('agent-description-input')).toHaveValue('Does things');
  });

  it('trims the name on blur and reports it via onNameChange', async () => {
    const onNameChange = vi.fn();
    renderForm({ name: '', onNameChange });
    const input = await screen.findByTestId('agent-name-input');
    const user = userEvent.setup();
    await user.type(input, '  Trimmed Name  ');
    await user.tab();
    expect(onNameChange).toHaveBeenLastCalledWith('Trimmed Name');
  });

  it('reports every keystroke on the description field via onDescriptionChange', async () => {
    const onDescriptionChange = vi.fn();
    renderForm({ description: '', onDescriptionChange });
    const input = await screen.findByTestId('agent-description-input');
    await userEvent.setup().type(input, 'x');
    expect(onDescriptionChange).toHaveBeenCalledWith('x');
  });

  it('shows the at-limit hint only while the name field is focused at the max length', async () => {
    renderForm({ name: 'x'.repeat(32) });
    const input = await screen.findByTestId('agent-name-input');
    // The name counter mounts only at the limit, so its absence is a real
    // absence. The description counter is a different test id — it is always
    // in the document (#848: never unmounted, only visibility-toggled).
    expect(screen.queryByTestId('agent-name-counter')).not.toBeInTheDocument();
    await userEvent.setup().click(input);
    const counter = screen.getByTestId('agent-name-counter');
    // The whole sentence, not just the count: a name field at its limit tells
    // the reader nothing more will be accepted (legacy
    // `test_agent_character_limits.py`'s own assertion).
    expect(counter).toHaveTextContent('0 characters left. You have reached the MAXIMUM character limit');
  });

  it('renders an existing tag passed via the tags prop', async () => {
    renderForm({ tags: [{ id: 1, name: 'billing', data: null }] });
    expect(await screen.findByText('billing')).toBeInTheDocument();
  });

  it('shows the remaining-characters hint while the description field is focused and non-empty', async () => {
    renderForm({ description: 'Does things' });
    const input = await screen.findByTestId('agent-description-input');
    // #848 — the counter's line is now reserved always (never unmounted),
    // so the SAME node is found before focus too; only its visibility toggles.
    const counter = screen.getByText(`${2304 - 'Does things'.length} characters left`);
    expect(counter).not.toBeVisible();
    await userEvent.setup().click(input);
    expect(counter).toBeVisible();
  });

  it('re-syncs the local name field when the name prop changes externally (e.g. a discard/reset)', async () => {
    const { rerender } = renderForm({ name: 'First Name' });
    expect(await screen.findByTestId('agent-name-input')).toHaveValue('First Name');

    const queryClient = createTestQueryClient();
    const rootRoute = createRootRoute({
      component: () => (
        <QueryClientProvider client={queryClient}>
          <ThemeProvider
            theme={theme}
            defaultMode={DEFAULT_COLOR_SCHEME}
          >
            <ApplicationEditForm
              name="Reset Name"
              onNameChange={vi.fn()}
              description="Does things"
              onDescriptionChange={vi.fn()}
              tags={[]}
              onTagsChange={vi.fn()}
              projectId="proj-1"
            />
          </ThemeProvider>
        </QueryClientProvider>
      ),
    });
    const router = createRouter({
      routeTree: rootRoute,
      history: createMemoryHistory({ initialEntries: ['/'] }),
      context: { auth: { getSelectedProjectId: () => 'proj-1' } },
    });
    // A SECOND router swapped in under the same tree: @tanstack/react-router
    // 1.170.32 (#680) mounts a fresh router's route tree asynchronously, so the
    // element is not there on the next tick the way 1.170.18 rendered it —
    // load the router first, then the swap is synchronous like before.
    await router.load();
    rerender(<RouterProvider router={router} />);
    expect(await screen.findByTestId('agent-name-input')).toHaveValue('Reset Name');
  });

  it('renders without crashing when isFromPipeline is set (pipeline entity icon path)', async () => {
    renderForm({ isFromPipeline: true });
    expect(await screen.findByTestId('agent-name-input')).toBeInTheDocument();
  });

  it('adds a freeSolo-typed new tag as a locally-created tag', async () => {
    const onTagsChange = vi.fn();
    renderForm({ tags: [], onTagsChange });
    const input = await screen.findByLabelText('Tags');
    const user = userEvent.setup();
    await user.type(input, 'new tag{Enter}');
    expect(onTagsChange).toHaveBeenCalled();
    const lastCallTags = onTagsChange.mock.calls.at(-1)?.[0] as { name: string }[];
    expect(lastCallTags.map((tag) => tag.name)).toEqual(['new tag']);
  });

  it('rejects a freeSolo-typed tag with invalid characters', async () => {
    const onTagsChange = vi.fn();
    renderForm({ tags: [], onTagsChange });
    const input = await screen.findByLabelText('Tags');
    const user = userEvent.setup();
    await user.type(input, '!!!invalid!!!{Enter}');
    // Invalid entries are filtered out entirely -> resolved to an empty tag list.
    expect(onTagsChange).toHaveBeenCalledWith([]);
  });

  it('accepts a freeSolo-typed tag name containing a comma, matching the old app’s NormalTagNameInputRegExp', async () => {
    const onTagsChange = vi.fn();
    renderForm({ tags: [], onTagsChange });
    const input = await screen.findByLabelText('Tags');
    const user = userEvent.setup();
    await user.type(input, 'a,b{Enter}');
    const lastCallTags = onTagsChange.mock.calls.at(-1)?.[0] as { name: string }[];
    expect(lastCallTags.map((tag) => tag.name)).toEqual(['a,b']);
  });

  it('clears the description focus (and its characters-left hint) on blur', async () => {
    renderForm({ description: 'Does things' });
    const input = await screen.findByTestId('agent-description-input');
    const user = userEvent.setup();
    await user.click(input);
    // #848 — the same node persists across blur (never unmounted); only its
    // visibility toggles back to hidden.
    const counter = screen.getByText(`${2304 - 'Does things'.length} characters left`);
    expect(counter).toBeVisible();
    await user.tab();
    expect(counter).not.toBeVisible();
  });

  it('opens the tag dropdown with a pre-selected tag, exercising option/value equality for both the existing chip and freeSolo strings', async () => {
    renderForm({ tags: [{ id: 1, name: 'billing', data: null }] });
    const input = await screen.findByLabelText('Tags');
    await userEvent.setup().click(input);
    // The pre-selected "billing" chip is compared against the fetched "billing" option
    // (both non-string Tag objects -> `option.id === selected.id`) while the dropdown is open.
    expect((await screen.findAllByText('billing')).length).toBeGreaterThan(0);
  });

  it('resolves a freeSolo-typed tag matching an existing tag name to that existing tag', async () => {
    const onTagsChange = vi.fn();
    renderForm({ tags: [], onTagsChange });
    const input = await screen.findByLabelText('Tags');
    const user = userEvent.setup();
    await user.type(input, 'billing{Enter}');
    const lastCallTags = onTagsChange.mock.calls.at(-1)?.[0] as { id: number; name: string }[];
    expect(lastCallTags).toEqual([{ id: 1, name: 'billing', data: null }]);
  });
});
