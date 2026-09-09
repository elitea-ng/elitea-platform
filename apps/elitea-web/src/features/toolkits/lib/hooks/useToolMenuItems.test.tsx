import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../../__tests__/testUtils';
import { useToolMenuItems } from './useToolMenuItems';
import type { UseToolMenuItemsParams, UseToolMenuItemsResult } from './useToolMenuItems';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function renderToolMenuItems(params: UseToolMenuItemsParams): { readonly box: { current: UseToolMenuItemsResult | undefined } } {
  const box: { current: UseToolMenuItemsResult | undefined } = { current: undefined };

  function ProbeComponent() {
    box.current = useToolMenuItems(params);
    return null;
  }

  function RootComponent() {
    return (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <ProbeComponent />
      </SocketClientContext.Provider>
    );
  }

  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({ component: RootComponent });
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

  return { box };
}

describe('useToolMenuItems', () => {
  it('excludes mcp-shaped and agent/application-labelled entries, and adds Custom for the non-MCP/non-application case', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          github: { metadata: { label: 'GitHub' } },
          mcp: { metadata: { label: 'MCP' } },
          hidden_tool: { metadata: { label: 'Hidden', hidden: true } },
          application: { metadata: { label: 'Agent' } },
        }),
      ),
    );

    const { box } = renderToolMenuItems({});

    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));
    const keys = box.current?.toolMenuItems.map((item) => item.key);
    expect(keys).toContain('github');
    expect(keys).toContain('custom');
    expect(keys).not.toContain('mcp');
    expect(keys).not.toContain('application');
  });

  // #865/#866: a hidden type is a REAL catalogued type the configured worker
  // cannot build — this hook backs the type PICKER, so it keeps the tile
  // (greyed by the caller) instead of making the type look like it never
  // existed, which is what happened before this fix.
  it('keeps a hidden entry, marked disabled with a reason, instead of dropping it', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          github: { metadata: { label: 'GitHub' } },
          hidden_tool: { metadata: { label: 'Hidden', hidden: true, unavailable_reason: 'the configured worker cannot build this type' } },
        }),
      ),
    );

    const { box } = renderToolMenuItems({});

    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));
    const items = box.current?.toolMenuItems ?? [];
    const hidden = items.find((item) => item.key === 'hidden_tool');
    expect(hidden).toBeDefined();
    expect(hidden?.disabled).toBe(true);
    expect(hidden?.disabledReason).toBe('the configured worker cannot build this type');
    // The click a keyboard user could still trigger on the underlying
    // control must stay inert — belt and suspenders alongside the UI's own
    // disabled control.
    hidden?.onClick();
  });

  it('falls back to a generic reason when the backend names none', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          hidden_tool: { metadata: { label: 'Hidden', hidden: true } },
        }),
      ),
    );

    const { box } = renderToolMenuItems({});

    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));
    const hidden = box.current?.toolMenuItems.find((item) => item.key === 'hidden_tool');
    expect(hidden?.disabled).toBe(true);
    expect(hidden?.disabledReason).toBeTruthy();
  });

  it('limits the MCP-mode menu to mcp-flavoured schemas plus the synthesized Remote MCP entry, excluding ordinary toolkit types (R1 regression guard)', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          github: { metadata: { label: 'GitHub' } },
          jira: { metadata: { label: 'Jira' } },
          openapi: { metadata: { label: 'OpenAPI' } },
          mcp: { metadata: { label: 'MCP' } },
          my_local_mcp: { metadata: { label: 'My Local MCP' }, type: 'mcp' },
        }),
      ),
    );

    const { box } = renderToolMenuItems({ isMCP: true });

    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));
    const keys = box.current?.toolMenuItems.map((item) => item.key);
    // Ordinary (non-mcp-shaped) toolkit types must NOT pollute the MCP menu.
    expect(keys).not.toContain('github');
    expect(keys).not.toContain('jira');
    expect(keys).not.toContain('openapi');
    // The user's own discovered MCP server, plus the synthesized "Remote MCP" entry, must be present.
    expect(keys).toContain('my_local_mcp');
    const remoteMcp = box.current?.toolMenuItems.find((item) => item.key === 'mcp');
    expect(remoteMcp?.label).toBe('Remote MCP');
  });

  it('does not add a Custom entry for isMCP', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})));

    const { box } = renderToolMenuItems({ isMCP: true });

    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));
    expect(box.current?.toolMenuItems.map((item) => item.key)).not.toContain('custom');
  });

  it('does not add a Custom entry for isApplication', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub', application: true } } })),
    );

    const { box } = renderToolMenuItems({ isApplication: true });

    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));
    const keys = box.current?.toolMenuItems.map((item) => item.key);
    expect(keys).toContain('github');
    expect(keys).not.toContain('custom');
  });

  it('carries an iconKind on every entry, including the synthesized Custom entry, instead of dropping icon information entirely (R6 regression guard)', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));

    const { box } = renderToolMenuItems({});

    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));
    const github = box.current?.toolMenuItems.find((item) => item.key === 'github');
    const custom = box.current?.toolMenuItems.find((item) => item.key === 'custom');
    expect(github).toHaveProperty('iconKind');
    expect(github?.iconKind).toBe('toolkit');
    expect(custom).toHaveProperty('iconKind');
    expect(custom?.iconKind).toBe('toolkit');
  });

  it('wires onAddTool into each entry as onClick, called with the entry key and the resolved schema map', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));

    const onAddToolInner = vi.fn();
    const onAddTool = vi.fn<NonNullable<UseToolMenuItemsParams['onAddTool']>>().mockReturnValue(onAddToolInner);
    const { box } = renderToolMenuItems({ onAddTool });

    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));
    const github = box.current?.toolMenuItems.find((item) => item.key === 'github');
    github?.onClick();

    expect(onAddTool).toHaveBeenCalledWith('github', expect.anything());
    const githubCalls = onAddTool.mock.calls.filter(([key]) => key === 'github');
    const [, schemaMapArg] = githubCalls.at(-1) ?? [];
    expect(schemaMapArg).toHaveProperty('github');
    expect(onAddToolInner).toHaveBeenCalledTimes(1);
  });
});
