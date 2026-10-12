import { fireEvent, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { RunStateNode, type RunStateNodeData, type RunStateNodeProps } from './RunStateNode';

const yamlJsonObject: YamlPipelineDocument = {};

function renderRunStateNode(overrides: Partial<RunStateNodeProps> = {}) {
  const data: RunStateNodeData = { status: 'Completed', label: 'Run #1', ...overrides.data };
  const props: RunStateNodeProps = {
    id: 'run-1',
    data,
    deleteRunNode: vi.fn(),
    onStopRun: vi.fn(),
    yamlJsonObject,
    ...overrides,
  };
  return { ...renderWithTheme(<RunStateNode {...props} />), props };
}

describe('RunStateNode', () => {
  it('renders the run label', () => {
    renderRunStateNode();
    expect(screen.getByText('Run #1')).toBeInTheDocument();
  });

  it('shows a spinner for an in-progress run', () => {
    renderRunStateNode({ data: { status: 'In progress', label: 'Run #1' } });
    expect(screen.getByRole('progressbar')).toBeInTheDocument();
  });

  it('preserves Stop without a spinner for an actual recovery suspension', async () => {
    const onStopRun = vi.fn();
    const { container } = renderRunStateNode({ id: 'paused-run', onStopRun,
      data: { status: 'Interrupt', label: 'Paused run', recoveryPaused: true } });
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
    const stopIcon = container.querySelector('[aria-label="Stop run"] svg');
    expect(stopIcon).toBeTruthy();
    fireEvent.click(stopIcon as SVGElement);
    expect(onStopRun).toHaveBeenCalledWith('paused-run');
    fireEvent.click(screen.getByText('Paused run'));
    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    expect(screen.getByRole('button', { name: /Stop/ })).toBeInTheDocument();
  });

  it('shows no spinner for a completed run', () => {
    renderRunStateNode({ data: { status: 'Completed', label: 'Run #1' } });
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
  });

  it('shows no spinner for an errored run', () => {
    renderRunStateNode({ data: { status: 'Error', label: 'Run #1' } });
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
  });

  it('renders a fallback icon (no spinner) for an unrecognised status', () => {
    renderRunStateNode({ data: { status: 'Something else', label: 'Run #1' } });
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
  });

  it('renders without throwing for a "Stopped" status (a distinct icon-color branch, no spinner)', () => {
    renderRunStateNode({ data: { status: 'Stopped', label: 'Run #1' } });
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
    expect(screen.getByText('Run #1')).toBeInTheDocument();
  });

  it('opens the RunStateDialog when the label is clicked', async () => {
    renderRunStateNode();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();

    fireEvent.click(screen.getByText('Run #1'));

    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
  });

  // `Dialog` portals its content to `document.body`, outside RTL's own
  // `container` -- every icon-click test below therefore scopes its `svg`
  // query to `container` specifically, so a later-opened dialog's own
  // (portalled) icons can never be picked up by accident.

  it('calls onStopRun with the node id when the stop icon is clicked on an in-progress run, without opening the dialog', () => {
    const onStopRun = vi.fn();
    const { container } = renderRunStateNode({
      id: 'run-42',
      onStopRun,
      data: { status: 'In progress', label: 'Run #1' },
    });

    const stopIconWrapper = container.querySelector('[aria-label="Stop run"]');
    expect(stopIconWrapper).toBeTruthy();
    const stopIcon = stopIconWrapper?.querySelector('svg');
    fireEvent.click(stopIcon as SVGElement);

    expect(onStopRun).toHaveBeenCalledWith('run-42');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('calls deleteRunNode with the node id when the delete icon is clicked on a non-in-progress run', () => {
    const deleteRunNode = vi.fn();
    const { container } = renderRunStateNode({
      id: 'run-42',
      deleteRunNode,
      data: { status: 'Completed', label: 'Run #1' },
    });

    const deleteIconWrapper = container.querySelector('[aria-label="Delete run"]');
    expect(deleteIconWrapper).toBeTruthy();
    const deleteIcon = deleteIconWrapper?.querySelector('svg');
    fireEvent.click(deleteIcon as SVGElement);

    expect(deleteRunNode).toHaveBeenCalledWith('run-42');
  });

  it('closes an already-open dialog when the delete icon is clicked', async () => {
    const { container } = renderRunStateNode({ data: { status: 'Completed', label: 'Run #1' } });
    fireEvent.click(screen.getByText('Run #1'));
    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());

    const deleteIconWrapper = container.querySelector('[aria-label="Delete run"]');
    const deleteIcon = deleteIconWrapper?.querySelector('svg');
    fireEvent.click(deleteIcon as SVGElement);

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('renders no tooltip aria-labels when avoidTooltip is true', () => {
    const { container } = renderRunStateNode({ avoidTooltip: true });
    expect(container.querySelector('[aria-label="View details"]')).not.toBeInTheDocument();
    expect(container.querySelector('[aria-label="Delete run"]')).not.toBeInTheDocument();
  });

  it('renders selected styling without throwing when selected is true', () => {
    expect(() => renderRunStateNode({ selected: true })).not.toThrow();
  });

  it('passes editorHeight/editorWidth through to the dialog without throwing', () => {
    expect(() => renderRunStateNode({ editorHeight: 400, editorWidth: 600 })).not.toThrow();
  });
});
