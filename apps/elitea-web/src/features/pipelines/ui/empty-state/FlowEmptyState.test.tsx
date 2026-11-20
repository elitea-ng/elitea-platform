import { cleanup, fireEvent, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { FlowEmptyState } from './FlowEmptyState';

describe('FlowEmptyState', () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it('renders the empty-state message and the Add Node button', () => {
    renderWithTheme(<FlowEmptyState onAddNode={vi.fn()} />);

    expect(screen.getByTestId('pipeline-empty-state')).toBeInTheDocument();
    expect(screen.getByText('Start by adding your first node to the canvas!')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-empty-state-add-node-button')).toBeInTheDocument();
  });

  it('hides the Add Node button when disabled', () => {
    renderWithTheme(
      <FlowEmptyState
        onAddNode={vi.fn()}
        disabled
      />,
    );

    expect(screen.getByText('Start by adding your first node to the canvas!')).toBeInTheDocument();
    expect(screen.queryByTestId('pipeline-empty-state-add-node-button')).toBeNull();
  });

  it('opens the node type selector and adds the chosen node', () => {
    const onAddNode = vi.fn();
    renderWithTheme(<FlowEmptyState onAddNode={onAddNode} />);

    fireEvent.click(screen.getByTestId('pipeline-empty-state-add-node-button'));
    expect(screen.getByTestId('pipeline-node-type-selector')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('pipeline-add-node-menu-item-llm'));

    expect(onAddNode).toHaveBeenCalledWith(FlowEditorConstants.PipelineNodeTypes.LLM);
    expect(screen.queryByTestId('pipeline-node-type-selector')).not.toBeVisible();
  });
});
