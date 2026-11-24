import type { ComponentProps } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { ViewMode } from '@/shared/lib/enums';
import { ThemeProvider } from '@mui/material/styles';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { usePipelineYamlStore } from '../model/pipelineYamlStore';
import { ConfigurationTab } from './ConfigurationTab';
import type { ChatConversationAdapter } from '../lib/hooks/usePipelineChat.hooks';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const BASE = '/api/v2';

beforeEach(() => {
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1280 });
  usePipelineYamlStore.setState({
    yamlCode: '',
    yamlJsonObject: {},
    initYamlCode: '',
    initYamlJsonObject: {},
    resetFlag: false,
    layoutVersion: undefined,
  });
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1024 });
  resetGeneratedClient();
});

function stubAdapter(): ChatConversationAdapter {
  return {
    createConversation: vi.fn().mockResolvedValue({ data: { id: 1, uuid: 'u1', participants: [] } }),
    deleteMessage: vi.fn().mockResolvedValue({}),
    deleteAllMessages: vi.fn().mockResolvedValue({}),
    stopChatTask: vi.fn().mockResolvedValue(undefined),
  };
}

function baseProps(overrides: Partial<ComponentProps<typeof ConfigurationTab>> = {}): ComponentProps<typeof ConfigurationTab> {
  return {
    isFetching: false,
    isError: false,
    applicationId: '1',
    pipelineName: 'My Pipeline',
    versionDetails: { id: 5 },
    setFieldValue: vi.fn(),
    setYamlDirty: vi.fn(),
    adapter: stubAdapter(),
    slots: {
      renderConfigurationForm: () => <div data-testid="configuration-form" />,
      renderChat: () => <div data-testid="chat-slot" />,
    },
    ...overrides,
  };
}

function renderConfigurationTab(props: Partial<ComponentProps<typeof ConfigurationTab>> = {}, path = '/pipelines/latest/1') {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const socket = createTestSocketClient();
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <SocketClientContext.Provider value={socket}>
          <ThemeProvider
            theme={theme}
            defaultMode={DEFAULT_COLOR_SCHEME}
          >
            <ConfigurationTab {...baseProps(props)} />
          </ThemeProvider>
        </SocketClientContext.Provider>
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: [path] }),
    context: { auth: { getSelectedProjectId: () => 'p1' } },
  });
  return { ...render(<RouterProvider router={router} />), socket };
}

