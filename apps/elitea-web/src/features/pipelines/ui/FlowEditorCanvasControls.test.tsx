import { fireEvent, screen } from '@testing-library/react';
import { ReactFlow, ReactFlowProvider } from '@xyflow/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createStorage } from '@/shared/lib/storage';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { FlowEditorBackground, FlowEditorCanvasControls } from './FlowEditorCanvasControls';

afterEach(() => {
  createStorage('local').remove('pipeline.flow.minimapVisible');
});

describe('FlowEditorCanvasControls', () => {
  it('renders accessible canvas control buttons and toggles lock', () => {
    const onToggleLock = vi.fn();
    renderWithTheme(
      <ReactFlowProvider>
        <ReactFlow>
          <FlowEditorBackground />
          <FlowEditorCanvasControls
            isLocked={false}
            onToggleLock={onToggleLock}
            orientation="TB"
            onArrange={vi.fn()}
          />
        </ReactFlow>
      </ReactFlowProvider>,
    );

    expect(screen.getByRole('button', { name: 'Zoom in' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Zoom out' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Fit view' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Auto-arrange' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Show minimap' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Lock canvas' }));
    expect(onToggleLock).toHaveBeenCalledTimes(1);
  });

  it('opens the TB/LR arrange menu and invokes onArrange with the selected direction', () => {
    const onArrange = vi.fn();
    renderWithTheme(
      <ReactFlowProvider>
        <ReactFlow>
          <FlowEditorCanvasControls
            isLocked={false}
            onToggleLock={vi.fn()}
            orientation="TB"
            onArrange={onArrange}
          />
        </ReactFlow>
      </ReactFlowProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Auto-arrange' }));
    fireEvent.click(screen.getByTestId('pipeline-canvas-arrange-lr'));

    expect(onArrange).toHaveBeenCalledWith('LR');
  });

  it('toggles minimap visibility and persists the preference in localStorage', () => {
    renderWithTheme(
      <ReactFlowProvider>
        <ReactFlow>
          <FlowEditorCanvasControls
            isLocked={false}
            onToggleLock={vi.fn()}
            orientation="TB"
            onArrange={vi.fn()}
          />
        </ReactFlow>
      </ReactFlowProvider>,
    );

    expect(screen.queryByTestId('rf__minimap')).not.toBeInTheDocument();

    fireEvent.click(screen.getByTestId('pipeline-canvas-minimap-toggle'));
    expect(screen.getByTestId('rf__minimap')).toBeInTheDocument();
    expect(createStorage('local').get('pipeline.flow.minimapVisible')).toBe('true');

    fireEvent.click(screen.getByTestId('pipeline-canvas-minimap-toggle'));
    expect(screen.queryByTestId('rf__minimap')).not.toBeInTheDocument();
    expect(createStorage('local').get('pipeline.flow.minimapVisible')).toBe('false');
  });
});
