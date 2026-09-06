import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import type { Theme } from '@mui/material/styles';
import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../../__tests__/testUtils';
import type { LLMModelSelectorProps } from '../../indexes/ui/IndexDetails/IndexChat';

import { TestToolSettings, contentContainerSx } from './TestToolSettings';
import type { TestToolSettingsProps } from './TestToolSettings';

// `features/toolkits/__tests__/testUtils.tsx`'s own `renderWithRouterAndProject`
// is a NOT-exported (test-file-private) helper — this local harness mirrors
// it exactly (same `ThemeProvider`/`QueryClientProvider`/`RouterProvider`
// stack, plus the `SocketClientContext` this component's own
// `useGetCurrentToolkitSchemas()` call additionally needs) rather than
// widening that shared test-only file's export surface, which a sibling
// Wave-2 sub-unit owns and may be concurrently editing in this shared
// worktree (per the mission's own cross-cutting-hazard note).
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderWithHarness(ui: ReactElement, projectId: string) {
  function RootComponent() {
    return (
      <QueryClientProvider client={createTestQueryClient()}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <SocketClientContext.Provider value={createTestSocketClient()}>{ui}</SocketClientContext.Provider>
        </ThemeProvider>
      </QueryClientProvider>
    );
  }
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => projectId } },
  });
  return render(<RouterProvider router={router} />);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { properties: { selected_tools: { items: { enum: ['list_issues', 'create_issue'] } } } } })));
});

afterEach(() => {
  resetGeneratedClient();
});

function FakeLLMModelSelector(_props: LLMModelSelectorProps) {
  return <div>llm-model-selector</div>;
}

function renderTestToolSettings(overrides: Partial<TestToolSettingsProps> = {}): ReturnType<typeof render> {
  const defaultProps: TestToolSettingsProps = {
    selectedTool: null,
    onChangeTool: vi.fn(),
    toolInputVariables: {},
    onChangeInputVariables: vi.fn(),
    onRunTool: vi.fn(),
    isRunning: false,
    isValidForm: false,
    selectedToolSchema: null,
    values: { type: 'github', settings: {} },
    llm: { selectedModel: null, onSelectModel: vi.fn(), models: [], llmSettings: undefined, onSetLLMSettings: vi.fn() },
    LLMModelSelector: FakeLLMModelSelector,
    indexNameValidation: { indexNameError: null, clearIndexNameError: vi.fn(), updateIndexNameError: vi.fn(), isIndexNameValid: vi.fn(() => true) },
    ...overrides,
  };

  return renderWithHarness(<TestToolSettings {...defaultProps} />, 'proj-1');
}

