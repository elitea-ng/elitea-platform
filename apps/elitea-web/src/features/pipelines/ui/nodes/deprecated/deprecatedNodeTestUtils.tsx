import type { ReactElement, ReactNode } from 'react';

import type { RenderResult } from '@testing-library/react';
import { render } from '@testing-library/react';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { FlowEditorContext, NodeConfigPanelContext, type FlowEditorContextValue } from '../../../lib/flow-editor/flowEditorContext';
import { createTestQueryClient } from '../../../__tests__/testUtils';

/**
 * Shared render harness for this sub-unit's (A2g) node component tests.
 * `ToolNode`/`LoopNode`/`LoopToolNode` all call `useSelectedProjectId()`
 * (this slice's local duplicate — see that hook's own doc comment), which
 * reads `useRouteContext()` and throws outside any `<RouterProvider>`
 * ancestor — the same real constraint the already-landed sibling
 * `ui/select/ToolSelect.test.tsx`'s own `renderWithRouterAndProject`
 * (`../../__tests__/testUtils.tsx`) documents and solves. That helper
 * cannot be reused directly here (it renders a bare component tree; these
 * tests additionally need a live `<ReactFlow>`/`<ReactFlowProvider>`
 * ancestor for `CustomHandle`'s `useNodeId`/`useEdges`/`useReactFlow`, the
 * same reason `CustomHandle.test.tsx` mounts one) — this combines both.
 *
 * Also stubs `ResizeObserver` (module-load time, once) — `<ReactFlow>`'s
 * own pane measuring calls it and jsdom does not implement it, the same
 * stub every sibling `ui/nodes/*.test.tsx` file's own `beforeAll` installs
 * individually; centralised here so every test importing this helper gets
 * it without repeating that boilerplate.
 */
if (typeof globalThis.ResizeObserver === 'undefined') {
  globalThis.ResizeObserver = class ResizeObserverStub {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  };
}

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

export function renderDeprecatedNode(
  nodeId: string,
  contextValue: FlowEditorContextValue,
  ui: ReactElement,
  projectId: string | undefined = 'project-1',
): RenderResult {
  const queryClient = createTestQueryClient();

  function TestNode(): ReactNode {
    return (
      <FlowEditorContext.Provider value={contextValue}>
        {ui}
        <NodeConfigPanelContext.Provider value={{ isEntrypoint: false, isLegacyLayout: true }}>
          {ui}
        </NodeConfigPanelContext.Provider>
      </FlowEditorContext.Provider>
    );
  }

  function RootComponent(): ReactNode {
    return (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <ReactFlowProvider>
            <ReactFlow
              nodes={[{ id: nodeId, type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
              edges={[]}
              nodeTypes={{ testNode: TestNode }}
              // Pan/zoom are d3-drag-driven and irrelevant to these
              // component tests; disabled to keep the pane's own event
              // listeners minimal. (Does NOT, by itself, make a real
              // `fireEvent.mouseDown` on a nested `Select` safe -- that
              // still arms react-flow's own pane-level `d3-drag` internals
              // regardless of this flag and throws in jsdom; tests in this
              // directory avoid opening a `Select` while mounted inside
              // this harness for that reason -- see `ToolNode.test.tsx`'s
              // own note.)
              panOnDrag={false}
              zoomOnScroll={false}
              zoomOnPinch={false}
              zoomOnDoubleClick={false}
              panOnScroll={false}
            />
          </ReactFlowProvider>
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
