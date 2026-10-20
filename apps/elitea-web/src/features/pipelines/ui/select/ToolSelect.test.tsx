import type { QueryClient } from '@tanstack/react-query';
import { waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { createTestQueryClient, renderWithRouterAndProject } from '../../__tests__/testUtils';
import { ToolSelect } from './ToolSelect';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';

let queryClient: QueryClient;

beforeEach(() => {
  queryClient = createTestQueryClient();
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})),
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () => HttpResponse.json({ mcp_enabled: true })),
  );
});

// ToolSelect fires two queries on mount (toolkit schemas, platform settings),
// and no test waits for them: every assertion here passes on the first render.
// A response that lands at the end of a test is delivered to React by
// TanStack Query's notifyManager on a setTimeout(0). When that was the file's
// last test, the timer could fire after Vitest had torn the jsdom environment
// down, and React's update scheduling read `window`: "window is not defined",
// an unhandled error in the full run only. Wait for both queries to settle
// while the tree is still mounted; waitFor drains a setTimeout(0) tick after it
// passes, so the delivery happens before cleanup unmounts the tree. This
// afterEach runs before Testing Library's cleanup (hooks run in stack order).
afterEach(async () => {
  await waitFor(() => expect(queryClient.isFetching()).toBe(0));
  resetGeneratedClient();
});

describe('ToolSelect', () => {
  it('lists every version tool by resolved name', async () => {
    const { findByText } = renderWithRouterAndProject(
      <ToolSelect versionTools={[{ type: 'github', name: 'github', toolkit_name: 'github' }]} />,
      PROJECT_ID,
      queryClient,
    );

    expect(await findByText('Toolkit')).toBeInTheDocument();
  });

  it('calls onSelectTool with the original tool object on selection', async () => {
    const user = userEvent.setup();
    const onSelectTool = vi.fn();
    const tool = { type: 'github', name: 'github', toolkit_name: 'github' };

    const { findByRole, getByRole } = renderWithRouterAndProject(
      <ToolSelect
        versionTools={[tool]}
        onSelectTool={onSelectTool}
      />,
      PROJECT_ID,
      queryClient,
    );

    await user.click(await findByRole('combobox'));
    await user.click(getByRole('option', { name: 'github' }));

    expect(onSelectTool).toHaveBeenCalledWith(tool);
  });

  /* elitea_issues: #2125 — toolkit/tool single-select dropdowns must support unselecting (re-clicking the selected option clears it), not force a YAML edit to remove the reference. */
  it('calls onSelectTool(null) on clear', async () => {
    const user = userEvent.setup();
    const onSelectTool = vi.fn();
    const tool = { type: 'github', name: 'github', toolkit_name: 'github' };

    const { findByRole } = renderWithRouterAndProject(
      <ToolSelect
        versionTools={[tool]}
        selectedToolkit="github"
        onSelectTool={onSelectTool}
      />,
      PROJECT_ID,
      queryClient,
    );

    const combobox = await findByRole('combobox');
    await user.click(combobox);
    // Re-selecting the already-selected option triggers onClear (baseline behaviour).
    await user.click(document.querySelector('[data-value="github"]') ?? combobox);

    expect(onSelectTool).toHaveBeenCalled();
  });

  it('resolves the toolkit label/value from the schema when toolkit_name is an explicit empty string', async () => {
    // Regression test for the `||` -> `??` fallback finding: an explicit
    // empty-string `toolkit_name` must still fall back to the
    // schema-derived name (baseline JS `||` falsy semantics), for BOTH
    // the option label and the value onSelectTool receives -- not stay
    // `''` the way a nullish-only `??` fallback would leave it.
    const user = userEvent.setup();
    const onSelectTool = vi.fn();
    const tool = { type: 'github', name: 'fallback-name', toolkit_name: '' };

    const { findByRole, getByRole } = renderWithRouterAndProject(
      <ToolSelect
        versionTools={[tool]}
        onSelectTool={onSelectTool}
      />,
      PROJECT_ID,
      queryClient,
    );

    await user.click(await findByRole('combobox'));
    const option = getByRole('option', { name: 'fallback-name' });
    expect(option).toBeInTheDocument();

    await user.click(option);
    expect(onSelectTool).toHaveBeenCalledWith(tool);
  });

  it('applies a custom filterTypes predicate', async () => {
    const filterTypes = vi.fn((tool: { readonly type?: string }) => tool.type === 'github');

    const { findByRole } = renderWithRouterAndProject(
      <ToolSelect
        versionTools={[
          { type: 'github', name: 'github', toolkit_name: 'github' },
          { type: 'jira', name: 'jira', toolkit_name: 'jira' },
        ]}
        filterTypes={filterTypes}
      />,
      PROJECT_ID,
      queryClient,
    );

    await findByRole('combobox');
    expect(filterTypes).toHaveBeenCalled();
  });

  it('is disabled', async () => {
    const { findByRole } = renderWithRouterAndProject(
      <ToolSelect
        versionTools={[]}
        disabled
      />,
      PROJECT_ID,
      queryClient,
    );
    expect(await findByRole('combobox')).toHaveAttribute('aria-disabled', 'true');
  });
});
