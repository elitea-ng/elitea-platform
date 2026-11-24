import { cleanup, fireEvent, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { PipelineNodeDescriptions } from './nodeTypeDescriptions';
import { NodeTypeSelector, type NodeTypeSelectorProps } from './NodeTypeSelector';

const { PipelineNodeTypes, PipelineNodeDisplayNames } = FlowEditorConstants;

function renderSelector(props: Partial<NodeTypeSelectorProps> = {}) {
  const anchorEl = document.createElement('div');
  document.body.appendChild(anchorEl);

  return renderWithTheme(
    <NodeTypeSelector
      open
      anchorEl={anchorEl}
      {...props}
    />,
  );
}

describe('NodeTypeSelector', () => {
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it('renders the title and all 15 compiler-admitted node types with their descriptions', () => {
    renderSelector();

    expect(screen.getByText('Select node type')).toBeInTheDocument();
    const cards = screen.getAllByTestId(/^pipeline-add-node-menu-item-/);
    expect(cards).toHaveLength(15);
    expect(screen.getByText(PipelineNodeDisplayNames[PipelineNodeTypes.Http])).toBeInTheDocument();
    expect(screen.getByText(PipelineNodeDescriptions[PipelineNodeTypes.Http] ?? '')).toBeInTheDocument();
    expect(screen.getByText(PipelineNodeDisplayNames[PipelineNodeTypes.Hitl])).toBeInTheDocument();
    expect(screen.getByText(PipelineNodeDescriptions[PipelineNodeTypes.LLM] ?? '')).toBeInTheDocument();
    expect(screen.getByText(PipelineNodeDescriptions[PipelineNodeTypes.Aggregate] ?? '')).toBeInTheDocument();
    expect(screen.getByText(PipelineNodeDescriptions[PipelineNodeTypes.Map] ?? '')).toBeInTheDocument();
    expect(screen.getByText(PipelineNodeDescriptions[PipelineNodeTypes.Parallel] ?? '')).toBeInTheDocument();
    expect(screen.getByText(PipelineNodeDescriptions[PipelineNodeTypes.SplitOut] ?? '')).toBeInTheDocument();
  });

  it('does not render deprecated or un-admitted node types', () => {
    renderSelector();

    expect(screen.queryByTestId('pipeline-add-node-menu-item-function')).toBeNull();
    expect(screen.queryByTestId('pipeline-add-node-menu-item-custom')).toBeNull();
  });

  it('normalizes underscored node types into kebab-case testids', () => {
    renderSelector();

    expect(screen.getByTestId('pipeline-add-node-menu-item-state-modifier')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-add-node-menu-item-split-out')).toBeInTheDocument();
  });

  it('calls onSelect with the type and then onClose when a card is clicked', () => {
    const onSelect = vi.fn();
    const onClose = vi.fn();
    renderSelector({ onSelect, onClose });

    fireEvent.click(screen.getByTestId('pipeline-add-node-menu-item-router'));

    expect(onSelect).toHaveBeenCalledWith(PipelineNodeTypes.Router);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('selects a card with the keyboard (Enter and Space)', () => {
    const onSelect = vi.fn();
    renderSelector({ onSelect });

    fireEvent.keyDown(screen.getByTestId('pipeline-add-node-menu-item-code'), { key: 'Enter' });
    expect(onSelect).toHaveBeenCalledWith(PipelineNodeTypes.Code);

    fireEvent.keyDown(screen.getByTestId('pipeline-add-node-menu-item-map'), { key: ' ' });
    expect(onSelect).toHaveBeenCalledWith(PipelineNodeTypes.Map);
  });

  it('renders one column inside a narrow canvas', () => {
    const boundaryEl = document.createElement('div');
    boundaryEl.getBoundingClientRect = () => ({
      top: 0,
      left: 0,
      width: 400,
      height: 500,
      bottom: 500,
      right: 400,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    });
    renderSelector({ boundaryEl });

    expect(screen.getByTestId('pipeline-node-type-selector')).toHaveAttribute('data-columns', '1');
  });

  it('keeps the fitted layout while closing instead of jumping to full size', () => {
    const anchorEl = document.createElement('div');
    document.body.appendChild(anchorEl);
    const boundaryEl = document.createElement('div');
    boundaryEl.getBoundingClientRect = () => ({
      top: 0,
      left: 0,
      width: 400,
      height: 500,
      bottom: 500,
      right: 400,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    });

    const { rerender } = renderWithTheme(
      <NodeTypeSelector
        open
        anchorEl={anchorEl}
        boundaryEl={boundaryEl}
      />,
    );

    rerender(
      <NodeTypeSelector
        open={false}
        anchorEl={anchorEl}
        boundaryEl={boundaryEl}
      />,
    );

    expect(screen.getByTestId('pipeline-node-type-selector')).toHaveAttribute('data-columns', '1');
  });

  it('renders only the available types when they are given', () => {
    renderSelector({ availableTypes: [PipelineNodeTypes.LLM, PipelineNodeTypes.Agent] });

    const cards = screen.getAllByTestId(/^pipeline-add-node-menu-item-/);
    expect(cards.map((card) => card.dataset['testid'])).toEqual([
      'pipeline-add-node-menu-item-agent',
      'pipeline-add-node-menu-item-llm',
    ]);
  });

  it('positions the popover at the anchor position when no anchor element is given', () => {
    renderWithTheme(
      <NodeTypeSelector
        open
        anchorPosition={{ top: 100, left: 200 }}
        transformOrigin={{ vertical: 'top', horizontal: 'left' }}
      />,
    );

    const paper = screen.getByTestId('pipeline-node-type-selector');
    expect(paper.style.top).toBe('100px');
    expect(paper.style.left).toBe('200px');
  });

  it('renders two columns by default', () => {
    renderSelector();

    expect(screen.getByTestId('pipeline-node-type-selector')).toHaveAttribute('data-columns', '2');
  });

  it('closes on Escape', () => {
    const onClose = vi.fn();
    renderSelector({ onClose });

    fireEvent.keyDown(screen.getByTestId('pipeline-node-type-selector'), { key: 'Escape' });

    expect(onClose).toHaveBeenCalled();
  });
});
