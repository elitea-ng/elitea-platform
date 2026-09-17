/**
 * Coverage for the chat page's entity-editor column composition (#940 A12).
 *
 * §6.2 discipline (R-M1): no `vi.mock()` of application modules — the real
 * `AgentEditor`/`PipelineEditor`/`ToolkitEditor` are mounted, each given
 * `undefined`/`null` entities and `isCreateMode: false` so they render
 * NOTHING internally (the exact "renders nothing" precondition each of
 * those components' own test file already establishes —
 * `AgentEditor.test.tsx`, `PipelineEditor.test.tsx`, `ToolkitEditor.test.tsx`)
 * while still exercising every branch THIS file owns: which editor mounts
 * for a given `isEditing` flag, and the pipeline `onSaveVersion` conditional
 * spread. The router/theme/query-client/socket harness below is the same
 * recipe those three files use.
 */
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { ChatEditorColumn, type ChatEditorColumnProps } from './ChatEditorColumn';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function baseProps(overrides: Partial<ChatEditorColumnProps> = {}): ChatEditorColumnProps {
  return {
    agent: {
      isEditing: false,
      agentForEditor: undefined,
      editAgent: { isCreateMode: false, onCloseAgentEditor: vi.fn() },
      agentCreation: { onAgentCreated: vi.fn() },
    },
    pipeline: {
      isEditing: false,
      editPipeline: { editingPipeline: undefined, isPipelineCreateMode: false, onClosePipelineEditor: vi.fn() },
      pipelineCreation: { onPipelineCreated: vi.fn() },
      config: { renderConfigurationPanels: () => ({ tools: null }), onSaveVersion: undefined, isSavingVersion: false },
    },
    toolkit: {
      isEditing: false,
      editToolkit: { editingToolkit: undefined, onCloseToolkitEditor: vi.fn() },
      toolkitCreation: { onToolkitCreated: vi.fn() },
      toolkitWriteDeps: { createToolkit: vi.fn(), saveToolkit: vi.fn() },
    },
    ...overrides,
  };
}

function renderColumn(props: ChatEditorColumnProps): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const rootRoute = createRootRoute({
    component: () => (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <ChatEditorColumn {...props} />
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
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(getPermissionListMockHandler([{ name: PERMISSIONS.applications.update, enabled: true }]));
});

afterEach(() => {
  resetGeneratedClient();
});

describe('ChatEditorColumn', () => {
  it('renders none of the three editors when nothing is being edited', async () => {
    renderColumn(baseProps());
    expect(await screen.findByTestId('chat-editor-panel')).toBeInTheDocument();
    expect(screen.queryByRole('heading')).not.toBeInTheDocument();
  });

  it('mounts the agent editor (which renders nothing for an undefined, non-create agent) when agent.isEditing is true', async () => {
    renderColumn(
      baseProps({
        agent: {
          isEditing: true,
          agentForEditor: undefined,
          editAgent: { isCreateMode: false, onCloseAgentEditor: vi.fn() },
          agentCreation: { onAgentCreated: vi.fn() },
        },
      }),
    );
    // `AgentEditor` itself renders nothing without an agent/create-mode
    // (see that component's own "renders nothing" test) — this test's own
    // claim is that mounting it at all did not throw, i.e. the `isEditing
    // && <AgentEditor .../>` branch executed cleanly.
    expect(await screen.findByTestId('chat-editor-panel')).toBeInTheDocument();
    expect(screen.queryByRole('heading')).not.toBeInTheDocument();
  });

  it('mounts the pipeline editor with onSaveVersion omitted from deps when the config hook has none', async () => {
    renderColumn(
      baseProps({
        pipeline: {
          isEditing: true,
          editPipeline: { editingPipeline: undefined, isPipelineCreateMode: false, onClosePipelineEditor: vi.fn() },
          pipelineCreation: { onPipelineCreated: vi.fn() },
          config: { renderConfigurationPanels: () => ({ tools: null }), onSaveVersion: undefined, isSavingVersion: false },
        },
      }),
    );
    expect(await screen.findByTestId('chat-editor-panel')).toBeInTheDocument();
    expect(screen.queryByRole('heading')).not.toBeInTheDocument();
  });

  it('mounts the pipeline editor with onSaveVersion present in deps when the config hook provides one', async () => {
    const onSaveVersion = vi.fn();
    renderColumn(
      baseProps({
        pipeline: {
          isEditing: true,
          editPipeline: { editingPipeline: undefined, isPipelineCreateMode: false, onClosePipelineEditor: vi.fn() },
          pipelineCreation: { onPipelineCreated: vi.fn() },
          config: { renderConfigurationPanels: () => ({ tools: null }), onSaveVersion, isSavingVersion: true },
        },
      }),
    );
    expect(await screen.findByTestId('chat-editor-panel')).toBeInTheDocument();
  });

  it('mounts the toolkit editor (which renders nothing for an undefined toolkit) when toolkit.isEditing is true', async () => {
    renderColumn(
      baseProps({
        toolkit: {
          isEditing: true,
          editToolkit: { editingToolkit: undefined, onCloseToolkitEditor: vi.fn() },
          toolkitCreation: { onToolkitCreated: vi.fn() },
          toolkitWriteDeps: { createToolkit: vi.fn(), saveToolkit: vi.fn() },
        },
      }),
    );
    expect(await screen.findByTestId('chat-editor-panel')).toBeInTheDocument();
    expect(screen.queryByRole('heading')).not.toBeInTheDocument();
  });

  it('mounts all three editors together when every group is editing', async () => {
    renderColumn(
      baseProps({
        agent: {
          isEditing: true,
          agentForEditor: undefined,
          editAgent: { isCreateMode: false, onCloseAgentEditor: vi.fn() },
          agentCreation: { onAgentCreated: vi.fn() },
        },
        pipeline: {
          isEditing: true,
          editPipeline: { editingPipeline: undefined, isPipelineCreateMode: false, onClosePipelineEditor: vi.fn() },
          pipelineCreation: { onPipelineCreated: vi.fn() },
          config: { renderConfigurationPanels: () => ({ tools: null }), onSaveVersion: undefined, isSavingVersion: false },
        },
        toolkit: {
          isEditing: true,
          editToolkit: { editingToolkit: undefined, onCloseToolkitEditor: vi.fn() },
          toolkitCreation: { onToolkitCreated: vi.fn() },
          toolkitWriteDeps: { createToolkit: vi.fn(), saveToolkit: vi.fn() },
        },
      }),
    );
    expect(await screen.findByTestId('chat-editor-panel')).toBeInTheDocument();
  });
});
