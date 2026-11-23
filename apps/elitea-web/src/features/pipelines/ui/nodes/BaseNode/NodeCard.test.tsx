import type { ReactElement } from 'react';

import { render, type RenderResult } from '@testing-library/react';
import { QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { beforeAll, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { ReactFlow, ReactFlowProvider } from '@xyflow/react';

import { buildFlowEditorContextValue, createTestQueryClient } from '../../../__tests__/testUtils';
import {
  FlowEditorContext,
  NodeConfigPanelContext,
  type FlowEditorContextValue,
  type NodeConfigPanelContextValue,
} from '../../../lib/flow-editor/flowEditorContext';
import { NodeCard, type NodeCardProps } from './NodeCard';

beforeAll(() => {
  if (typeof globalThis.ResizeObserver === 'undefined') {
    globalThis.ResizeObserver = class ResizeObserverStub {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    };
  }
});

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderTree(ui: ReactElement): RenderResult {
  return render(
    <QueryClientProvider client={createTestQueryClient()}>
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        {ui}
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

function renderNodeCard(
  props: Partial<NodeCardProps>,
  flowEditorValue?: FlowEditorContextValue,
  panelContext?: NodeConfigPanelContextValue,
): RenderResult {
  const fullProps: NodeCardProps = {
    name: 'Node1',
    isEntrypoint: false,
    id: 'Node1',
    type: 'llm',
    children: <div data-testid="node-card-children">child content</div>,
    ...props,
  };

  const nodeEl = panelContext ? (
    <NodeConfigPanelContext.Provider value={panelContext}>
      <NodeCard {...fullProps} />
    </NodeConfigPanelContext.Provider>
  ) : (
    <ReactFlow
      nodes={[{ id: 'Node1', type: 'testNode', position: { x: 0, y: 0 }, data: {} }]}
      edges={[]}
      nodeTypes={{ testNode: () => <NodeCard {...fullProps} /> }}
    />
  );

  return renderTree(
    <ReactFlowProvider>
      {flowEditorValue ? <FlowEditorContext.Provider value={flowEditorValue}>{nodeEl}</FlowEditorContext.Provider> : nodeEl}
    </ReactFlowProvider>,
  );
}

describe('NodeCard (Wave U2 dual-mode)', () => {
  it('renders nothing without a FlowEditorContext.Provider ancestor', () => {
    const { container } = renderNodeCard({});
    expect(container.querySelector('[data-testid="pipeline-node-v2-card-node1"]')).not.toBeInTheDocument();
  });

  it('renders compact view-only NodeCardView on the canvas without inline form children', () => {
    const flowEditorValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', type: 'llm' }] },
    });
    const { getByTestId, queryByTestId } = renderNodeCard({}, flowEditorValue);

    expect(getByTestId('pipeline-node-v2-card-node1')).toBeInTheDocument();
    expect(getByTestId('pipeline-node-v2-name')).toHaveTextContent('Node1');
    expect(queryByTestId('node-card-children')).not.toBeInTheDocument();
  });

  it('shows Entrypoint badge on the canvas card when isEntrypoint is true', () => {
    const flowEditorValue = buildFlowEditorContextValue({
      yamlJsonObject: { entry_point: 'Node1', nodes: [{ id: 'Node1', type: 'llm' }] },
    });
    const { getByTestId } = renderNodeCard({ isEntrypoint: true }, flowEditorValue);

    expect(getByTestId('pipeline-node-v2-entrypoint')).toBeInTheDocument();
  });

  it('renders handles slot on the canvas card and exposes data-performing', () => {
    const flowEditorValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', type: 'llm' }] },
    });
    const { getByTestId } = renderNodeCard(
      {
        isPerforming: true,
        handles: () => <div data-testid="custom-handle-slot" />,
      },
      flowEditorValue,
    );

    expect(getByTestId('custom-handle-slot')).toBeInTheDocument();
    expect(getByTestId('pipeline-node-v2-card-node1')).toHaveAttribute('data-performing', 'true');
  });

  it('renders form children inside NodeConfigPanelContext (panel mode)', () => {
    const flowEditorValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', type: 'llm' }] },
    });
    const { getByTestId, queryByTestId } = renderNodeCard(
      {},
      flowEditorValue,
      { isEntrypoint: false, isLegacyLayout: false },
    );

    expect(getByTestId('node-card-children')).toBeInTheDocument();
    expect(queryByTestId('pipeline-node-v2-card-node1')).not.toBeInTheDocument();
  });

  it('wraps legacy layout children in a Basic accordion inside NodeConfigPanelContext', () => {
    const flowEditorValue = buildFlowEditorContextValue({
      yamlJsonObject: { nodes: [{ id: 'Node1', type: 'tool' }] },
    });
    const { getByTestId } = renderNodeCard(
      { type: 'tool' },
      flowEditorValue,
      { isEntrypoint: false, isLegacyLayout: true },
    );

    expect(getByTestId('pipeline-node-panel-basic-section')).toBeInTheDocument();
    expect(getByTestId('node-card-children')).toBeInTheDocument();
  });
});