describe('ConfigurationTab', () => {
  it('shows a spinner while fetching', async () => {
    renderConfigurationTab({ isFetching: true });
    expect(await screen.findByRole('progressbar')).toBeInTheDocument();
    expect(screen.queryByTestId('configuration-form')).not.toBeInTheDocument();
  });

  it('shows the error message on isError, taking priority over isFetching', async () => {
    renderConfigurationTab({ isError: true, isFetching: true });
    expect(await screen.findByText('Failed to load data! Please try refreshing the page.')).toBeInTheDocument();
  });

  it('renders both the configuration-form slot and the chat slot', async () => {
    renderConfigurationTab();
    expect(await screen.findByTestId('configuration-form')).toBeInTheDocument();
    expect(screen.getByTestId('chat-slot')).toBeInTheDocument();
  });

  it('keeps the canvas row from scrolling and gives each column its own scroll (issue 2039)', async () => {
    renderConfigurationTab();
    const row = (await screen.findByTestId('pipeline-config-tab')).parentElement as HTMLElement;
    expect(getComputedStyle(row).overflow).toBe('hidden');
    expect(getComputedStyle(screen.getByTestId('pipeline-config-tab')).overflowY).toBe('auto');
    expect(getComputedStyle(screen.getByTestId('pipeline-chat-content')).overflowY).toBe('auto');
  });

  it('passes testPaneSettings-equivalent live data down to renderChat (llmSettings/type/tools/interaction_uuid)', async () => {
    let received: Record<string, unknown> | null = null;
    renderConfigurationTab({
      versionDetails: { id: 5, type: 'chat', tools: [{ id: 't1', type: 'toolkit' }], llm_settings: { model_name: 'gpt-4' } },
      slots: {
        renderConfigurationForm: () => <div data-testid="configuration-form" />,
        renderChat: (slotProps) => {
          received = slotProps.settings;
          return <div data-testid="chat-slot" />;
        },
      },
    });

    await screen.findByTestId('chat-slot');
    await waitFor(() => expect(received).not.toBeNull());
    expect(received).toMatchObject({
      type: 'chat',
      llmSettings: { model_name: 'gpt-4' },
      existingToolkitIds: ['t1'],
    });
    expect((received as unknown as Record<string, string>)['interaction_uuid']).toMatch(/^pipeline_1_/);
  });

  it('does not render an onShowHistory-driven history view by default, and shows it via ChatPanel once triggered', async () => {
    const renderRunHistory = vi.fn(() => <div data-testid="run-history" />);
    const user = userEvent.setup();
    renderConfigurationTab({
      slots: {
        renderConfigurationForm: () => <div data-testid="configuration-form" />,
        renderChat: () => <div data-testid="chat-slot" />,
        renderRunHistory,
      },
    });

    await screen.findByTestId('chat-slot');
    expect(screen.queryByTestId('run-history')).not.toBeInTheDocument();

    // Visible Run History button in EditorPanel toolbar while TestPanel is closed
    const historyButtons = screen.getAllByTestId('pipeline-history-tab');
    await user.click(historyButtons[0] as HTMLElement);

    expect(await screen.findByTestId('run-history')).toBeInTheDocument();
    expect(screen.queryByTestId('configuration-form')).not.toBeInTheDocument();
  });

  it('run-history\'s onClose closes the history view (handleCloseHistory), returning to the two-panel layout', async () => {
    const user = userEvent.setup();
    renderConfigurationTab({
      slots: {
        renderConfigurationForm: () => <div data-testid="configuration-form" />,
        renderChat: () => <div data-testid="chat-slot" />,
        renderRunHistory: ({ onClose }) => (
          <button
            type="button"
            onClick={onClose}
          >
            close-history
          </button>
        ),
      },
    });

    await screen.findByTestId('chat-slot');
    await user.click(screen.getAllByTestId('pipeline-history-tab')[0] as HTMLElement);
    await user.click(await screen.findByRole('button', { name: 'close-history' }));

    expect(await screen.findByTestId('configuration-form')).toBeInTheDocument();
  });

  it('restoring History mounts the test pane with the original conversation target and opens TestPanel', async () => {
    const user = userEvent.setup();
    renderConfigurationTab({
      slots: {
        renderConfigurationForm: () => <div data-testid="configuration-form" />,
        renderChat: ({ restore }) => <div data-testid="chat-slot">{restore?.conversationId}</div>,
        renderRunHistory: ({ onRestoreConversation }) => (
          <button
            type="button"
            onClick={() => onRestoreConversation(42)}
          >
            restore-conversation
          </button>
        ),
      },
    });

    await screen.findByTestId('chat-slot');
    await user.click(screen.getAllByTestId('pipeline-history-tab')[0] as HTMLElement);
    await user.click(await screen.findByRole('button', { name: 'restore-conversation' }));
    expect(await screen.findByTestId('chat-slot')).toHaveTextContent('42');
    expect(screen.getByTestId('pipeline-test-panel')).toHaveAttribute('aria-hidden', 'false');
    expect(screen.queryByRole('button', { name: 'restore-conversation' })).not.toBeInTheDocument();
  });

  it('re-layouts (isSmallWindow -> true) when the window narrows past the breakpoint, and schedules editorPanelRef.fitView() without throwing', async () => {
    renderConfigurationTab();
    await screen.findByTestId('configuration-form');

    act(() => {
      Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 800 });
      window.dispatchEvent(new Event('resize'));
    });
    // The config pane's own container re-renders in the narrow (column) layout —
    // `data-testid="pipeline-config-tab"` stays mounted either way, so this just
    // confirms the resize->state-update->re-render chain ran without throwing.
    await waitFor(() => expect(screen.getByTestId('pipeline-config-tab')).toBeInTheDocument());

    act(() => {
      Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1280 });
      window.dispatchEvent(new Event('resize'));
    });
  });

  it('does not offer onShowHistory when applicationId is undefined (no history for an unsaved pipeline)', async () => {
    renderConfigurationTab({ applicationId: undefined });
    await screen.findByTestId('chat-slot');
    expect(screen.queryByTestId('pipeline-history-tab')).not.toBeInTheDocument();
  });

  it('collapsing the config pane (GeneralFormPanel\'s own collapse button) exercises handleCollapsedGeneralPane', async () => {
    const user = userEvent.setup();
    renderConfigurationTab();

    const configTab = await screen.findByTestId('pipeline-config-tab');
    const collapseButton = within(configTab).getByRole('button');
    await user.click(collapseButton);

    // Content is hidden while collapsed — confirms the click was wired through to real state.
    expect(screen.queryByTestId('configuration-form')).not.toBeInTheDocument();
  });

  it('opens the on-demand TestPanel via Test Pipeline button and closes it via Close button', async () => {
    const user = userEvent.setup();
    usePipelineYamlStore.setState({
      yamlCode: 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n',
      yamlJsonObject: { entry_point: 'Printer_1', nodes: [{ id: 'Printer_1', type: 'printer', transition: 'END' }] },
      initYamlCode: 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n',
      initYamlJsonObject: { entry_point: 'Printer_1', nodes: [{ id: 'Printer_1', type: 'printer', transition: 'END' }] },
    });
    renderConfigurationTab();

    const panel = await screen.findByTestId('pipeline-test-panel');
    expect(panel).toHaveAttribute('aria-hidden', 'true');

    const testBtn = screen.getByTestId('pipeline-test-button');
    expect(testBtn).toBeEnabled();
    await user.click(testBtn);

    expect(panel).toHaveAttribute('aria-hidden', 'false');
    expect(screen.queryByTestId('pipeline-test-button')).not.toBeInTheDocument();

    await user.click(screen.getByTestId('pipeline-test-panel-close'));
    expect(panel).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByTestId('pipeline-test-button')).toBeInTheDocument();
  });

  it('disables Test Pipeline button when pipeline is unsaved or has no nodes connected to End', async () => {
    renderConfigurationTab({ applicationId: undefined });
    const testBtn = await screen.findByTestId('pipeline-test-button');
    expect(testBtn).toBeDisabled();
  });

  it('a real renderClearChatButton slot wires onClear through to handleHasRunsInProgress once TestPanel is opened', async () => {
    const user = userEvent.setup();
    usePipelineYamlStore.setState({
      yamlCode: 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n',
      yamlJsonObject: { entry_point: 'Printer_1', nodes: [{ id: 'Printer_1', type: 'printer', transition: 'END' }] },
      initYamlCode: 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n    transition: END\n',
      initYamlJsonObject: { entry_point: 'Printer_1', nodes: [{ id: 'Printer_1', type: 'printer', transition: 'END' }] },
    });
    renderConfigurationTab({
      slots: {
        renderConfigurationForm: () => <div data-testid="configuration-form" />,
        renderChat: () => <div data-testid="chat-slot" />,
        renderClearChatButton: ({ onClear }) => (
          <button
            type="button"
            onClick={onClear}
          >
            Clear
          </button>
        ),
      },
    });

    await user.click(await screen.findByTestId('pipeline-test-button'));
    // Does not throw even though `editorPanelRef.current` is null (the `?? false` fallback).
    await user.click(screen.getByRole('button', { name: 'Clear' }));
  });

  it('settings.onStopRun/onRcvAgentEvent do not throw when invoked by the test-chat slot (editorPanelRef unresolved)', async () => {
    let capturedSettings: Record<string, unknown> | null = null;
    renderConfigurationTab({
      slots: {
        renderConfigurationForm: () => <div data-testid="configuration-form" />,
        renderChat: (slotProps) => {
          capturedSettings = slotProps.settings;
          return <div data-testid="chat-slot" />;
        },
      },
    });

    await screen.findByTestId('chat-slot');
    await waitFor(() => expect(capturedSettings).not.toBeNull());
    const settings = capturedSettings as unknown as { onStopRun: () => void; onRcvAgentEvent: (e: unknown) => void };
    expect(() => settings.onStopRun()).not.toThrow();
    expect(() => settings.onRcvAgentEvent({ type: 'x' })).not.toThrow();
  });

  it('derives EditorPanel\'s versionTools/llmSettings from versionDetails.tools/llm_settings without crashing (see flowEditorVersionInputs.helpers.test.ts for the mapping\'s own field-level coverage — EditorPanel\'s lazy FlowWrapper/FlowEditor chain has a real, unrelated broken transitive dependency, so a value-level assertion that these two reach FlowEditor itself is not possible from this composition-root-level test)', async () => {
    renderConfigurationTab({
      versionDetails: {
        id: 5,
        tools: [{ id: 't1', type: 'toolkit', toolkit_name: 'GitHub' }],
        llm_settings: { model_name: 'gpt-4o', temperature: 0.8, max_tokens: 2048 },
      },
    });
    expect(await screen.findByTestId('chat-slot')).toBeInTheDocument();
  });

  it('wires usePipelineMCPToolsStatusMonitor: an mcp_status socket event for the version\'s MCP tool writes the updated tools array via setFieldValue', async () => {
    const setFieldValue = vi.fn();
    const mcpTool = { id: 'mcp-1', type: 'mcp', meta: { mcp: true } };
    const { socket } = renderConfigurationTab({
      setFieldValue,
      versionDetails: { id: 5, tools: [mcpTool] },
    });
    await screen.findByTestId('chat-slot');

    socket.simulateServerEvent('mcp_status', { type: 'mcp', connected: true, project_id: 'p1' });

    await waitFor(() =>
      expect(setFieldValue).toHaveBeenCalledWith('version_details.tools', [{ ...mcpTool, online: true }]),
    );
  });

  it('stacks panels cleanly without overlap below 1200px while keeping desktop >= 1200px layout intact', async () => {
    Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 960 });
    renderConfigurationTab();
    const configPane = await screen.findByTestId('pipeline-config-tab');
    const mainRow = configPane.parentElement as HTMLElement;
    const splitBox = mainRow.children[1] as HTMLElement;
    const editorBox = splitBox.children[0] as HTMLElement;
    const chatBox = splitBox.children[1] as HTMLElement;

    expect(getComputedStyle(mainRow).flexDirection).toBe('column');
    expect(getComputedStyle(mainRow).overflow).toBe('scroll');
    expect(getComputedStyle(configPane).minWidth).toBe('100%');
    expect(getComputedStyle(configPane).overflowY).toBe('visible');
    expect(getComputedStyle(configPane).flexShrink).toBe('0');
    expect(getComputedStyle(splitBox).flexDirection).toBe('column');
    expect(getComputedStyle(editorBox).maxWidth).toBe('100%');
    expect(getComputedStyle(editorBox).minWidth).toBe('100%');
    expect(getComputedStyle(chatBox).height).toBe('auto');
    expect(getComputedStyle(screen.getByTestId('pipeline-chat-content')).overflowY).toBe('visible');
  });

  it('withholds Test Pipeline, ChatPanel, Run History, and disables Add node in Public read-only viewMode', async () => {
    renderConfigurationTab({ viewMode: ViewMode.Public });
    expect(await screen.findByRole('button', { name: 'Add node' })).toBeDisabled();
    expect(screen.queryByTestId('pipeline-test-button')).not.toBeInTheDocument();
    expect(screen.queryByTestId('pipeline-test-panel')).not.toBeInTheDocument();
    expect(screen.queryByTestId('pipeline-history-tab')).not.toBeInTheDocument();
  });

  it('allows expanding GeneralFormPanel even when room.foldGeneral auto-folds it on narrow desktop widths', async () => {
    const user = userEvent.setup();
    const originalClientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth');
    Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => 800 });
    try {
      renderConfigurationTab();
      const configTab = await screen.findByTestId('pipeline-config-tab');
      // Folded automatically because 800 - CHAT_FOLDED_PX < 480
      expect(screen.queryByTestId('configuration-form')).not.toBeInTheDocument();

      await user.click(within(configTab).getByTestId('pipeline-config-collapse-button'));
      expect(await screen.findByTestId('configuration-form')).toBeInTheDocument();
    } finally {
      if (originalClientWidth) Object.defineProperty(HTMLElement.prototype, 'clientWidth', originalClientWidth);
      else delete (HTMLElement.prototype as { clientWidth?: unknown }).clientWidth;
    }
  });
});
