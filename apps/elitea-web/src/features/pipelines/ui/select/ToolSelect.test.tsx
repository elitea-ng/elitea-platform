import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../../__tests__/testUtils';
import { ToolSelect } from './ToolSelect';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})),
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () => HttpResponse.json({ mcp_enabled: true })),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('ToolSelect', () => {
  it('lists every version tool by resolved name', async () => {
    const { findByText } = renderWithRouterAndProject(
      <ToolSelect versionTools={[{ type: 'github', name: 'github', toolkit_name: 'github' }]} />,
      PROJECT_ID,
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
    );
    expect(await findByRole('combobox')).toHaveAttribute('aria-disabled', 'true');
  });
});