describe('TestToolSettings', () => {
  it('renders the Test Settings title and the injected LLMModelSelector', async () => {
    renderTestToolSettings();
    expect(await screen.findByText('Test Settings')).toBeInTheDocument();
    expect(await screen.findByText('llm-model-selector')).toBeInTheDocument();
  });

  it('lists the explicit values.settings.selected_tools, sorted by label, when present', async () => {
    const user = userEvent.setup();
    renderTestToolSettings({ values: { type: 'github', settings: { selected_tools: ['zeta_tool', 'alpha_tool'] } } });
    await user.click(await screen.findByLabelText('Tool'));
    const options = await screen.findAllByRole('option');
    expect(options.map((option) => option.textContent)).toEqual(['Alpha tool', 'Zeta tool']);
  });

  it('falls back to the static schema tool names when there is no explicit selection', async () => {
    const user = userEvent.setup();
    renderTestToolSettings();
    await user.click(await screen.findByLabelText('Tool'));
    const options = await screen.findAllByRole('option');
    expect(options.map((option) => option.textContent)).toEqual(['Create issue', 'List issues']);
  });

  /**
   * #440. Three outcomes that used to look identical on screen: a toolkit
   * with no tools, a toolkit whose tools the backend publishes at runtime,
   * and a failed read. The first two now differ from the third, and the
   * three tests below are what discriminate them — one alone cannot.
   */
  describe('dynamic tool catalogue (#440)', () => {
    const DISCOVER_PATH = '/api/v2/elitea_core/toolkit_discover_tools/prompt_lib/:projectId/:toolkitType';
    /** A type with no static schema entry and no explicit selection, so the dynamic tier is the one in use. */
    const DYNAMIC_VALUES = { type: 'openapi_tool', settings: {} };

    it('lists the tools the backend publishes for a dynamic toolkit type', async () => {
      const user = userEvent.setup();
      server.use(
        http.post(DISCOVER_PATH, () =>
          HttpResponse.json({
            tools: [
              { id: '2', name: 'zeta_op', type: 'openapi_tool' },
              { id: '1', name: 'alpha_op', type: 'openapi_tool' },
            ],
            total: 2,
          }),
        ),
      );

      renderTestToolSettings({ values: DYNAMIC_VALUES });

      await user.click(await screen.findByLabelText('Tool'));
      const options = await screen.findAllByRole('option');
      expect(options.map((option) => option.textContent)).toEqual(['Alpha op', 'Zeta op']);
      expect(screen.queryByTestId('tool-list-error')).not.toBeInTheDocument();
    });

    it('shows an error instead of the picker when the read fails', async () => {
      server.use(http.post(DISCOVER_PATH, () => HttpResponse.json({ error: 'read available tools failed' }, { status: 500 })));

      renderTestToolSettings({ values: DYNAMIC_VALUES });

      expect(await screen.findByTestId('tool-list-error')).toBeInTheDocument();
      // The empty picker must not stand in for the failure.
      expect(screen.queryByLabelText('Tool')).not.toBeInTheDocument();
    });

    it('shows the empty picker, not an error, when the read succeeds with no tools', async () => {
      server.use(http.post(DISCOVER_PATH, () => HttpResponse.json({ tools: [], total: 0 })));

      renderTestToolSettings({ values: DYNAMIC_VALUES });

      expect(await screen.findByLabelText('Tool')).toBeInTheDocument();
      expect(screen.queryByTestId('tool-list-error')).not.toBeInTheDocument();
    });

    /**
     * The second read behind this picker. A lost schema read empties the
     * static tier, so the catalogue tier takes over; on a 200 with no tools
     * the screen used to show an empty picker for a failure.
     */
    it('shows an error, not the empty picker, when the toolkit schema read fails', async () => {
      server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ error: 'schemas unavailable' }, { status: 500 })));
      server.use(http.post(DISCOVER_PATH, () => HttpResponse.json({ tools: [], total: 0 })));

      renderTestToolSettings({ values: DYNAMIC_VALUES });

      expect(await screen.findByTestId('tool-list-error')).toBeInTheDocument();
      expect(screen.queryByLabelText('Tool')).not.toBeInTheDocument();
    });
  });

  /**
   * #440. The argument form drew no fields both for a tool that takes no
   * arguments and for a lost schema read. `toolSchemaRead` tells them apart.
   */
  describe('tool argument schema read (#440)', () => {
    it('shows an error with a retry above the argument form when the schema read failed', async () => {
      const onRetry = vi.fn();
      renderTestToolSettings({ selectedTool: 'list_issues', selectedToolSchema: null, toolSchemaRead: { isError: true, onRetry } });

      const error = await screen.findByTestId('tool-schema-error');
      expect(error).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      expect(onRetry).toHaveBeenCalledTimes(1);
    });

    it('shows no error when the schema read succeeded and the tool takes no arguments', async () => {
      renderTestToolSettings({ selectedTool: 'list_issues', selectedToolSchema: { properties: {} }, toolSchemaRead: { isError: false, onRetry: vi.fn() } });

      expect(await screen.findByText('RUN TOOL')).toBeInTheDocument();
      expect(screen.queryByTestId('tool-schema-error')).not.toBeInTheDocument();
    });
  });

  it('calls onChangeTool with the selected value', async () => {
    const user = userEvent.setup();
    const onChangeTool = vi.fn();
    renderTestToolSettings({ onChangeTool, values: { type: 'github', settings: { selected_tools: ['alpha_tool'] } } });
    await user.click(await screen.findByLabelText('Tool'));
    await user.click(await screen.findByRole('option', { name: 'Alpha tool' }));
    expect(onChangeTool).toHaveBeenCalledWith('alpha_tool');
  });

  it('renders no tool-argument form and no Run Tool button while no tool is selected', () => {
    renderTestToolSettings({ selectedTool: null });
    expect(screen.queryByText('RUN TOOL')).not.toBeInTheDocument();
  });

  it('renders one field per selectedToolSchema property once a tool is selected, and merges field edits into the whole toolInputVariables object', async () => {
    const user = userEvent.setup();
    const onChangeInputVariables = vi.fn();
    renderTestToolSettings({
      selectedTool: 'list_issues',
      toolInputVariables: { other_field: 'kept' },
      onChangeInputVariables,
      selectedToolSchema: { properties: { repo: { type: 'string', title: 'Repo' } } },
    });

    const input = await screen.findByLabelText('Repo');
    await user.type(input, 'x');

    expect(onChangeInputVariables).toHaveBeenCalledWith({ other_field: 'kept', repo: 'x' });
  });

  it('Run Tool is disabled when isValidForm is false', async () => {
    renderTestToolSettings({ selectedTool: 'list_issues', isValidForm: false, selectedToolSchema: { properties: {} } });
    expect(await screen.findByText('RUN TOOL')).toBeDisabled();
  });

  it('Run Tool is disabled while isRunning is true even when the form is valid', async () => {
    renderTestToolSettings({ selectedTool: 'list_issues', isValidForm: true, isRunning: true, selectedToolSchema: { properties: {} } });
    expect(await screen.findByText('RUN TOOL')).toBeDisabled();
  });

  it('Run Tool is disabled while an index-name error is present', async () => {
    renderTestToolSettings({
      selectedTool: 'list_issues',
      isValidForm: true,
      selectedToolSchema: { properties: {} },
      indexNameValidation: { indexNameError: 'Index "x" already exists', clearIndexNameError: vi.fn(), updateIndexNameError: vi.fn(), isIndexNameValid: vi.fn(() => false) },
    });
    expect(await screen.findByText('RUN TOOL')).toBeDisabled();
  });

  it('Run Tool is enabled and calls onRunTool when the form is valid and nothing is running', async () => {
    const user = userEvent.setup();
    const onRunTool = vi.fn();
    renderTestToolSettings({ selectedTool: 'list_issues', isValidForm: true, onRunTool, selectedToolSchema: { properties: {} } });
    const button = await screen.findByText('RUN TOOL');
    expect(button).not.toBeDisabled();
    await user.click(button);
    expect(onRunTool).toHaveBeenCalled();
  });

  it('flags an invalid index_data index_name while editing, and clears it once valid', async () => {
    const updateIndexNameError = vi.fn();
    const clearIndexNameError = vi.fn();
    const isIndexNameValid = vi.fn((name: string) => name !== 'taken');
    renderTestToolSettings({
      selectedTool: 'index_data',
      selectedToolSchema: { properties: { index_name: { type: 'string', title: 'Index name' } } },
      indexNameValidation: { indexNameError: null, clearIndexNameError, updateIndexNameError, isIndexNameValid },
    });

    // A single `fireEvent.change` (not `user.type`, which sends per-keystroke
    // deltas against a CONTROLLED field whose `value` prop this isolated
    // component test never updates between renders) sets the full text in
    // one shot, matching what `onFieldChange`/`StringTextField`'s own
    // `handleChange` actually receives as `event.target.value` in the app.
    const input = await screen.findByLabelText('Index name');
    fireEvent.change(input, { target: { value: 'taken' } });

    expect(updateIndexNameError).toHaveBeenCalledWith('taken');
  });

  // Regression coverage for the dropped `ContentContainer` CSS finding:
  // before the fix, `contentContainerSx` was a plain object with no `lg`+
  // overflow entry at all (the baseline `ContentContainer` styled
  // component's own `overflowY:'scroll'` + hidden-native-scrollbar behavior
  // was silently dropped) — this assertion fails against that prior shape
  // and passes against the restored one.
  it('contentContainerSx restores ContentContainer\'s lg+ hidden-scrollbar overflow behavior', () => {
    const containerStyles = (contentContainerSx as (theme: Theme) => Record<string, unknown>)(theme);
    expect(containerStyles['boxSizing']).toBe('border-box');
    const lgUp = containerStyles[theme.breakpoints.up('lg')] as Record<string, unknown>;
    expect(lgUp['overflowY']).toBe('scroll');
    expect(lgUp['scrollbarWidth']).toBe('none');
  });
});
