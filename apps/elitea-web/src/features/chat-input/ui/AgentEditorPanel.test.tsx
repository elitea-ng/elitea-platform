import type { ReactElement } from 'react';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { AgentEditorPanel } from './AgentEditorPanel';
import type { AgentEditorPanelProps } from './AgentEditorPanel';

/**
 * `AgentEditorPanel` needs a real TanStack Router root context
 * (`useSelectedProjectId`/`useEditedParticipantId`) AND a real
 * `usePermissionList` round-trip (`useCheckPermission`) — same harness
 * shape as `features/agents/ui/generate-agent-modal/GenerateAgentButton
 * .test.tsx` (no `vi.mock` of application modules, R-M1).
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderPanel(ui: ReactElement, options: { projectId?: string } = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  function RootComponent() {
    return (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          {ui}
        </ThemeProvider>
      </QueryClientProvider>
    );
  }
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => options.projectId } },
  });
  return render(<RouterProvider router={router} />);
}

const ALL_ENV_KEYS = ['VITE_SERVER_URL', 'VITE_BASE_URI', 'VITE_SOCKET_SERVER', 'VITE_SOCKET_PATH', 'VITE_PUBLIC_PROJECT_ID'] as const;
const g = globalThis as unknown as Record<string, unknown>;
const realProcessEnv = (g['process'] as { env: Record<string, string | undefined> }).env;

// jsdom has no ResizeObserver — `useAgentEditorPanelFit` creates one
// unconditionally on mount (same stub already established at
// `useAgentEditorPanelFit.hooks.test.tsx`/`features/agents/ui/
// BaseCardBody.test.tsx` for the identical situation).
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

const participant = {
  id: 'participant-1',
  entityName: 'application' as const,
  entityMeta: { id: 'agent-1', projectId: 'proj-1' },
  entitySettings: { agentType: 'chat', iconMeta: undefined },
};

const participantDetails = {
  id: 'agent-1',
  name: 'My Agent',
  versions: [
    { id: 'v1', name: 'base', status: 'draft', agentType: 'chat', createdAt: '2024-01-01' },
    { id: 'v2', name: 'v2', status: 'published', agentType: 'chat', createdAt: '2024-02-01' },
  ],
};

function baseProps(overrides: Partial<AgentEditorPanelProps> = {}): AgentEditorPanelProps {
  return {
    activeParticipant: participant,
    participantDetails,
    version: { selectedVersionId: 'v1', onSelect: vi.fn() },
    variablesEditor: { variables: [], onChange: vi.fn() },
    editorNav: {},
    ...overrides,
  };
}

// `useAgentEditorPanelFit` measures the container's grandparent
// `offsetWidth` (jsdom always reports 0), which would otherwise always
// resolve to the small (icon-only) view — stubbed wide here so the
// full-text assertions below reflect the "enough room" branch, matching
// `useAgentEditorPanelFit.hooks.test.tsx`'s own "full view" technique.
let restoreOffsetWidth: (() => void) | undefined;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  resetConfigForTests();
  for (const key of ALL_ENV_KEYS) delete realProcessEnv[key];
  vi.stubEnv('VITE_SERVER_URL', '/api/v2');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_SOCKET_SERVER', 'http://localhost');
  vi.stubEnv('VITE_SOCKET_PATH', '/socket.io');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', 'public-proj');
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);

  const originalDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetWidth');
  Object.defineProperty(HTMLElement.prototype, 'offsetWidth', { configurable: true, value: 800 });
  restoreOffsetWidth = () => {
    if (originalDescriptor) Object.defineProperty(HTMLElement.prototype, 'offsetWidth', originalDescriptor);
  };
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  resetConfigForTests();
  restoreOffsetWidth?.();
});

describe('AgentEditorPanel', () => {
  it('shows the loading skeleton while participantDetails has not resolved for this participant', async () => {
    server.use(getPermissionListMockHandler([]));
    renderPanel(
      <AgentEditorPanel {...baseProps({ participantDetails: undefined })} />,
      { projectId: 'proj-1' },
    );
    expect(await screen.findByRole('button', { name: 'Switch to model' })).toBeInTheDocument();
    expect(screen.queryByText('My Agent')).not.toBeInTheDocument();
  });

  it('renders the participant name, version selector and settings button once resolved', async () => {
    server.use(getPermissionListMockHandler([{ name: PERMISSIONS.applications.update, enabled: true }]));
    renderPanel(<AgentEditorPanel {...baseProps()} />, { projectId: 'proj-1' });

    await waitFor(() => expect(screen.getByText('My Agent')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'version selector menu: base' })).toHaveTextContent('base');
    expect(screen.getByRole('button', { name: 'agent settings menu' })).toBeInTheDocument();
  });

  it('does not render the version selector/variables editor when there are none', async () => {
    server.use(getPermissionListMockHandler([{ name: PERMISSIONS.applications.update, enabled: true }]));
    renderPanel(
      <AgentEditorPanel {...baseProps({ participantDetails: { id: 'agent-1', name: 'My Agent', versions: [] } })} />,
      { projectId: 'proj-1' },
    );
    await waitFor(() => expect(screen.getByText('My Agent')).toBeInTheDocument());
    expect(screen.queryByRole('button', { name: /^version selector menu/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'variables selector menu' })).not.toBeInTheDocument();
  });

  it('renders the variables editor when there are variables, and forwards edits', async () => {
    server.use(getPermissionListMockHandler([{ name: PERMISSIONS.applications.update, enabled: true }]));
    const onChange = vi.fn();
    renderPanel(
      <AgentEditorPanel
        {...baseProps({ variablesEditor: { variables: [{ key: 'x', value: '1' }], onChange } })}
      />,
      { projectId: 'proj-1' },
    );
    await waitFor(() => expect(screen.getByRole('button', { name: 'variables selector menu' })).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'variables selector menu' }));
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }));
    expect(onChange).toHaveBeenCalledWith([{ key: 'x', value: '1' }]);
  });

  it('shows "View agent Settings" and a disabled-looking settings affordance for a public-project participant lacking edit permission', async () => {
    server.use(getPermissionListMockHandler([]));
    renderPanel(
      <AgentEditorPanel
        {...baseProps({
          activeParticipant: { ...participant, entityMeta: { id: 'agent-1', projectId: 'public-proj' } },
        })}
      />,
      { projectId: 'public-proj' },
    );
    await waitFor(() => expect(screen.getByRole('button', { name: 'agent settings menu' })).toBeInTheDocument());
    fireEvent.mouseOver(screen.getByRole('button', { name: 'agent settings menu' }));
    expect(await screen.findByText('View agent Settings')).toBeInTheDocument();
  });

  it('calls onSwitchToModel from the close button', async () => {
    server.use(getPermissionListMockHandler([{ name: PERMISSIONS.applications.update, enabled: true }]));
    const onSwitchToModel = vi.fn();
    renderPanel(<AgentEditorPanel {...baseProps({ onSwitchToModel })} />, { projectId: 'proj-1' });
    await waitFor(() => expect(screen.getByText('My Agent')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Switch to model' }));
    expect(onSwitchToModel).toHaveBeenCalledTimes(1);
  });

  it('disables the entity-switch, version-selector, and variables-editor buttons (not just settings) via the ButtonGroup disabled cascade, matching the baseline streaming-lock behaviour', async () => {
    server.use(getPermissionListMockHandler([{ name: PERMISSIONS.applications.update, enabled: true }]));
    renderPanel(
      <AgentEditorPanel
        {...baseProps({
          disabled: true,
          variablesEditor: { variables: [{ key: 'x', value: '1' }], onChange: vi.fn() },
        })}
      />,
      { projectId: 'proj-1' },
    );
    await waitFor(() => expect(screen.getByText('My Agent')).toBeInTheDocument());
    // Disabled buttons can't fire the hover/focus events Tooltip normally
    // listens for, so MUI's Tooltip falls back to setting its title as the
    // trigger's own `aria-label` — hence "Switch Agent" (the tooltip text),
    // not "My Agent" (the button's own visible text), is this button's
    // accessible name once disabled. Same MUI mechanism the baseline relies
    // on for its own identically-structured `Tooltip`-wrapped `Button`.
    expect(screen.getByRole('button', { description: 'Switch Agent' })).toBeDisabled();
    expect(screen.getByRole('button', { name: /^version selector menu/ })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'variables selector menu' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'agent settings menu' })).toBeDisabled();
  });

  it('calls onSelect (and onCloseAgentEditor) when a version is chosen with no dirty editor guard', async () => {
    server.use(getPermissionListMockHandler([{ name: PERMISSIONS.applications.update, enabled: true }]));
    const onSelect = vi.fn();
    const onCloseAgentEditor = vi.fn();
    renderPanel(
      <AgentEditorPanel
        {...baseProps({ version: { selectedVersionId: 'v1', onSelect }, editorNav: { onCloseAgentEditor } })}
      />,
      { projectId: 'proj-1' },
    );
    await waitFor(() => expect(screen.getByRole('button', { name: /^version selector menu/ })).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /^version selector menu/ }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'v2' }));
    expect(onCloseAgentEditor).toHaveBeenCalledTimes(1);
    expect(onSelect).toHaveBeenCalledWith(participantDetails.versions[1]);
  });
});
