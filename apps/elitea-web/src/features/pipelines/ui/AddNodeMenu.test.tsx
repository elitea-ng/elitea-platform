import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import * as RuntimeContractConstants from '../lib/flow-editor/constants/runtimeContract.constants';
import { AddNodeMenu } from './AddNodeMenu';

describe('AddNodeMenu', () => {
  it('opens the palette on trigger click and lists every non-deprecated node type once, alphabetically', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);

    const trigger = screen.getByRole('button', { name: 'Add node' });
    expect(trigger).toHaveTextContent('Node');
    await user.click(trigger);

    const menu = await screen.findByRole('menu');
    expect(within(menu).getByText('Select node type')).toBeInTheDocument();
    const items = within(menu).getAllByRole('menuitem');
    const labels = items.map((item) => item.getAttribute('aria-label'));

    // Deprecated/invisible node types must never appear.
    expect(labels).not.toContain('Tool');
    expect(labels).not.toContain('Function');
    expect(labels).not.toContain('Condition');
    expect(labels).not.toContain('Pipeline');
    expect(labels).not.toContain('Loop');
    expect(labels).not.toContain('Loop from tool');
    expect(labels).not.toContain('End');
    expect(labels).not.toContain('Ghost');
    expect(labels).not.toContain('Default');

    // Representative non-deprecated types must be present.
    expect(labels).toContain('Agent');
    expect(labels).toContain('LLM');

    // Sorted case-insensitively, ascending.
    const sorted = [...labels].sort((a, b) => (a ?? '').toLowerCase().localeCompare((b ?? '').toLowerCase()));
    expect(labels).toEqual(sorted);
  });

  /** Compiler code parsing is supported; execution requires an admitted sandbox (compiler.rs:1544 and :1029). Custom remains unsupported. */
  it('offers exactly the fifteen node types the pipeline editor authors, including Code, HTTP Request and Parallel and excluding Custom', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const menu = await screen.findByRole('menu');
    const labels = within(menu)
      .getAllByRole('menuitem')
      .map((item) => item.getAttribute('aria-label'));

    expect(labels).toContain('Code');
    expect(labels).toContain('HTTP Request');
    expect(labels).not.toContain('Custom');

    expect([...labels].sort((a, b) => (a ?? '').localeCompare(b ?? ''))).toEqual(
      [
        'Agent',
        'Aggregate',
        'Code',
        'Decision',
        'HTTP Request',
        'Human-in-the-loop',
        'LLM',
        'MCP',
        'Map',
        'Parallel',
        'Printer',
        'Router',
        'SplitOut',
        'State modifier',
        'Toolkit',
      ].sort((a, b) => a.localeCompare(b)),
    );
  });

  /* elitea_issues: #2661 — the node picker must render 2 columns by default, not 3. */
  it('renders the node palette in 2 columns by default', async () => {
    const user = userEvent.setup();
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const menu = await screen.findByTestId('pipeline-node-type-selector');

    expect(menu).toHaveAttribute('data-columns', '2');
  });

  it.each([
    ['Agent', 'agent'],
    ['Aggregate', 'aggregate'],
    ['Code', 'code'],
    ['Decision', 'decision'],
    ['Human-in-the-loop', 'hitl'],
    ['LLM', 'llm'],
    ['Map', 'map'],
    ['MCP', 'mcp'],
    ['Parallel', 'parallel'],
    ['Printer', 'printer'],
    ['Router', 'router'],
    ['SplitOut', 'split_out'],
    ['State modifier', 'state_modifier'],
    ['Toolkit', 'toolkit'],
  ])('the %s item dispatches its exact compiler-admitted node type', async (label, expectedType) => {
    const user = userEvent.setup();
    const onAddNode = vi.fn();
    renderWithTheme(<AddNodeMenu onAddNode={onAddNode} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const menu = await screen.findByRole('menu');
    await user.click(within(menu).getByRole('menuitem', { name: label }));

    const type = onAddNode.mock.calls[0]?.[0] as string;
    expect(onAddNode).toHaveBeenCalledTimes(1);
    expect(type).toBe(expectedType);
    expect(RuntimeContractConstants.isCompilerAdmittedNodeType(type)).toBe(true);
  });

  it('calls onAddNode with the clicked type and closes the palette', async () => {
    const user = userEvent.setup();
    const onAddNode = vi.fn();
    renderWithTheme(<AddNodeMenu onAddNode={onAddNode} />);

    await user.click(screen.getByRole('button', { name: 'Add node' }));
    const menu = await screen.findByRole('menu');
    await user.click(within(menu).getByRole('menuitem', { name: 'Agent' }));

    expect(onAddNode).toHaveBeenCalledWith('agent');
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('disables the trigger button when disabled is true', () => {
    renderWithTheme(<AddNodeMenu onAddNode={vi.fn()} disabled />);
    expect(screen.getByRole('button', { name: 'Add node' })).toBeDisabled();
  });
});
